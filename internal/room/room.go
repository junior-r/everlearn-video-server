// Package room maps LiveKit names to Everlearn ids: a room is a class session and a participant identity is a Membership id.
package room

import (
	"strings"

	"github.com/google/uuid"
)

// Topics of the data messages the video-server publishes into a room.
const (
	TopicChat     = "chat"
	TopicCaptions = "captions"
)

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
