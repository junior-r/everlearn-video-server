// Package webhook receives LiveKit webhooks, verifies their signature and dispatches room and participant events.
package webhook

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/livekit/protocol/livekit"

	"github.com/everlearn/video-server/internal/room"
)

// LiveKit event names.
const (
	eventRoomStarted       = "room_started"
	eventRoomFinished      = "room_finished"
	eventParticipantJoined = "participant_joined"
	eventParticipantLeft   = "participant_left"
	eventTrackPublished    = "track_published"
	eventTrackUnpublished  = "track_unpublished"
	eventEgressEnded       = "egress_ended"
)

// Participant is one connection of a member to a class session.
type Participant struct {
	SessionID    string
	MembershipID string
	Name         string
	SID          string
	JoinedAt     time.Time
}

// Microphone is an audio track a member publishes.
type Microphone struct {
	SessionID   string
	SpeakerID   string
	SpeakerName string
	TrackSID    string
}

// Recording is a file Egress finished writing for a class.
type Recording struct {
	SessionID string
	EgressID  string
	// Path is where Egress wrote it, as Egress sees its disk.
	Path      string
	StartedAt time.Time
	EndedAt   time.Time
	SizeBytes int64
}

// Listener reacts to what happens in class rooms. An error makes LiveKit deliver the event again.
type Listener interface {
	RoomStarted(ctx context.Context, sessionID string, at time.Time) error
	RoomFinished(ctx context.Context, sessionID string, at time.Time) error
	ParticipantJoined(ctx context.Context, participant Participant) error
	ParticipantLeft(ctx context.Context, participant Participant, at time.Time) error
	MicrophoneOpened(ctx context.Context, microphone Microphone) error
	MicrophoneClosed(ctx context.Context, microphone Microphone) error
	RecordingFinished(ctx context.Context, recording Recording) error
}

// Receiver authenticates a webhook request and decodes its event.
type Receiver func(request *http.Request) (*livekit.WebhookEvent, error)

// Handler is the HTTP endpoint LiveKit posts its webhooks to.
type Handler struct {
	receive  Receiver
	listener Listener
	log      *slog.Logger
}

// NewHandler builds the endpoint. receive must reject requests LiveKit did not sign.
func NewHandler(receive Receiver, listener Listener, log *slog.Logger) *Handler {
	return &Handler{receive: receive, listener: listener, log: log}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	event, err := h.receive(request)
	if err != nil {
		h.log.Warn("rejected a webhook", "error", err)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if err := h.dispatch(request.Context(), event); err != nil {
		h.log.Error("handling a webhook", "event", event.GetEvent(), "error", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) dispatch(ctx context.Context, event *livekit.WebhookEvent) error {
	if event.GetEvent() == eventEgressEnded {
		return h.egressEnded(ctx, event.GetEgressInfo())
	}
	sessionID, ok := room.SessionID(event.GetRoom().GetName())
	if !ok {
		// A room this platform did not create
		return nil
	}
	at := eventTime(event)
	switch event.GetEvent() {
	case eventRoomStarted:
		return h.listener.RoomStarted(ctx, sessionID, at)
	case eventRoomFinished:
		return h.listener.RoomFinished(ctx, sessionID, at)
	}

	identity := event.GetParticipant().GetIdentity()
	if !room.IsMembership(identity) {
		return nil
	}
	switch event.GetEvent() {
	case eventParticipantJoined:
		return h.listener.ParticipantJoined(ctx, participantOf(sessionID, event, at))
	case eventParticipantLeft:
		return h.listener.ParticipantLeft(ctx, participantOf(sessionID, event, at), at)
	case eventTrackPublished, eventTrackUnpublished:
		track := event.GetTrack()
		if track.GetType() != livekit.TrackType_AUDIO || track.GetSource() != livekit.TrackSource_MICROPHONE {
			return nil
		}
		microphone := Microphone{
			SessionID:   sessionID,
			SpeakerID:   identity,
			SpeakerName: event.GetParticipant().GetName(),
			TrackSID:    track.GetSid(),
		}
		if event.GetEvent() == eventTrackPublished {
			return h.listener.MicrophoneOpened(ctx, microphone)
		}
		return h.listener.MicrophoneClosed(ctx, microphone)
	}
	return nil
}

// egressEnded passes on the file of a finished recording. One that failed before writing anything is only logged.
func (h *Handler) egressEnded(ctx context.Context, egress *livekit.EgressInfo) error {
	sessionID, ok := room.SessionID(egress.GetRoomName())
	if !ok {
		return nil
	}
	for _, file := range egress.GetFileResults() {
		if file.GetSize() <= 0 {
			continue
		}
		// A file cut short by a failure still holds the class up to that moment, so it is kept too
		return h.listener.RecordingFinished(ctx, Recording{
			SessionID: sessionID,
			EgressID:  egress.GetEgressId(),
			Path:      file.GetFilename(),
			StartedAt: time.Unix(0, file.GetStartedAt()),
			EndedAt:   time.Unix(0, file.GetEndedAt()),
			SizeBytes: file.GetSize(),
		})
	}
	h.log.Warn("a recording ended without a file", "session", sessionID, "status", egress.GetStatus().String(), "error", egress.GetError())
	return nil
}

func participantOf(sessionID string, event *livekit.WebhookEvent, at time.Time) Participant {
	participant := event.GetParticipant()
	joinedAt := at
	if seconds := participant.GetJoinedAt(); seconds > 0 {
		joinedAt = time.Unix(seconds, 0)
	}
	return Participant{
		SessionID:    sessionID,
		MembershipID: participant.GetIdentity(),
		Name:         participant.GetName(),
		SID:          participant.GetSid(),
		JoinedAt:     joinedAt,
	}
}

// eventTime is when LiveKit says the event happened, or now if it does not say.
func eventTime(event *livekit.WebhookEvent) time.Time {
	if seconds := event.GetCreatedAt(); seconds > 0 {
		return time.Unix(seconds, 0)
	}
	return time.Now()
}
