package lab

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

	"github.com/everlearn/video-server/internal/room"
)

const (
	classRoom = "class-0199d3c2-7b1e-7c3a-9f41-2f6f5e0c1a11"
	teacher   = "0199d3c2-8000-7c3a-9f41-2f6f5e0c1a33"
	student   = "0199d3c2-8000-7c3a-9f41-2f6f5e0c1a22"
)

// stubs plays the token check and the video server, recording the metadata written.
type stubs struct {
	caller   room.Member
	err      error
	metadata []string
}

func (s *stubs) Authenticate(token string) (room.Member, error) {
	if token != "valid-token" {
		return room.Member{}, errors.New("bad token")
	}
	return s.caller, nil
}

func (s *stubs) SetMetadata(_ context.Context, roomName, metadata string) error {
	if roomName != classRoom {
		return errors.New("wrong room")
	}
	s.metadata = append(s.metadata, metadata)
	return s.err
}

func as(identity string, isTeacher bool, labs ...string) *stubs {
	return &stubs{caller: room.Member{Room: classRoom, Identity: identity, Teacher: isTeacher, Labs: labs}}
}

func call(s *stubs, target func(*Handler) http.HandlerFunc, authorization, body string) int {
	handler := NewHandler(s, s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler.now = func() time.Time { return time.Date(2026, 10, 14, 9, 30, 0, 0, time.UTC) }
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response := httptest.NewRecorder()
	target(handler)(response, request)
	return response.Code
}

var (
	share = func(h *Handler) http.HandlerFunc { return h.Share }
	stop  = func(h *Handler) http.HandlerFunc { return h.Stop }
)

const bearer = "Bearer valid-token"

func TestTheTeacherSharesALabTheirTokenLists(t *testing.T) {
	s := as(teacher, true, "gravity", "function-grapher")

	code := call(s, share, bearer, `{"labKey":"function-grapher","state":{"expression":"x^2","zoom":2}}`)

	if code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", code)
	}
	if len(s.metadata) != 1 {
		t.Fatalf("metadata written %d times, want 1", len(s.metadata))
	}
	var written Metadata
	if err := json.Unmarshal([]byte(s.metadata[0]), &written); err != nil {
		t.Fatalf("metadata is not JSON: %v", err)
	}
	if written.Lab == nil || written.Lab.LabKey != "function-grapher" || written.Lab.SharedBy != teacher {
		t.Fatalf("lab = %+v", written.Lab)
	}
	if string(written.Lab.State) != `{"expression":"x^2","zoom":2}` || written.Lab.UpdatedAt != "2026-10-14T09:30:00Z" {
		t.Fatalf("state = %s at %s", written.Lab.State, written.Lab.UpdatedAt)
	}
}

func TestOnlyTheTeacherSharesAndOnlyTheLabsTheBackendSigned(t *testing.T) {
	cases := map[string]struct {
		stubs *stubs
		body  string
		want  int
	}{
		"a Student":               {as(student, false, "gravity"), `{"labKey":"gravity","state":{}}`, http.StatusForbidden},
		"a lab not in the token":  {as(teacher, true, "gravity"), `{"labKey":"forces","state":{}}`, http.StatusForbidden},
		"a Teacher with no labs":  {as(teacher, true), `{"labKey":"gravity","state":{}}`, http.StatusForbidden},
		"a state that is a list":  {as(teacher, true, "gravity"), `{"labKey":"gravity","state":[1,2]}`, http.StatusBadRequest},
		"no state":                {as(teacher, true, "gravity"), `{"labKey":"gravity"}`, http.StatusBadRequest},
		"a body that is not JSON": {as(teacher, true, "gravity"), `labKey=gravity`, http.StatusBadRequest},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if code := call(tc.stubs, share, bearer, tc.body); code != tc.want {
				t.Fatalf("status = %d, want %d", code, tc.want)
			}
			if len(tc.stubs.metadata) != 0 {
				t.Fatalf("metadata was written: %v", tc.stubs.metadata)
			}
		})
	}
}

func TestAStateTooLargeIsRefused(t *testing.T) {
	s := as(teacher, true, "completions")
	big := `{"labKey":"completions","state":{"text":"` + strings.Repeat("a", maxStateBytes) + `"}}`

	if code := call(s, share, bearer, big); code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
}

func TestATokenIsRequired(t *testing.T) {
	s := as(teacher, true, "gravity")

	if code := call(s, share, "", `{"labKey":"gravity","state":{}}`); code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", code)
	}
	if code := call(s, stop, "Bearer forged", ``); code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", code)
	}
}

func TestTheTeacherStopsSharing(t *testing.T) {
	s := as(teacher, true, "gravity")

	if code := call(s, stop, bearer, ``); code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", code)
	}
	if len(s.metadata) != 1 || s.metadata[0] != `{"lab":null}` {
		t.Fatalf("metadata = %v, want the lab cleared", s.metadata)
	}
	if code := call(as(student, false), stop, bearer, ``); code != http.StatusForbidden {
		t.Fatalf("a Student stopping: status = %d, want 403", code)
	}
}

func TestARoomAlreadyClosedIsNotFoundAndAFailureIsABadGateway(t *testing.T) {
	gone := as(teacher, true, "gravity")
	gone.err = room.ErrNotInRoom
	if code := call(gone, share, bearer, `{"labKey":"gravity","state":{}}`); code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
	failing := as(teacher, true, "gravity")
	failing.err = errors.New("livekit down")
	if code := call(failing, stop, bearer, ``); code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", code)
	}
}
