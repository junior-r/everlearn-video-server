// Package metrics reports the lifecycle of each meeting: when its room opened and when it closed.
package metrics

import (
	"context"
	"time"

	"github.com/everlearn/video-server/internal/backend"
)

// Reporter buffers a report for the backend.
type Reporter interface {
	Enqueue(ctx context.Context, report backend.Report) error
}

// Rooms reports when a session's room opens and closes. The backend derives the session status, its real duration and its peak attendance from these and from the attendance reports.
type Rooms struct {
	reports Reporter
}

// NewRooms builds a lifecycle reporter.
func NewRooms(reports Reporter) *Rooms {
	return &Rooms{reports: reports}
}

// Started reports that the first participant opened the room.
func (r *Rooms) Started(ctx context.Context, sessionID string, at time.Time) error {
	return r.reports.Enqueue(ctx, backend.RoomStarted(sessionID, at))
}

// Finished reports that the room closed.
func (r *Rooms) Finished(ctx context.Context, sessionID string, at time.Time) error {
	return r.reports.Enqueue(ctx, backend.RoomFinished(sessionID, at))
}
