package sfu

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	protologger "github.com/livekit/protocol/logger"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"

	"github.com/everlearn/video-server/internal/room"
	"github.com/everlearn/video-server/internal/transcription"
)

// ListenerIdentity names the hidden participant. It is not a Membership id, so attendance ignores it.
const ListenerIdentity = "everlearn-listener"

// A class lasts far less; the token only has to outlive one room.
const listenerTokenValidity = 12 * time.Hour

// Listener is the audio source of the transcription pipeline: it joins a room as a hidden
// participant that only subscribes to microphones, and leaves when nobody is being heard.
type Listener struct {
	client   *Client
	url      string
	interval time.Duration
	log      *slog.Logger

	mu    sync.Mutex
	rooms map[string]*heardRoom
}

// heardRoom is one room connection shared by every microphone heard in a session.
type heardRoom struct {
	connect sync.Once
	room    *lksdk.Room
	err     error

	mu sync.Mutex
	// Microphones being transcribed; the connection closes when none is left
	listeners int
	tracks    map[string]*webrtc.TrackRemote
	waiting   map[string]chan *webrtc.TrackRemote
}

// NewListener builds the audio source. url is the LiveKit address; interval is how much audio each chunk holds.
func NewListener(client *Client, url string, interval time.Duration, log *slog.Logger) *Listener {
	// The SDK narrates every ICE step on stderr; what matters is logged here
	lksdk.SetLogger(protologger.LogRLogger(logr.Discard()))
	return &Listener{client: client, url: url, interval: interval, log: log, rooms: make(map[string]*heardRoom)}
}

// Listen delivers the audio of one microphone in chunks until ctx is cancelled or the track ends.
// A room that cannot be joined closes the channel with no chunks instead of failing.
func (l *Listener) Listen(ctx context.Context, track transcription.Track) (<-chan transcription.Chunk, error) {
	chunks := make(chan transcription.Chunk)
	// Joining takes a network round trip, and the webhook that announced the track must not wait for it
	go func() {
		defer close(chunks)
		heard, err := l.enter(track.SessionID)
		if err != nil {
			l.log.Warn("microphone not transcribed", "session", track.SessionID, "track", track.SID, "error", err)
			return
		}
		defer l.leave(track.SessionID, heard, track.SID)
		arrived := heard.await(track.SID)
		select {
		case <-ctx.Done():
		case remote := <-arrived:
			codec := remote.Codec()
			l.log.Info("hearing a microphone", "session", track.SessionID, "track", track.SID, "codec", codec.MimeType)
			err := chunkAudio(ctx, remote, codec.ClockRate, codec.Channels, l.interval, time.Now, chunks)
			if err != nil && !errors.Is(err, io.EOF) && ctx.Err() == nil {
				l.log.Warn("microphone audio ended", "session", track.SessionID, "track", track.SID, "error", err)
			}
		}
	}()
	return chunks, nil
}

// enter returns the connection to a session's room, joining it if this is the first microphone heard.
func (l *Listener) enter(sessionID string) (*heardRoom, error) {
	l.mu.Lock()
	heard := l.rooms[sessionID]
	if heard == nil {
		heard = &heardRoom{tracks: make(map[string]*webrtc.TrackRemote), waiting: make(map[string]chan *webrtc.TrackRemote)}
		l.rooms[sessionID] = heard
	}
	heard.mu.Lock()
	heard.listeners++
	heard.mu.Unlock()
	l.mu.Unlock()

	// Outside the lock: joining takes a network round trip and its callbacks need the room's state
	heard.connect.Do(func() { heard.room, heard.err = l.join(sessionID, heard) })
	if heard.err != nil {
		l.leave(sessionID, heard, "")
		return nil, heard.err
	}
	return heard, nil
}

func (l *Listener) join(sessionID string, heard *heardRoom) (*lksdk.Room, error) {
	roomName := room.RoomName(sessionID)
	canSubscribe, cannot := true, false
	token, err := auth.NewAccessToken(l.client.apiKey, l.client.apiSecret).
		SetIdentity(ListenerIdentity).
		// An agent does not keep a room open once its members have left
		SetKind(livekit.ParticipantInfo_AGENT).
		SetVideoGrant(&auth.VideoGrant{
			RoomJoin:       true,
			Room:           roomName,
			Hidden:         true,
			CanSubscribe:   &canSubscribe,
			CanPublish:     &cannot,
			CanPublishData: &cannot,
		}).
		SetValidFor(listenerTokenValidity).
		ToJWT()
	if err != nil {
		return nil, fmt.Errorf("signing the listener token: %w", err)
	}
	callback := &lksdk.RoomCallback{
		ParticipantCallback: lksdk.ParticipantCallback{
			OnTrackPublished: func(publication *lksdk.RemoteTrackPublication, _ *lksdk.RemoteParticipant) {
				l.hear(sessionID, publication)
			},
			OnTrackSubscribed: func(track *webrtc.TrackRemote, publication *lksdk.RemoteTrackPublication, _ *lksdk.RemoteParticipant) {
				heard.deliver(publication.SID(), track)
			},
			OnTrackUnsubscribed: func(_ *webrtc.TrackRemote, publication *lksdk.RemoteTrackPublication, _ *lksdk.RemoteParticipant) {
				heard.forget(publication.SID())
			},
		},
		OnDisconnected: func() {
			l.log.Info("listener lost the room", "session", sessionID)
		},
	}
	// Subscribing by hand keeps cameras and screens from being sent to the listener
	joined, err := lksdk.ConnectToRoomWithToken(l.url, token, callback, lksdk.WithAutoSubscribe(false))
	if err != nil {
		return nil, fmt.Errorf("joining room %s as listener: %w", roomName, err)
	}
	l.log.Info("listener joined the room", "session", sessionID)
	for _, participant := range joined.GetRemoteParticipants() {
		for _, publication := range participant.TrackPublications() {
			if remote, ok := publication.(*lksdk.RemoteTrackPublication); ok {
				l.hear(sessionID, remote)
			}
		}
	}
	return joined, nil
}

// hear subscribes to a publication when it is a microphone.
func (l *Listener) hear(sessionID string, publication *lksdk.RemoteTrackPublication) {
	if publication.Source() != livekit.TrackSource_MICROPHONE {
		return
	}
	if err := publication.SetSubscribed(true); err != nil {
		l.log.Warn("subscribing to a microphone", "session", sessionID, "track", publication.SID(), "error", err)
	}
}

// leave stops waiting for a track and closes the room connection when it was the last one heard.
func (l *Listener) leave(sessionID string, heard *heardRoom, trackSID string) {
	l.mu.Lock()
	heard.mu.Lock()
	delete(heard.waiting, trackSID)
	heard.listeners--
	isLast := heard.listeners == 0
	heard.mu.Unlock()
	if isLast && l.rooms[sessionID] == heard {
		delete(l.rooms, sessionID)
	}
	l.mu.Unlock()
	if isLast && heard.room != nil {
		heard.room.Disconnect()
	}
}

// await returns a channel that receives the subscribed track with this SID, at once if it already arrived.
func (h *heardRoom) await(trackSID string) <-chan *webrtc.TrackRemote {
	arrived := make(chan *webrtc.TrackRemote, 1)
	h.mu.Lock()
	defer h.mu.Unlock()
	if track, subscribed := h.tracks[trackSID]; subscribed {
		arrived <- track
	} else {
		h.waiting[trackSID] = arrived
	}
	return arrived
}

func (h *heardRoom) deliver(trackSID string, track *webrtc.TrackRemote) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tracks[trackSID] = track
	if arrived, awaited := h.waiting[trackSID]; awaited {
		arrived <- track
		delete(h.waiting, trackSID)
	}
}

func (h *heardRoom) forget(trackSID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.tracks, trackSID)
}
