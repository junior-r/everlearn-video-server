package backend

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

const sessionID = "0199d3c2-7b1e-7c3a-9f41-2f6f5e0c1a11"

func TestReportsAddressTheSessionAndUseMillisecondUTCTimes(t *testing.T) {
	joined := time.Date(2026, 10, 12, 10, 2, 0, 123_456_789, time.FixedZone("VET", -4*3600))
	left := joined.Add(38 * time.Minute)

	report := ParticipantLeft(sessionID, "member", "PA_1", joined, left)

	if want := "/internal/classroom/sessions/" + sessionID + "/participants/left"; report.Path != want {
		t.Fatalf("path = %q, want %q", report.Path, want)
	}
	var body map[string]string
	if err := json.Unmarshal(report.Body, &body); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"membershipId":   "member",
		"participantSid": "PA_1",
		"joinedAt":       "2026-10-12T14:02:00.123Z",
		"leftAt":         "2026-10-12T14:40:00.123Z",
	}
	for key, value := range want {
		if body[key] != value {
			t.Errorf("%s = %q, want %q", key, body[key], value)
		}
	}
}

func TestClientSendsTheSecretAndTellsRejectionFromFailure(t *testing.T) {
	status := http.StatusNoContent
	var gotSecret, gotPath, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotSecret, gotPath, gotBody = r.Header.Get("X-Internal-Secret"), r.URL.Path, string(raw)
		w.WriteHeader(status)
	}))
	defer server.Close()
	client := NewClient(server.URL, "shared-secret")
	report := RoomStarted(sessionID, time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC))

	if err := client.Send(context.Background(), report); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotSecret != "shared-secret" || gotPath != report.Path || gotBody != `{"startedAt":"2026-10-12T08:00:00.000Z"}` {
		t.Fatalf("request was %q %q %q", gotSecret, gotPath, gotBody)
	}

	for _, rejected := range []int{http.StatusBadRequest, http.StatusNotFound, http.StatusConflict} {
		status = rejected
		if err := client.Send(context.Background(), report); !errors.Is(err, ErrRejected) {
			t.Errorf("status %d: err = %v, want ErrRejected", rejected, err)
		}
	}
	for _, retryable := range []int{http.StatusUnauthorized, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		status = retryable
		if err := client.Send(context.Background(), report); err == nil || errors.Is(err, ErrRejected) {
			t.Errorf("status %d: err = %v, want a retryable error", retryable, err)
		}
	}
	server.Close()
	if err := client.Send(context.Background(), report); err == nil || errors.Is(err, ErrRejected) {
		t.Errorf("unreachable backend: err = %v, want a retryable error", err)
	}
}

// memoryQueue behaves like RedisQueue within one process.
type memoryQueue struct {
	mu       sync.Mutex
	pending  [][]byte
	inFlight [][]byte
}

func (q *memoryQueue) Push(_ context.Context, item []byte) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pending = append(q.pending, item)
	return nil
}

func (q *memoryQueue) Claim(context.Context, time.Duration) ([]byte, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pending) == 0 {
		return nil, nil
	}
	item := q.pending[0]
	q.pending = q.pending[1:]
	q.inFlight = append(q.inFlight, item)
	return item, nil
}

func (q *memoryQueue) Ack(context.Context, []byte) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.inFlight = q.inFlight[1:]
	return nil
}

func (q *memoryQueue) Release(_ context.Context, item []byte) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.inFlight = q.inFlight[1:]
	q.pending = append([][]byte{item}, q.pending...)
	return nil
}

func (q *memoryQueue) Recover(context.Context) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pending = append(q.inFlight, q.pending...)
	q.inFlight = nil
	return nil
}

func (q *memoryQueue) size() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending) + len(q.inFlight)
}

// scriptedSender fails the next deliveries with the queued errors, then accepts everything.
type scriptedSender struct {
	mu        sync.Mutex
	failures  []error
	delivered []string
}

func (s *scriptedSender) Send(_ context.Context, report Report) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.failures) > 0 {
		err := s.failures[0]
		s.failures = s.failures[1:]
		return err
	}
	s.delivered = append(s.delivered, report.Path)
	return nil
}

func (s *scriptedSender) paths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.delivered...)
}

func runUntilEmpty(t *testing.T, outbox *Outbox, queue *memoryQueue) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		outbox.Run(ctx)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for queue.size() > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	if queue.size() > 0 {
		t.Fatalf("%d reports were never delivered", queue.size())
	}
}

func newTestOutbox(queue Queue, sender Sender) *Outbox {
	outbox := NewOutbox(queue, sender, slog.New(slog.NewTextHandler(io.Discard, nil)))
	outbox.claimWait, outbox.retryDelay = time.Millisecond, time.Millisecond
	return outbox
}

func TestOutboxDeliversInOrderAndRetriesUntilTheBackendIsBack(t *testing.T) {
	queue := &memoryQueue{}
	sender := &scriptedSender{failures: []error{errors.New("backend down"), errors.New("backend down")}}
	outbox := newTestOutbox(queue, sender)
	at := time.Now()
	for _, report := range []Report{
		RoomStarted(sessionID, at),
		ParticipantJoined(sessionID, "member", "PA_1", at),
		RoomFinished(sessionID, at),
	} {
		if err := outbox.Enqueue(context.Background(), report); err != nil {
			t.Fatal(err)
		}
	}

	runUntilEmpty(t, outbox, queue)

	got := sender.paths()
	want := []string{"started", "participants/joined", "finished"}
	if len(got) != len(want) {
		t.Fatalf("delivered %v", got)
	}
	for i, suffix := range want {
		if got[i] != "/internal/classroom/sessions/"+sessionID+"/"+suffix {
			t.Errorf("delivery %d = %q, want …/%s", i, got[i], suffix)
		}
	}
}

func TestOutboxDropsWhatTheBackendRefusesAndKeepsGoing(t *testing.T) {
	queue := &memoryQueue{}
	sender := &scriptedSender{failures: []error{ErrRejected}}
	outbox := newTestOutbox(queue, sender)
	_ = outbox.Enqueue(context.Background(), RoomStarted("unknown-session", time.Now()))
	_ = outbox.Enqueue(context.Background(), RoomStarted(sessionID, time.Now()))
	_ = queue.Push(context.Background(), []byte("not json"))

	runUntilEmpty(t, outbox, queue)

	if got := sender.paths(); len(got) != 1 || got[0] != "/internal/classroom/sessions/"+sessionID+"/started" {
		t.Fatalf("delivered %v, want only the accepted report", got)
	}
}

func TestOutboxRecoversReportsLeftInFlightByAPreviousRun(t *testing.T) {
	first, _ := json.Marshal(RoomStarted(sessionID, time.Now()))
	second, _ := json.Marshal(RoomFinished(sessionID, time.Now()))
	queue := &memoryQueue{inFlight: [][]byte{first}, pending: [][]byte{second}}
	sender := &scriptedSender{}

	runUntilEmpty(t, newTestOutbox(queue, sender), queue)

	got := sender.paths()
	if len(got) != 2 || got[0] != "/internal/classroom/sessions/"+sessionID+"/started" {
		t.Fatalf("delivered %v, want the interrupted report first", got)
	}
}
