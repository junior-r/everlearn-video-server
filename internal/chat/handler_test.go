package chat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/everlearn/video-server/internal/backend"
)

const (
	session = "0199d3c2-7b1e-7c3a-9f41-2f6f5e0c1a11"
	member  = "0199d3c2-8000-7c3a-9f41-2f6f5e0c1a22"
)

// stubs plays the room, the token check and the report buffer.
type stubs struct {
	sender     Sender
	authErr    error
	publishErr error
	enqueueErr error
	published  []string
	topics     []string
	reports    []backend.Report
}

func (s *stubs) Authenticate(token string) (Sender, error) {
	if token != "valid-token" {
		return Sender{}, errors.New("bad token")
	}
	return s.sender, s.authErr
}

func (s *stubs) Publish(_ context.Context, roomName, topic string, payload []byte) error {
	if s.publishErr != nil {
		return s.publishErr
	}
	s.published = append(s.published, roomName+" "+string(payload))
	s.topics = append(s.topics, topic)
	return nil
}

func (s *stubs) Enqueue(_ context.Context, report backend.Report) error {
	if s.enqueueErr != nil {
		return s.enqueueErr
	}
	s.reports = append(s.reports, report)
	return nil
}

func newStubs() *stubs {
	return &stubs{sender: Sender{Room: "class-" + session, Identity: member, Name: "Grace Hopper"}}
}

func post(s *stubs, authorization, body string) *httptest.ResponseRecorder {
	handler := NewHandler(s, s, s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler.now = func() time.Time { return time.Date(2026, 10, 12, 8, 3, 0, 0, time.UTC) }
	request := httptest.NewRequest(http.MethodPost, "/chat", strings.NewReader(body))
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestShowsTheMessageToTheRoomAndCapturesIt(t *testing.T) {
	s := newStubs()

	response := post(s, "Bearer valid-token", `{"content":"  ¿Puede repetir?  ","senderId":"someone-else"}`)

	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d", response.Code)
	}
	var message Message
	if err := json.Unmarshal(response.Body.Bytes(), &message); err != nil {
		t.Fatal(err)
	}
	// The sender comes from the token, whatever the body claims
	if message.SenderID != member || message.SenderName != "Grace Hopper" || message.Content != "¿Puede repetir?" {
		t.Fatalf("message = %+v", message)
	}
	if len(s.published) != 1 || s.topics[0] != "chat" || s.published[0] != "class-"+session+" "+response.Body.String() {
		t.Fatalf("published %q on %q", s.published, s.topics)
	}
	if len(s.reports) != 1 || s.reports[0].Path != "/internal/classroom/sessions/"+session+"/chat-messages" {
		t.Fatalf("reports = %+v", s.reports)
	}
	var stored map[string]string
	if err := json.Unmarshal(s.reports[0].Body, &stored); err != nil {
		t.Fatal(err)
	}
	if stored["id"] != message.ID || stored["senderId"] != member || stored["content"] != "¿Puede repetir?" || stored["sentAt"] != "2026-10-12T08:03:00.000Z" {
		t.Fatalf("stored = %v", stored)
	}
}

func TestRefusesWhoeverHasNoValidRoomToken(t *testing.T) {
	for name, authorization := range map[string]string{
		"no header":    "",
		"not a bearer": "Basic valid-token",
		"bad token":    "Bearer forged",
	} {
		s := newStubs()
		if response := post(s, authorization, `{"content":"hola"}`); response.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", name, response.Code)
		}
		if len(s.published)+len(s.reports) != 0 {
			t.Errorf("%s: a message got through", name)
		}
	}
}

func TestRefusesTokensForOtherRoomsOrNonMembers(t *testing.T) {
	for name, sender := range map[string]Sender{
		"a room that is not a class":    {Room: "lobby", Identity: member},
		"an identity that is no member": {Room: "class-" + session, Identity: "recorder-bot"},
	} {
		s := newStubs()
		s.sender = sender
		if response := post(s, "Bearer valid-token", `{"content":"hola"}`); response.Code != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403", name, response.Code)
		}
		if len(s.published)+len(s.reports) != 0 {
			t.Errorf("%s: a message got through", name)
		}
	}
}

func TestRefusesEmptyOversizedAndMalformedMessages(t *testing.T) {
	bodies := map[string]string{
		"blank":      `{"content":"   "}`,
		"missing":    `{}`,
		"not json":   `hola`,
		"too long":   `{"content":"` + strings.Repeat("ñ", 2001) + `"}`,
		"huge body":  `{"content":"` + strings.Repeat("a", 20<<10) + `"}`,
		"wrong type": `{"content":42}`,
	}
	for name, body := range bodies {
		s := newStubs()
		if response := post(s, "Bearer valid-token", body); response.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, response.Code)
		}
		if len(s.published) != 0 {
			t.Errorf("%s: published", name)
		}
	}
	// The limit counts characters, not bytes
	s := newStubs()
	if response := post(s, "Bearer valid-token", `{"content":"`+strings.Repeat("ñ", 2000)+`"}`); response.Code != http.StatusAccepted {
		t.Errorf("2000 characters: status = %d, want 202", response.Code)
	}
}

func TestStoresNothingWhenTheRoomCannotBeReachedButKeepsAShownMessage(t *testing.T) {
	unreachable := newStubs()
	unreachable.publishErr = errors.New("livekit down")
	if response := post(unreachable, "Bearer valid-token", `{"content":"hola"}`); response.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", response.Code)
	}
	if len(unreachable.reports) != 0 {
		t.Error("a message nobody saw was stored")
	}

	unbuffered := newStubs()
	unbuffered.enqueueErr = errors.New("redis down")
	if response := post(unbuffered, "Bearer valid-token", `{"content":"hola"}`); response.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202: the room already saw the message", response.Code)
	}
}
