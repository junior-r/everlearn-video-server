// Package sfu is the adapter to the LiveKit server: it verifies what LiveKit signs and publishes data into rooms.
package sfu

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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
}

// NewClient builds a client for the LiveKit server API at httpURL.
func NewClient(httpURL, apiKey, apiSecret string) *Client {
	return &Client{
		apiKey:    apiKey,
		apiSecret: apiSecret,
		rooms:     livekit.NewRoomServiceProtobufClient(httpURL, &http.Client{Timeout: 10 * time.Second}),
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

// MuteMicrophone silences a participant's open microphone. They may open it again themselves.
func (c *Client) MuteMicrophone(ctx context.Context, roomName, identity string) error {
	ctx, err := c.asServer(ctx, &auth.VideoGrant{RoomAdmin: true, Room: roomName})
	if err != nil {
		return err
	}
	participant, err := c.rooms.GetParticipant(ctx, &livekit.RoomParticipantIdentity{Room: roomName, Identity: identity})
	if err != nil {
		return gone(err)
	}
	for _, track := range participant.GetTracks() {
		if track.GetSource() != livekit.TrackSource_MICROPHONE || track.GetMuted() {
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
