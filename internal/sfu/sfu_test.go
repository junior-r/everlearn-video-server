package sfu

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/livekit/protocol/auth"
)

const (
	apiKey    = "devkey"
	apiSecret = "devsecret_change_me_32_chars_minimum"
)

func roomToken(t *testing.T, key, secret string, grant *auth.VideoGrant, validFor time.Duration) string {
	t.Helper()
	token, err := auth.NewAccessToken(key, secret).
		SetIdentity("membership-1").
		SetName("Grace Hopper").
		SetVideoGrant(grant).
		SetValidFor(validFor).
		ToJWT()
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// backendToken signs a token the way the NestJS backend does: plain JWT claims, no LiveKit SDK.
func backendToken(t *testing.T, expiresAt time.Time) string {
	t.Helper()
	claims := jwt.MapClaims{
		"iss":   apiKey,
		"sub":   "membership-1",
		"name":  "Grace Hopper",
		"exp":   expiresAt.Unix(),
		"video": map[string]any{"room": "class-1", "roomJoin": true, "canPublishData": false},
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(apiSecret))
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestAuthenticateReadsTheTokensTheBackendSigns(t *testing.T) {
	client := NewClient("http://livekit.invalid", apiKey, apiSecret)

	sender, err := client.Authenticate(backendToken(t, time.Now().Add(time.Hour)))

	if err != nil || sender.Room != "class-1" || sender.Identity != "membership-1" || sender.Name != "Grace Hopper" {
		t.Fatalf("sender = %+v, err = %v", sender, err)
	}
}

func TestAuthenticateAcceptsOnlyRoomTokensSignedWithOurSecret(t *testing.T) {
	client := NewClient("http://livekit.invalid", apiKey, apiSecret)
	join := &auth.VideoGrant{RoomJoin: true, Room: "class-1"}

	sender, err := client.Authenticate(roomToken(t, apiKey, apiSecret, join, time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if sender.Room != "class-1" || sender.Identity != "membership-1" || sender.Name != "Grace Hopper" {
		t.Fatalf("sender = %+v", sender)
	}

	refused := map[string]string{
		"another secret":  roomToken(t, apiKey, "another_secret_that_is_32_chars_long", join, time.Minute),
		"another API key": roomToken(t, "otherkey", apiSecret, join, time.Minute),
		"expired":         backendToken(t, time.Now().Add(-time.Hour)),
		"no room":         roomToken(t, apiKey, apiSecret, &auth.VideoGrant{RoomJoin: true}, time.Minute),
		"no join grant":   roomToken(t, apiKey, apiSecret, &auth.VideoGrant{Room: "class-1"}, time.Minute),
		"not a token":     "garbage",
	}
	for name, token := range refused {
		if _, err := client.Authenticate(token); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// signedWebhook builds a request the way LiveKit does: a token whose claim is the hash of the body.
func signedWebhook(t *testing.T, secret string, body []byte) *http.Request {
	t.Helper()
	hash := sha256.Sum256(body)
	token, err := auth.NewAccessToken(apiKey, secret).
		SetValidFor(time.Minute).
		SetSha256(base64.StdEncoding.EncodeToString(hash[:])).
		ToJWT()
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/webhooks/livekit", bytes.NewReader(body))
	request.Header.Set("Authorization", token)
	request.Header.Set("Content-Type", "application/webhook+json")
	return request
}

func TestReceiveWebhookAcceptsOnlyWhatLiveKitSigned(t *testing.T) {
	client := NewClient("http://livekit.invalid", apiKey, apiSecret)
	body := []byte(`{"event":"participant_joined","room":{"name":"class-1"},"participant":{"identity":"membership-1","sid":"PA_1","joinedAt":"1000"},"createdAt":"1001"}`)

	event, err := client.ReceiveWebhook(signedWebhook(t, apiSecret, body))
	if err != nil {
		t.Fatal(err)
	}
	if event.GetEvent() != "participant_joined" || event.GetRoom().GetName() != "class-1" || event.GetParticipant().GetJoinedAt() != 1000 || event.GetCreatedAt() != 1001 {
		t.Fatalf("event = %v", event)
	}

	if _, err := client.ReceiveWebhook(signedWebhook(t, "another_secret_that_is_32_chars_long", body)); err == nil {
		t.Error("accepted a webhook signed with another secret")
	}
	tampered := signedWebhook(t, apiSecret, body)
	tampered.Body = http.NoBody
	if _, err := client.ReceiveWebhook(tampered); err == nil {
		t.Error("accepted a body that does not match its signature")
	}
	unsigned := httptest.NewRequest(http.MethodPost, "/webhooks/livekit", bytes.NewReader(body))
	if _, err := client.ReceiveWebhook(unsigned); err == nil {
		t.Error("accepted an unsigned webhook")
	}
}
