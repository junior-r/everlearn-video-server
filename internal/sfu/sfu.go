// Package sfu is the adapter to the LiveKit server: it verifies what LiveKit signs and publishes data into rooms.
package sfu

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"time"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/webhook"
	"github.com/twitchtv/twirp"

	"github.com/everlearn/video-server/internal/room"
)

// Client talks to one LiveKit server with one API key.
type Client struct {
	apiKey    string
	apiSecret string
	rooms     livekit.RoomService
	egress    livekit.Egress
}

// NewClient builds a client for the LiveKit server API at httpURL.
func NewClient(httpURL, apiKey, apiSecret string) *Client {
	return &Client{
		apiKey:    apiKey,
		apiSecret: apiSecret,
		rooms:     livekit.NewRoomServiceProtobufClient(httpURL, &http.Client{Timeout: 10 * time.Second}),
		// Starting a recording waits for Egress to open Chrome and join the room
		egress: livekit.NewEgressProtobufClient(httpURL, &http.Client{Timeout: 45 * time.Second}),
	}
}

// ReceiveWebhook checks that LiveKit signed the request with our API secret and decodes its event.
func (c *Client) ReceiveWebhook(request *http.Request) (*livekit.WebhookEvent, error) {
	return webhook.ReceiveWebhookEvent(request, auth.NewSimpleKeyProvider(c.apiKey, c.apiSecret))
}

// Authenticate verifies a room access token and returns who holds it and for which room.
func (c *Client) Authenticate(token string) (room.Member, error) {
	verifier, err := auth.ParseAPIToken(token)
	if err != nil {
		return room.Member{}, err
	}
	if verifier.APIKey() != c.apiKey {
		return room.Member{}, errors.New("token issued by another API key")
	}
	_, grants, err := verifier.Verify(c.apiSecret)
	if err != nil {
		return room.Member{}, err
	}
	if grants.Video == nil || !grants.Video.RoomJoin || grants.Video.Room == "" {
		return room.Member{}, errors.New("token grants no room")
	}
	return room.Member{
		Room:     grants.Video.Room,
		Identity: grants.Identity,
		Name:     grants.Name,
		Teacher:  grants.Attributes[room.AttributeRole] == room.RoleTeacher,
		Labs:     room.ParseLabs(grants.Attributes[room.AttributeLabs]),
		CanRecord: grants.Attributes[room.AttributeRole] == room.RoleTeacher &&
			grants.Attributes[room.AttributeRecord] == "true",
	}, nil
}

// asServer authorizes a call to the LiveKit server API with a short-lived token of our own.
func (c *Client) asServer(ctx context.Context, grant *auth.VideoGrant) (context.Context, error) {
	token, err := auth.NewAccessToken(c.apiKey, c.apiSecret).SetVideoGrant(grant).SetValidFor(time.Minute).ToJWT()
	if err != nil {
		return nil, fmt.Errorf("signing the server token: %w", err)
	}
	return twirp.WithHTTPRequestHeaders(ctx, http.Header{"Authorization": {"Bearer " + token}})
}

// gone turns LiveKit's "not found" into room.ErrNotInRoom, so callers need not know twirp.
func gone(err error) error {
	var failure twirp.Error
	if errors.As(err, &failure) && failure.Code() == twirp.NotFound {
		return room.ErrNotInRoom
	}
	return err
}

// Publish sends a reliable data message to every participant of a room.
func (c *Client) Publish(ctx context.Context, roomName, topic string, payload []byte) error {
	ctx, err := c.asServer(ctx, &auth.VideoGrant{RoomAdmin: true, Room: roomName})
	if err != nil {
		return err
	}
	_, err = c.rooms.SendData(ctx, &livekit.SendDataRequest{
		Room:  roomName,
		Data:  payload,
		Kind:  livekit.DataPacket_RELIABLE,
		Topic: &topic,
	})
	return err
}

// SetAttribute sets one attribute of a participant, seen by the whole room. An empty value removes it.
func (c *Client) SetAttribute(ctx context.Context, roomName, identity, key, value string) error {
	ctx, err := c.asServer(ctx, &auth.VideoGrant{RoomAdmin: true, Room: roomName})
	if err != nil {
		return err
	}
	_, err = c.rooms.UpdateParticipant(ctx, &livekit.UpdateParticipantRequest{
		Room:       roomName,
		Identity:   identity,
		Attributes: map[string]string{key: value},
	})
	return gone(err)
}

// isHeard tells whether a track carries sound the room hears: a microphone or a shared screen's audio.
func isHeard(source livekit.TrackSource) bool {
	return source == livekit.TrackSource_MICROPHONE || source == livekit.TrackSource_SCREEN_SHARE_AUDIO
}

// Silence mutes every open track of a participant the room hears. They may open their microphone again themselves.
func (c *Client) Silence(ctx context.Context, roomName, identity string) error {
	ctx, err := c.asServer(ctx, &auth.VideoGrant{RoomAdmin: true, Room: roomName})
	if err != nil {
		return err
	}
	participant, err := c.rooms.GetParticipant(ctx, &livekit.RoomParticipantIdentity{Room: roomName, Identity: identity})
	if err != nil {
		return gone(err)
	}
	for _, track := range participant.GetTracks() {
		if !isHeard(track.GetSource()) || track.GetMuted() {
			continue
		}
		_, err := c.rooms.MutePublishedTrack(ctx, &livekit.MuteRoomTrackRequest{
			Room:     roomName,
			Identity: identity,
			TrackSid: track.GetSid(),
			Muted:    true,
		})
		if err != nil {
			return gone(err)
		}
	}
	return nil
}

// Close ends a room for everyone in it. LiveKit then reports room_finished, which is what closes the class.
func (c *Client) Close(ctx context.Context, roomName string) error {
	ctx, err := c.asServer(ctx, &auth.VideoGrant{RoomCreate: true})
	if err != nil {
		return err
	}
	_, err = c.rooms.DeleteRoom(ctx, &livekit.DeleteRoomRequest{Room: roomName})
	return gone(err)
}

// SetMetadata replaces the metadata of a room, which LiveKit hands to everyone in it and to whoever joins later.
func (c *Client) SetMetadata(ctx context.Context, roomName, metadata string) error {
	ctx, err := c.asServer(ctx, &auth.VideoGrant{RoomAdmin: true, Room: roomName})
	if err != nil {
		return err
	}
	_, err = c.rooms.UpdateRoomMetadata(ctx, &livekit.UpdateRoomMetadataRequest{Room: roomName, Metadata: metadata})
	return gone(err)
}

// recordingPreset keeps a shared screen legible at about 3 Mbit/s, some 1.4 GB per hour of class.
const recordingPreset = livekit.EncodingOptionsPreset_H264_720P_30

// StartRecording has Egress film the whole room into one MP4 at filepath, on Egress's own disk: every
// camera and shared screen in the speaker layout, with every microphone and screen sound mixed in.
func (c *Client) StartRecording(ctx context.Context, roomName, filepath string) error {
	if path.Ext(filepath) != ".mp4" {
		return fmt.Errorf("a recording is an .mp4 file, got %q", filepath)
	}
	ctx, err := c.asServer(ctx, &auth.VideoGrant{RoomRecord: true})
	if err != nil {
		return err
	}
	_, err = c.egress.StartRoomCompositeEgress(ctx, &livekit.RoomCompositeEgressRequest{
		RoomName: roomName,
		Layout:   "speaker",
		Options:  &livekit.RoomCompositeEgressRequest_Preset{Preset: recordingPreset},
		FileOutputs: []*livekit.EncodedFileOutput{{
			FileType:        livekit.EncodedFileType_MP4,
			Filepath:        filepath,
			DisableManifest: true,
		}},
	})
	return gone(err)
}

// ActiveRecordings returns the ids of the recordings of a room still in progress.
func (c *Client) ActiveRecordings(ctx context.Context, roomName string) ([]string, error) {
	ctx, err := c.asServer(ctx, &auth.VideoGrant{RoomRecord: true})
	if err != nil {
		return nil, err
	}
	list, err := c.egress.ListEgress(ctx, &livekit.ListEgressRequest{RoomName: roomName, Active: true})
	if err != nil {
		return nil, gone(err)
	}
	ids := make([]string, 0, len(list.GetItems()))
	for _, item := range list.GetItems() {
		ids = append(ids, item.GetEgressId())
	}
	return ids, nil
}

// StopRecording ends a recording; Egress then finishes its file and LiveKit reports egress_ended.
func (c *Client) StopRecording(ctx context.Context, egressID string) error {
	ctx, err := c.asServer(ctx, &auth.VideoGrant{RoomRecord: true})
	if err != nil {
		return err
	}
	_, err = c.egress.StopEgress(ctx, &livekit.StopEgressRequest{EgressId: egressID})
	return gone(err)
}
