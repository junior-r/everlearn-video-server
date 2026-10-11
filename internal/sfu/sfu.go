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

	"github.com/everlearn/video-server/internal/chat"
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
func (c *Client) Authenticate(token string) (chat.Sender, error) {
	verifier, err := auth.ParseAPIToken(token)
	if err != nil {
		return chat.Sender{}, err
	}
	if verifier.APIKey() != c.apiKey {
		return chat.Sender{}, errors.New("token issued by another API key")
	}
	_, grants, err := verifier.Verify(c.apiSecret)
	if err != nil {
		return chat.Sender{}, err
	}
	if grants.Video == nil || !grants.Video.RoomJoin || grants.Video.Room == "" {
		return chat.Sender{}, errors.New("token grants no room")
	}
	return chat.Sender{Room: grants.Video.Room, Identity: grants.Identity, Name: grants.Name}, nil
}

// Publish sends a reliable data message to every participant of a room.
func (c *Client) Publish(ctx context.Context, roomName, topic string, payload []byte) error {
	token, err := auth.NewAccessToken(c.apiKey, c.apiSecret).
		SetVideoGrant(&auth.VideoGrant{RoomAdmin: true, Room: roomName}).
		SetValidFor(time.Minute).
		ToJWT()
	if err != nil {
		return fmt.Errorf("signing the server token: %w", err)
	}
	ctx, err = twirp.WithHTTPRequestHeaders(ctx, http.Header{"Authorization": {"Bearer " + token}})
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
