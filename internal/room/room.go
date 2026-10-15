// Package room maps LiveKit names to Everlearn ids: a room is a class session and a participant identity is a Membership id.
package room

import (
	"errors"
	"strings"

	"github.com/google/uuid"
)

// Topics of the data messages the video-server publishes into a room.
const (
	TopicChat        = "chat"
	TopicChatDeleted = "chat-deleted"
	TopicCaptions    = "captions"
)

// Participant attributes. The backend signs the role into the access token; the hand is set only by the video-server.
const (
	AttributeRole = "role"
	AttributeHand = "hand"
	RoleTeacher   = "TEACHER"
)

// ErrNotInRoom is returned when a participant, or the room itself, is no longer there.
var ErrNotInRoom = errors.New("room: participant is not in the room")

// Member is who a room access token says its holder is.
type Member struct {
	Room     string
	Identity string
	Name     string
	// Teacher is true for the Teacher giving the class, the only one who may moderate it.
	Teacher bool
}

// The backend names a session's room "class-<session id>".
const prefix = "class-"

// RoomName returns the room of a class session.
func RoomName(sessionID string) string {
	return prefix + sessionID
}

// SessionID returns the class session a room belongs to. ok is false for rooms that are not ours.
func SessionID(roomName string) (sessionID string, ok bool) {
	id, found := strings.CutPrefix(roomName, prefix)
	if !found || uuid.Validate(id) != nil {
		return "", false
	}
	return id, true
}

// IsMembership reports whether a participant identity is a Membership id.
func IsMembership(identity string) bool {
	return uuid.Validate(identity) == nil
}
