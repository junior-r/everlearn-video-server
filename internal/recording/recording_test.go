package recording

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/everlearn/video-server/internal/backend"
	"github.com/everlearn/video-server/internal/room"
)

const (
	session   = "0199d3c2-7b1e-7c3a-9f41-2f6f5e0c1a11"
	classRoom = "class-" + session
	teacher   = "0199d3c2-8000-7c3a-9f41-2f6f5e0c1a33"
)

// stubs plays the token check and Egress, recording what was asked of it.
type stubs struct {
	caller   room.Member
	active   []string
	listErr  error
	startErr error
	stopErr  error
	calls    []string
}

func (s *stubs) Authenticate(token string) (room.Member, error) {
	if token != "valid-token" {
		return room.Member{}, errors.New("bad token")
	}
	return s.caller, nil
}

func (s *stubs) StartRecording(_ context.Context, roomName, filepath string) error {
	s.calls = append(s.calls, "start "+roomName+" "+filepath)
	return s.startErr
}

func (s *stubs) ActiveRecordings(context.Context, string) ([]string, error) {
	return s.active, s.listErr
}

func (s *stubs) StopRecording(_ context.Context, egressID string) error {
	s.calls = append(s.calls, "stop "+egressID)
	return s.stopErr
}

func as(isTeacher, canRecord bool) *stubs {
	return &stubs{caller: room.Member{Room: classRoom, Identity: teacher, Teacher: isTeacher, CanRecord: canRecord}}
}

func call(s *stubs, endpoint func(*Handler) http.HandlerFunc, token string) int {
	handler := NewHandler(s, s, "/recordings", slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler.newID = func() string { return "0199d3c2-9000-7c3a-9f41-2f6f5e0c1a99" }
	request := httptest.NewRequest(http.MethodPost, "/recording", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	endpoint(handler)(response, request)
	return response.Code
}

func start(h *Handler) http.HandlerFunc { return h.Start }
func stop(h *Handler) http.HandlerFunc  { return h.Stop }

func TestTheTeacherStartsRecordingIntoAFileNamedHere(t *testing.T) {
	s := as(true, true)
	if code := call(s, start, "valid-token"); code != http.StatusNoContent {
		t.Fatalf("status = %d", code)
	}
	want := "start " + classRoom + " /recordings/" + session + "/0199d3c2-9000-7c3a-9f41-2f6f5e0c1a99.mp4"
	if len(s.calls) != 1 || s.calls[0] != want {
		t.Fatalf("calls = %v", s.calls)
	}
}

func TestAClassAlreadyBeingRecordedIsNotRecordedTwice(t *testing.T) {
	s := as(true, true)
	s.active = []string{"EG_1"}
	if code := call(s, start, "valid-token"); code != http.StatusNoContent || len(s.calls) != 0 {
		t.Fatalf("status = %d, calls = %v", code, s.calls)
	}
}

func TestOnlyATeacherTheBackendLetRecordMayRecord(t *testing.T) {
	cases := map[string]struct {
		stubs *stubs
		token string
		want  int
	}{
		"no token":                  {as(true, true), "", http.StatusUnauthorized},
		"a Student":                 {as(false, false), "valid-token", http.StatusForbidden},
		"a Teacher without consent": {as(true, false), "valid-token", http.StatusForbidden},
	}
	for name, c := range cases {
		for _, endpoint := range []func(*Handler) http.HandlerFunc{start, stop} {
			if code := call(c.stubs, endpoint, c.token); code != c.want || len(c.stubs.calls) != 0 {
				t.Errorf("%s: status = %d, calls = %v", name, code, c.stubs.calls)
			}
		}
	}
}

func TestStoppingEndsEveryRecordingInProgress(t *testing.T) {
	s := as(true, true)
	s.active = []string{"EG_1", "EG_2"}
	if code := call(s, stop, "valid-token"); code != http.StatusNoContent {
		t.Fatalf("status = %d", code)
	}
	if len(s.calls) != 2 || s.calls[0] != "stop EG_1" || s.calls[1] != "stop EG_2" {
		t.Fatalf("calls = %v", s.calls)
	}
}

func TestARecordingThatAlreadyEndedCountsAsStopped(t *testing.T) {
	s := as(true, true)
	s.active = []string{"EG_1"}
	s.stopErr = room.ErrNotInRoom
	if code := call(s, stop, "valid-token"); code != http.StatusNoContent {
		t.Fatalf("status = %d", code)
	}
}

func TestEgressFailuresAreReported(t *testing.T) {
	s := as(true, true)
	s.startErr = errors.New("no egress available")
	if code := call(s, start, "valid-token"); code != http.StatusBadGateway {
		t.Fatalf("status = %d", code)
	}
	gone := as(true, true)
	gone.listErr = room.ErrNotInRoom
	if code := call(gone, start, "valid-token"); code != http.StatusNotFound {
		t.Fatalf("status = %d", code)
	}
}

type reports struct{ items []backend.Report }

func (r *reports) Enqueue(_ context.Context, report backend.Report) error {
	r.items = append(r.items, report)
	return nil
}

func TestAFinishedFileIsReportedRelativeToTheSpool(t *testing.T) {
	sink := &reports{}
	archive := NewArchive(sink, "/recordings/", slog.New(slog.NewTextHandler(io.Discard, nil)))
	started := time.Date(2026, 10, 28, 15, 0, 0, 0, time.UTC)
	err := archive.Finished(context.Background(), File{
		SessionID: session,
		EgressID:  "EG_1",
		Path:      "/recordings/" + session + "/a.mp4",
		StartedAt: started,
		EndedAt:   started.Add(time.Hour),
		SizeBytes: 1_400_000_000,
	})
	if err != nil || len(sink.items) != 1 {
		t.Fatalf("err = %v, reports = %v", err, sink.items)
	}
	if sink.items[0].Path != "/internal/classroom/sessions/"+session+"/recordings" {
		t.Fatalf("path = %s", sink.items[0].Path)
	}
	var body map[string]any
	if err := json.Unmarshal(sink.items[0].Body, &body); err != nil {
		t.Fatal(err)
	}
	if body["file"] != session+"/a.mp4" || body["egressId"] != "EG_1" || body["sizeBytes"] != 1.4e9 ||
		body["startedAt"] != "2026-10-28T15:00:00.000Z" || body["endedAt"] != "2026-10-28T16:00:00.000Z" {
		t.Fatalf("body = %v", body)
	}
}

func TestAFileOutsideTheSpoolIsIgnored(t *testing.T) {
	sink := &reports{}
	archive := NewArchive(sink, "/recordings", slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, outside := range []string{"/elsewhere/a.mp4", "/recordings/../etc/a.mp4", "/recordings"} {
		if err := archive.Finished(context.Background(), File{SessionID: session, Path: outside}); err != nil {
			t.Fatal(err)
		}
	}
	if len(sink.items) != 0 {
		t.Fatalf("reports = %v", sink.items)
	}
}
