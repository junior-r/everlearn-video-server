// Package lab lets the Teacher of a class share a lab with the room: its key and its state,
// which every browser draws on its own, instead of a video of it.
package lab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/everlearn/video-server/internal/access"
	"github.com/everlearn/video-server/internal/room"
)

// maxStateBytes bounds the state of a shared lab: parameters and an exercise, never a recording.
const maxStateBytes = 8 << 10

// Room is what sharing a lab needs from the video server.
type Room interface {
	SetMetadata(ctx context.Context, roomName, metadata string) error
}

// Shared is the lab on show in a class.
type Shared struct {
	LabKey    string          `json:"labKey"`
	State     json.RawMessage `json:"state"`
	SharedBy  string          `json:"sharedBy"`
	UpdatedAt string          `json:"updatedAt"`
}

// Metadata is the room metadata this service writes. A nil Lab means nothing is shared.
type Metadata struct {
	Lab *Shared `json:"lab"`
}

// Handler serves the endpoints that share a lab. Only the Teacher may, and only a lab their token lists.
type Handler struct {
	auth access.Authenticator
	room Room
	log  *slog.Logger
	now  func() time.Time
}

// NewHandler builds the lab sharing endpoints.
func NewHandler(auth access.Authenticator, room Room, log *slog.Logger) *Handler {
	return &Handler{auth: auth, room: room, log: log, now: time.Now}
}

// teacher identifies the caller and refuses, with 403, anyone who is not the Teacher of the class.
func (h *Handler) teacher(w http.ResponseWriter, request *http.Request) (access.Caller, bool) {
	caller, ok := access.Identify(h.auth, w, request)
	if !ok {
		return caller, false
	}
	if !caller.Teacher {
		w.WriteHeader(http.StatusForbidden)
		return caller, false
	}
	return caller, true
}

// write stores the metadata and answers: 204, 404 when the room is gone, 502 otherwise.
func (h *Handler) write(w http.ResponseWriter, request *http.Request, caller access.Caller, metadata Metadata) {
	payload, err := json.Marshal(metadata)
	if err == nil {
		err = h.room.SetMetadata(request.Context(), caller.Room, string(payload))
	}
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, room.ErrNotInRoom):
		w.WriteHeader(http.StatusNotFound)
	default:
		h.log.Error("sharing a lab", "session", caller.SessionID, "error", err)
		w.WriteHeader(http.StatusBadGateway)
	}
}

// Share puts a lab on show, or updates the one on show. The state must be a JSON object.
func (h *Handler) Share(w http.ResponseWriter, request *http.Request) {
	caller, ok := h.teacher(w, request)
	if !ok {
		return
	}
	var body struct {
		LabKey string          `json:"labKey"`
		State  json.RawMessage `json:"state"`
	}
	if !access.Decode(w, request, &body) {
		return
	}
	state := bytes.TrimSpace(body.State)
	if len(state) == 0 || state[0] != '{' || len(state) > maxStateBytes {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	// The backend signed which labs the institution lets this Teacher use
	if !slices.Contains(caller.Labs, body.LabKey) {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	h.write(w, request, caller, Metadata{Lab: &Shared{
		LabKey:    body.LabKey,
		State:     state,
		SharedBy:  caller.Identity,
		UpdatedAt: h.now().UTC().Format(time.RFC3339Nano),
	}})
}

// Stop takes the lab off show.
func (h *Handler) Stop(w http.ResponseWriter, request *http.Request) {
	caller, ok := h.teacher(w, request)
	if !ok {
		return
	}
	h.write(w, request, caller, Metadata{})
}
