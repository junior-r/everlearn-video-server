package moderation

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/everlearn/video-server/internal/room"
)

const (
	classRoom = "class-0199d3c2-7b1e-7c3a-9f41-2f6f5e0c1a11"
	teacher   = "0199d3c2-8000-7c3a-9f41-2f6f5e0c1a33"
	student   = "0199d3c2-8000-7c3a-9f41-2f6f5e0c1a22"
	classmate = "0199d3c2-8000-7c3a-9f41-2f6f5e0c1a44"
)

// stubs plays the token check and the video server, recording what was asked of it.
type stubs struct {
	caller room.Member
	err    error
	calls  []string
}

func (s *stubs) Authenticate(token string) (room.Member, error) {
	if token != "valid-token" {
		return room.Member{}, errors.New("bad token")
	}
	return s.caller, nil
}

func (s *stubs) SetAttribute(_ context.Context, roomName, identity, key, value string) error {
	s.calls = append(s.calls, strings.Join([]string{"attribute", roomName, identity, key, value}, " "))
	return s.err
}

func (s *stubs) MuteMicrophone(_ context.Context, roomName, identity string) error {
	s.calls = append(s.calls, "mute "+roomName+" "+identity)
	return s.err
}

func (s *stubs) Close(_ context.Context, roomName string) error {
	s.calls = append(s.calls, "close "+roomName)
	return s.err
}

func as(identity string, isTeacher bool) *stubs {
	return &stubs{caller: room.Member{Room: classRoom, Identity: identity, Teacher: isTeacher}}
}

type endpoint func(*Handler) http.HandlerFunc

var (
	hand      endpoint = func(h *Handler) http.HandlerFunc { return h.Hand }
	lowerHand endpoint = func(h *Handler) http.HandlerFunc { return h.LowerHand }
	mute      endpoint = func(h *Handler) http.HandlerFunc { return h.Mute }
	end       endpoint = func(h *Handler) http.HandlerFunc { return h.End }
)

func call(s *stubs, target endpoint, authorization, body string) int {
	handler := NewHandler(s, s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler.now = func() time.Time { return time.Date(2026, 10, 12, 8, 3, 0, 250_000_000, time.UTC) }
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response := httptest.NewRecorder()
	target(handler)(response, request)
	return response.Code
}

func TestAMemberRaisesAndLowersTheirOwnHand(t *testing.T) {
	s := as(student, false)

	// The body cannot name someone else: the hand is always the caller's
	if status := call(s, hand, "Bearer valid-token", `{"raised":true,"identity":"`+classmate+`"}`); status != http.StatusNoContent {
		t.Fatalf("raising: status = %d", status)
	}
	if status := call(s, hand, "Bearer valid-token", `{"raised":false}`); status != http.StatusNoContent {
		t.Fatalf("lowering: status = %d", status)
	}

	want := []string{
		"attribute " + classRoom + " " + student + " hand 2026-10-12T08:03:00.250Z",
		"attribute " + classRoom + " " + student + " hand ",
	}
	if strings.Join(s.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls = %q", s.calls)
	}
}

func TestTheTeacherKeepsOrder(t *testing.T) {
	target := `{"identity":"` + student + `"}`
	for name, action := range map[string]struct {
		endpoint endpoint
		body     string
		want     string
	}{
		"silences a Student": {mute, target, "mute " + classRoom + " " + student},
		"lowers a hand":      {lowerHand, target, "attribute " + classRoom + " " + student + " hand "},
		"ends the class":     {end, ``, "close " + classRoom},
	} {
		s := as(teacher, true)
		if status := call(s, action.endpoint, "Bearer valid-token", action.body); status != http.StatusNoContent {
			t.Errorf("%s: status = %d, want 204", name, status)
		}
		if len(s.calls) != 1 || s.calls[0] != action.want {
			t.Errorf("%s: calls = %q", name, s.calls)
		}
	}
}

func TestAStudentModeratesNobody(t *testing.T) {
	target := `{"identity":"` + classmate + `"}`
	for name, action := range map[string]struct {
		endpoint endpoint
		body     string
	}{
		"silencing a classmate":       {mute, target},
		"silencing the Teacher":       {mute, `{"identity":"` + teacher + `"}`},
		"lowering a classmate's hand": {lowerHand, target},
		"ending the class":            {end, ``},
	} {
		s := as(student, false)
		if status := call(s, action.endpoint, "Bearer valid-token", action.body); status != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403", name, status)
		}
		if len(s.calls) != 0 {
			t.Errorf("%s: reached the room: %q", name, s.calls)
		}
	}
}

func TestRefusesWhoeverHasNoClassToken(t *testing.T) {
	for _, target := range []endpoint{hand, lowerHand, mute, end} {
		s := as(teacher, true)
		if status := call(s, target, "", `{"raised":true}`); status != http.StatusUnauthorized {
			t.Errorf("no token: status = %d, want 401", status)
		}
		if status := call(s, target, "Bearer forged", `{"raised":true}`); status != http.StatusUnauthorized {
			t.Errorf("forged token: status = %d, want 401", status)
		}
		s.caller.Room = "lobby"
		if status := call(s, target, "Bearer valid-token", `{"raised":true}`); status != http.StatusForbidden {
			t.Errorf("another kind of room: status = %d, want 403", status)
		}
		if len(s.calls) != 0 {
			t.Errorf("reached the room: %q", s.calls)
		}
	}
}

func TestRefusesMalformedRequests(t *testing.T) {
	for name, attempt := range map[string]struct {
		endpoint endpoint
		body     string
	}{
		"a hand neither up nor down": {hand, `{}`},
		"a hand that is not json":    {hand, `up`},
		"a target that is no member": {mute, `{"identity":"everlearn-listener"}`},
		"no target":                  {lowerHand, `{}`},
	} {
		s := as(teacher, true)
		if status := call(s, attempt.endpoint, "Bearer valid-token", attempt.body); status != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, status)
		}
		if len(s.calls) != 0 {
			t.Errorf("%s: reached the room", name)
		}
	}
}

func TestTellsAGoneParticipantFromAnUnreachableRoom(t *testing.T) {
	target := `{"identity":"` + student + `"}`

	gone := as(teacher, true)
	gone.err = room.ErrNotInRoom
	if status := call(gone, mute, "Bearer valid-token", target); status != http.StatusNotFound {
		t.Errorf("participant gone: status = %d, want 404", status)
	}
	// Ending a class whose room is already gone is not a failure
	if status := call(gone, end, "Bearer valid-token", ``); status != http.StatusNoContent {
		t.Errorf("room gone: status = %d, want 204", status)
	}

	down := as(teacher, true)
	down.err = errors.New("livekit down")
	if status := call(down, mute, "Bearer valid-token", target); status != http.StatusBadGateway {
		t.Errorf("livekit down: status = %d, want 502", status)
	}
}
