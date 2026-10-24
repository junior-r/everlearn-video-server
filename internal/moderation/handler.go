// Package moderation lets a member raise their hand, and the Teacher of a class keep order in it:
// lower a hand, silence a microphone and end the class for everyone.
package moderation

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/everlearn/video-server/internal/access"
	"github.com/everlearn/video-server/internal/room"
)

// Room is what moderating needs from the video server.
type Room interface {
	SetAttribute(ctx context.Context, roomName, identity, key, value string) error
	MuteMicrophone(ctx context.Context, roomName, identity string) error
	Close(ctx context.Context, roomName string) error
}

// Handler serves the moderation endpoints. Who asks, and whether they are the Teacher,
// always comes from the room access token.
type Handler struct {
	auth access.Authenticator
	room Room
	log  *slog.Logger
	now  func() time.Time
}

// NewHandler builds the moderation endpoints.
func NewHandler(auth access.Authenticator, room Room, log *slog.Logger) *Handler {
	return &Handler{auth: auth, room: room, log: log, now: time.Now}
}

// reply answers a call to the video server: 204, 404 when the participant already left, 502 otherwise.
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

// handTime keeps milliseconds: hands raised within the same second must still come out in order.
const handTime = "2006-01-02T15:04:05.000Z07:00"

// Hand raises or lowers the caller's own hand. The value is when it went up, so hands can be taken in order.
func (h *Handler) Hand(w http.ResponseWriter, request *http.Request) {
	caller, ok := access.Identify(h.auth, w, request)
	if !ok {
		return
	}
	var body struct {
		Raised *bool `json:"raised"`
	}
	if !access.Decode(w, request, &body) {
		return
	}
	if body.Raised == nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	value := ""
	if *body.Raised {
		value = h.now().UTC().Format(handTime)
	}
	err := h.room.SetAttribute(request.Context(), caller.Room, caller.Identity, room.AttributeHand, value)
	h.reply(w, "moving a hand", caller, err)
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

// teacherAndTarget identifies the Teacher calling and the participant they act on.
func (h *Handler) teacherAndTarget(w http.ResponseWriter, request *http.Request) (caller access.Caller, identity string, ok bool) {
	caller, ok = h.teacher(w, request)
	if !ok {
		return caller, "", false
	}
	var body struct {
		Identity string `json:"identity"`
	}
	if !access.Decode(w, request, &body) {
		return caller, "", false
	}
	// Only members can be moderated: never the hidden listener
	if !room.IsMembership(body.Identity) {
		w.WriteHeader(http.StatusBadRequest)
		return caller, "", false
	}
	return caller, body.Identity, true
}

// LowerHand lowers the hand of a participant the Teacher has attended to.
func (h *Handler) LowerHand(w http.ResponseWriter, request *http.Request) {
	caller, identity, ok := h.teacherAndTarget(w, request)
	if !ok {
		return
	}
	err := h.room.SetAttribute(request.Context(), caller.Room, identity, room.AttributeHand, "")
	h.reply(w, "lowering a hand", caller, err)
}

// Mute silences a participant's microphone. Only the Teacher may.
func (h *Handler) Mute(w http.ResponseWriter, request *http.Request) {
	caller, identity, ok := h.teacherAndTarget(w, request)
	if !ok {
		return
	}
	h.reply(w, "muting a microphone", caller, h.room.MuteMicrophone(request.Context(), caller.Room, identity))
}

// End closes the class for everyone. Only the Teacher may.
func (h *Handler) End(w http.ResponseWriter, request *http.Request) {
	caller, ok := h.teacher(w, request)
	if !ok {
		return
	}
	err := h.room.Close(request.Context(), caller.Room)
	// A room that is already gone is a class that already ended
	if errors.Is(err, room.ErrNotInRoom) {
		err = nil
	}
	h.reply(w, "ending a class", caller, err)
}
