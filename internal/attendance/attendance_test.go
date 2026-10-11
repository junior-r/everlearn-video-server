package attendance

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/everlearn/video-server/internal/backend"
)

var start = time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)

func stay(session, member, sid string, joinedAfter time.Duration) Stay {
	return Stay{SessionID: session, MembershipID: member, ParticipantSID: sid, JoinedAt: start.Add(joinedAfter)}
}

func TestTrackerAddsUpEveryConnectionOfAMemberPerSession(t *testing.T) {
	tracker := NewTracker()

	connected, total := tracker.Leave(stay("math", "grace", "PA_1", 0), start.Add(10*time.Minute))
	if connected != 10*time.Minute || total != 10*time.Minute {
		t.Fatalf("first stay: connected %v, total %v", connected, total)
	}
	// A reconnection: a new connection of the same member
	connected, total = tracker.Leave(stay("math", "grace", "PA_2", 15*time.Minute), start.Add(40*time.Minute))
	if connected != 25*time.Minute || total != 35*time.Minute {
		t.Fatalf("second stay: connected %v, total %v", connected, total)
	}
	// Other members and other sessions are counted apart
	if _, total = tracker.Leave(stay("math", "linus", "PA_3", 0), start.Add(5*time.Minute)); total != 5*time.Minute {
		t.Fatalf("another member: total %v", total)
	}
	if _, total = tracker.Leave(stay("physics", "grace", "PA_4", 0), start.Add(time.Minute)); total != time.Minute {
		t.Fatalf("another session: total %v", total)
	}
}

func TestTrackerNeverCountsNegativeTimeAndForgetsFinishedSessions(t *testing.T) {
	tracker := NewTracker()

	if connected, total := tracker.Leave(stay("math", "grace", "PA_1", time.Minute), start); connected != 0 || total != 0 {
		t.Fatalf("leaving before joining: connected %v, total %v", connected, total)
	}
	tracker.Leave(stay("math", "grace", "PA_2", 0), start.Add(10*time.Minute))
	tracker.Leave(stay("physics", "grace", "PA_3", 0), start.Add(3*time.Minute))

	tracker.Forget("math")

	if _, total := tracker.Leave(stay("math", "grace", "PA_4", 0), start.Add(time.Minute)); total != time.Minute {
		t.Fatalf("total after forgetting the session = %v", total)
	}
	if _, total := tracker.Leave(stay("physics", "grace", "PA_5", 0), start.Add(time.Minute)); total != 4*time.Minute {
		t.Fatalf("another session's total changed: %v", total)
	}
}

type reports struct {
	sent []backend.Report
	err  error
}

func (r *reports) Enqueue(_ context.Context, report backend.Report) error {
	if r.err != nil {
		return r.err
	}
	r.sent = append(r.sent, report)
	return nil
}

func newRecorder(sink *reports) *Recorder {
	return NewRecorder(NewTracker(), sink, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestRecorderReportsArrivalsAndDepartures(t *testing.T) {
	sink := &reports{}
	recorder := newRecorder(sink)
	visit := stay("math", "grace", "PA_1", 2*time.Minute)

	if err := recorder.Joined(context.Background(), visit); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Left(context.Background(), visit, start.Add(40*time.Minute)); err != nil {
		t.Fatal(err)
	}

	if sent := sink.sent; len(sent) != 2 || !strings.HasSuffix(sent[0].Path, "/math/participants/joined") || !strings.HasSuffix(sent[1].Path, "/math/participants/left") {
		t.Fatalf("reports = %+v", sink.sent)
	}
	var left map[string]string
	if err := json.Unmarshal(sink.sent[1].Body, &left); err != nil {
		t.Fatal(err)
	}
	if left["membershipId"] != "grace" || left["joinedAt"] != "2026-10-12T08:02:00.000Z" || left["leftAt"] != "2026-10-12T08:40:00.000Z" {
		t.Fatalf("left report = %v", left)
	}
}

func TestRecorderDoesNotCountADepartureItCouldNotBuffer(t *testing.T) {
	sink := &reports{err: errors.New("redis down")}
	recorder := newRecorder(sink)
	visit := stay("math", "grace", "PA_1", 0)

	if err := recorder.Left(context.Background(), visit, start.Add(10*time.Minute)); err == nil {
		t.Fatal("want the error, so LiveKit delivers the event again")
	}
	// The redelivery must not double the time
	sink.err = nil
	if err := recorder.Left(context.Background(), visit, start.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, total := recorder.tracker.Leave(stay("math", "grace", "PA_2", 0), start); total != 10*time.Minute {
		t.Fatalf("total = %v, want 10m", total)
	}
}
