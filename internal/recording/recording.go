// Package recording films a class at its Teacher's request, through LiveKit Egress, and reports
// each finished file to the backend, which keeps it where the deployment stores recordings.
package recording

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/everlearn/video-server/internal/access"
	"github.com/everlearn/video-server/internal/backend"
	"github.com/everlearn/video-server/internal/room"
)

// Egress is what recording needs from LiveKit.
type Egress interface {
	StartRecording(ctx context.Context, roomName, filepath string) error
	ActiveRecordings(ctx context.Context, roomName string) ([]string, error)
	StopRecording(ctx context.Context, egressID string) error
}

// Handler serves the endpoints that start and stop recording a class. Only a Teacher whose token
// the backend signed with the record attribute may use them.
type Handler struct {
	auth   access.Authenticator
	egress Egress
	// dir is where Egress writes, as Egress sees its disk.
	dir   string
	log   *slog.Logger
	newID func() string
}

// NewHandler builds the recording endpoints. dir is the spool Egress writes finished files to.
func NewHandler(auth access.Authenticator, egress Egress, dir string, log *slog.Logger) *Handler {
	return &Handler{auth: auth, egress: egress, dir: dir, log: log, newID: uuid.NewString}
}

// recorder identifies the caller and refuses, with 403, anyone the token does not let record.
func (h *Handler) recorder(w http.ResponseWriter, request *http.Request) (access.Caller, bool) {
	caller, ok := access.Identify(h.auth, w, request)
	if !ok {
		return caller, false
	}
	if !caller.Teacher || !caller.CanRecord {
		w.WriteHeader(http.StatusForbidden)
		return caller, false
	}
	return caller, true
}

// reply answers: 204, 404 when the room is gone, 502 when Egress could not do it.
func (h *Handler) reply(w http.ResponseWriter, action string, caller access.Caller, err error) {
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, room.ErrNotInRoom):
		w.WriteHeader(http.StatusNotFound)
	default:
		h.log.Error(action, "session", caller.SessionID, "error", err)
		w.WriteHeader(http.StatusBadGateway)
	}
}

// Start begins recording the class. A class already being recorded is left as it is.
func (h *Handler) Start(w http.ResponseWriter, request *http.Request) {
	caller, ok := h.recorder(w, request)
	if !ok {
		return
	}
	active, err := h.egress.ActiveRecordings(request.Context(), caller.Room)
	if err != nil || len(active) > 0 {
		h.reply(w, "listing recordings", caller, err)
		return
	}
	// The name is chosen here, so nothing from a browser reaches a path on disk
	file := path.Join(h.dir, caller.SessionID, h.newID()+".mp4")
	h.reply(w, "starting a recording", caller, h.egress.StartRecording(request.Context(), caller.Room, file))
}

// Stop ends every recording of the class in progress. A class not being recorded is left as it is.
func (h *Handler) Stop(w http.ResponseWriter, request *http.Request) {
	caller, ok := h.recorder(w, request)
	if !ok {
		return
	}
	active, err := h.egress.ActiveRecordings(request.Context(), caller.Room)
	for _, egressID := range active {
		if err != nil {
			break
		}
		// One that ended meanwhile is already stopped
		if err = h.egress.StopRecording(request.Context(), egressID); errors.Is(err, room.ErrNotInRoom) {
			err = nil
		}
	}
	h.reply(w, "stopping a recording", caller, err)
}

// Reporter buffers a report for the backend.
type Reporter interface {
	Enqueue(ctx context.Context, report backend.Report) error
}

// File is a recording Egress finished writing.
type File struct {
	SessionID string
	EgressID  string
	// Path is where Egress wrote it, as Egress sees its disk.
	Path      string
	StartedAt time.Time
	EndedAt   time.Time
	SizeBytes int64
}

// Archive tells the backend about each finished recording, so it can store the file and list it for the class.
type Archive struct {
	reports Reporter
	dir     string
	log     *slog.Logger
}

// NewArchive builds the reporter of finished recordings. dir is the spool Egress writes to.
func NewArchive(reports Reporter, dir string, log *slog.Logger) *Archive {
	return &Archive{reports: reports, dir: dir, log: log}
}

// Finished reports a recording, named relative to the spool. A file outside it was not started here and is ignored.
func (a *Archive) Finished(ctx context.Context, file File) error {
	name, inSpool := strings.CutPrefix(path.Clean(file.Path), path.Clean(a.dir)+"/")
	if !inSpool {
		a.log.Warn("ignoring a recording outside the spool", "session", file.SessionID, "path", file.Path)
		return nil
	}
	return a.reports.Enqueue(ctx, backend.Recording(
		file.SessionID, file.EgressID, name, file.StartedAt, file.EndedAt, file.SizeBytes,
	))
}
