// Package attendance turns participant join and leave events into attendance data keyed by Membership id.
package attendance

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/everlearn/video-server/internal/backend"
)

// Stay is one connection of a member to a class session. A reconnection is a new Stay.
type Stay struct {
	SessionID      string
	MembershipID   string
	ParticipantSID string
	JoinedAt       time.Time
}

type memberKey struct{ sessionID, membershipID string }

// Tracker adds up how long each member has been connected to each live session.
type Tracker struct {
	mu     sync.Mutex
	totals map[memberKey]time.Duration
}

// NewTracker builds an empty tracker.
func NewTracker() *Tracker {
	return &Tracker{totals: make(map[memberKey]time.Duration)}
}

// Leave closes a stay. It returns how long that connection lasted and the member's total time in the session so far.
func (t *Tracker) Leave(stay Stay, leftAt time.Time) (connected, total time.Duration) {
	// A clock difference between servers must never produce negative attendance
	connected = max(leftAt.Sub(stay.JoinedAt), 0)
	key := memberKey{stay.SessionID, stay.MembershipID}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.totals[key] += connected
	return connected, t.totals[key]
}

// Forget drops the totals of a session that has finished.
func (t *Tracker) Forget(sessionID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for key := range t.totals {
		if key.sessionID == sessionID {
			delete(t.totals, key)
		}
	}
}

// Reporter buffers a report for the backend.
type Reporter interface {
	Enqueue(ctx context.Context, report backend.Report) error
}

// Recorder reports every arrival and departure to the backend, which stores them as attendance.
type Recorder struct {
	tracker *Tracker
	reports Reporter
	log     *slog.Logger
}

// NewRecorder builds a recorder that reports through reports.
func NewRecorder(tracker *Tracker, reports Reporter, log *slog.Logger) *Recorder {
	return &Recorder{tracker: tracker, reports: reports, log: log}
}

// Joined records that a member connected.
func (r *Recorder) Joined(ctx context.Context, stay Stay) error {
	return r.reports.Enqueue(ctx, backend.ParticipantJoined(stay.SessionID, stay.MembershipID, stay.ParticipantSID, stay.JoinedAt))
}

// Left records that a member disconnected and how long they took part.
func (r *Recorder) Left(ctx context.Context, stay Stay, leftAt time.Time) error {
	report := backend.ParticipantLeft(stay.SessionID, stay.MembershipID, stay.ParticipantSID, stay.JoinedAt, leftAt)
	if err := r.reports.Enqueue(ctx, report); err != nil {
		return err
	}
	// Counted only once the report is buffered: LiveKit redelivers the event if we answer with an error
	connected, total := r.tracker.Leave(stay, leftAt)
	r.log.Info("participant left",
		"session", stay.SessionID,
		"membership", stay.MembershipID,
		"connectedSeconds", int(connected.Seconds()),
		"totalSeconds", int(total.Seconds()),
	)
	return nil
}

// SessionFinished releases what the tracker holds for a session.
func (r *Recorder) SessionFinished(sessionID string) {
	r.tracker.Forget(sessionID)
}
