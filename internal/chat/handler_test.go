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
	"github.com/everlearn/video-server/internal/room"
)

const (
	session = "0199d3c2-7b1e-7c3a-9f41-2f6f5e0c1a11"
	member  = "0199d3c2-8000-7c3a-9f41-2f6f5e0c1a22"
	teacher = "0199d3c2-8000-7c3a-9f41-2f6f5e0c1a33"
	mate    = "0199d3c2-8000-7c3a-9f41-2f6f5e0c1a44"
)

// stubs plays the room, the token check and the report buffer.
type stubs struct {
	sender     room.Member
	authErr    error
	publishErr error
	enqueueErr error
	published  []string
	topics     []string
	reports    []backend.Report
}

func (s *stubs) Authenticate(token string) (room.Member, error) {
	if token != "valid-token" {
		return room.Member{}, errors.New("bad token")
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
	return &stubs{sender: room.Member{Room: "class-" + session, Identity: member, Name: "Grace Hopper"}}
}

func newHandler(s *stubs) *Handler {
	handler := NewHandler(s, s, s, []byte("signing-key"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler.now = func() time.Time { return time.Date(2026, 10, 12, 8, 3, 0, 0, time.UTC) }
	return handler
}

func call(serve http.HandlerFunc, authorization, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response := httptest.NewRecorder()
	serve(response, request)
	return response
}

func post(s *stubs, authorization, body string) *httptest.ResponseRecorder {
	return call(newHandler(s).ServeHTTP, authorization, body)
}

// sent posts a message as the stubbed sender and returns it as the room received it.
func sent(t *testing.T, s *stubs) Message {
	t.Helper()
	var message Message
	if err := json.Unmarshal(post(s, "Bearer valid-token", `{"content":"hola"}`).Body.Bytes(), &message); err != nil {
		t.Fatal(err)
	}
	return message
}

// remove asks to take a message down as the stubbed sender.
func remove(s *stubs, message Message) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"id": message.ID, "senderId": message.SenderID, "signature": message.Signature})
	return call(newHandler(s).Delete, "Bearer valid-token", string(body))
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
	for name, sender := range map[string]room.Member{
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

func TestTheAuthorAndTheTeacherTakeAMessageDown(t *testing.T) {
	for name, remover := range map[string]room.Member{
		"its author":  {Room: "class-" + session, Identity: member},
		"the Teacher": {Room: "class-" + session, Identity: teacher, Teacher: true},
	} {
		s := newStubs()
		message := sent(t, s)
		s.sender = remover

		if response := remove(s, message); response.Code != http.StatusNoContent {
			t.Fatalf("%s: status = %d, want 204", name, response.Code)
		}
		if len(s.published) != 2 || s.topics[1] != "chat-deleted" || s.published[1] != "class-"+session+` {"id":"`+message.ID+`"}` {
			t.Fatalf("%s: published %q on %q", name, s.published, s.topics)
		}
		var stored map[string]string
		if err := json.Unmarshal(s.reports[1].Body, &stored); err != nil {
			t.Fatal(err)
		}
		if s.reports[1].Path != "/internal/classroom/sessions/"+session+"/chat-messages/deleted" ||
			stored["id"] != message.ID || stored["deletedById"] != remover.Identity || stored["deletedAt"] != "2026-10-12T08:03:00.000Z" {
			t.Fatalf("%s: report = %s %v", name, s.reports[1].Path, stored)
		}
	}
}

func TestNobodyElseTakesAMessageDown(t *testing.T) {
	const otherSession = "0199d3c2-7b1e-7c3a-9f41-2f6f5e0c1a99"
	here := "class-" + session
	refused := map[string]struct {
		remover room.Member
		tamper  func(*Message)
		status  int
	}{
		"a classmate":                  {remover: room.Member{Room: here, Identity: mate}, status: http.StatusForbidden},
		"a classmate posing as author": {remover: room.Member{Room: here, Identity: mate}, tamper: func(m *Message) { m.SenderID = mate }, status: http.StatusNotFound},
		"the Teacher of another class": {remover: room.Member{Room: "class-" + otherSession, Identity: teacher, Teacher: true}, status: http.StatusNotFound},
		"a forged signature":           {remover: room.Member{Room: here, Identity: member}, tamper: func(m *Message) { m.Signature = "00" }, status: http.StatusNotFound},
		"an id that is no message":     {remover: room.Member{Room: here, Identity: member}, tamper: func(m *Message) { m.ID = "nope" }, status: http.StatusBadRequest},
	}
	for name, attempt := range refused {
		s := newStubs()
		message := sent(t, s)
		s.sender = attempt.remover
		if attempt.tamper != nil {
			attempt.tamper(&message)
		}
		if response := remove(s, message); response.Code != attempt.status {
			t.Errorf("%s: status = %d, want %d", name, response.Code, attempt.status)
		}
		if len(s.published) != 1 || len(s.reports) != 1 {
			t.Errorf("%s: the message was taken down", name)
		}
	}
}
