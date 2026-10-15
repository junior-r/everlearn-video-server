// Package access identifies the class member behind a browser request from the room access token it carries.
package access

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/everlearn/video-server/internal/room"
)

const (
	maxBodyBytes = 16 << 10
	bearerPrefix = "Bearer "
)

// Authenticator checks a room access token issued by the backend.
type Authenticator interface {
	Authenticate(token string) (room.Member, error)
}

// Caller is a member of a class, and the class session their token lets them into.
type Caller struct {
	room.Member
	SessionID string
}

// Identify returns who sent a request. ok is false after the refusal has been written:
// 401 without a valid token, 403 when the token is not a member's token for a class room.
func Identify(auth Authenticator, w http.ResponseWriter, request *http.Request) (caller Caller, ok bool) {
	token, found := strings.CutPrefix(request.Header.Get("Authorization"), bearerPrefix)
	if !found {
		w.WriteHeader(http.StatusUnauthorized)
		return Caller{}, false
	}
	member, err := auth.Authenticate(token)
	if err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		return Caller{}, false
	}
	sessionID, isClass := room.SessionID(member.Room)
	if !isClass || !room.IsMembership(member.Identity) {
		w.WriteHeader(http.StatusForbidden)
		return Caller{}, false
	}
	return Caller{Member: member, SessionID: sessionID}, true
}

// Decode reads a small JSON body into target. ok is false after a 400 has been written.
func Decode(w http.ResponseWriter, request *http.Request, target any) (ok bool) {
	if err := json.NewDecoder(http.MaxBytesReader(w, request.Body, maxBodyBytes)).Decode(target); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return false
	}
	return true
}
