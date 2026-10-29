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
	// AttributeLabs lists, comma separated, the labs the backend lets the Teacher share with the class.
	AttributeLabs = "labs"
	// AttributeRecord is "true" when the backend lets the Teacher record the class.
	AttributeRecord = "record"
	RoleTeacher     = "TEACHER"
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
	// Labs are the keys of the labs this member may share with the room; empty for anyone but the Teacher.
	Labs []string
	// CanRecord is true when the backend lets this member record the class; never for a Student.
	CanRecord bool
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

// ParseLabs reads the labs attribute: keys separated by commas, blanks left out.
func ParseLabs(attribute string) []string {
	var labs []string
	for _, key := range strings.Split(attribute, ",") {
		if key = strings.TrimSpace(key); key != "" {
			labs = append(labs, key)
		}
	}
	return labs
}
