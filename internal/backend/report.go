// Package backend is the client for the NestJS internal API, the only place events are persisted.
package backend

import (
	"encoding/json"
	"fmt"
	"time"
)

// Report is one fact about a live class, addressed to an endpoint of the backend's internal API.
type Report struct {
	Path string          `json:"path"`
	Body json.RawMessage `json:"body"`
}

func report(sessionID, endpoint string, body any) Report {
	raw, err := json.Marshal(body)
	if err != nil {
		// Bodies are flat structs of strings, so this cannot happen
		panic(fmt.Sprintf("backend: encoding %s report: %v", endpoint, err))
	}
	return Report{Path: "/internal/classroom/sessions/" + sessionID + "/" + endpoint, Body: raw}
}

// stamp formats a time the way the backend parses it: UTC, to the millisecond.
func stamp(at time.Time) string {
	return at.UTC().Format("2006-01-02T15:04:05.000Z")
}

// RoomStarted reports that a session's room opened.
func RoomStarted(sessionID string, at time.Time) Report {
	return report(sessionID, "started", map[string]string{"startedAt": stamp(at)})
}

// RoomFinished reports that a session's room closed.
func RoomFinished(sessionID string, at time.Time) Report {
	return report(sessionID, "finished", map[string]string{"endedAt": stamp(at)})
}

// ParticipantJoined reports one connection of a member to a session.
func ParticipantJoined(sessionID, membershipID, participantSID string, joinedAt time.Time) Report {
	return report(sessionID, "participants/joined", map[string]string{
		"membershipId":   membershipID,
		"participantSid": participantSID,
		"joinedAt":       stamp(joinedAt),
	})
}

// ParticipantLeft reports the end of a connection. It repeats the join time so the backend can store the stay even if the join report was lost.
func ParticipantLeft(sessionID, membershipID, participantSID string, joinedAt, leftAt time.Time) Report {
	return report(sessionID, "participants/left", map[string]string{
		"membershipId":   membershipID,
		"participantSid": participantSID,
		"joinedAt":       stamp(joinedAt),
		"leftAt":         stamp(leftAt),
	})
}

// ChatMessage reports a message sent in a session. The id makes a retried delivery harmless.
func ChatMessage(sessionID, id, senderID, content string, sentAt time.Time) Report {
	return report(sessionID, "chat-messages", map[string]string{
		"id":       id,
		"senderId": senderID,
		"content":  content,
		"sentAt":   stamp(sentAt),
	})
}

// ChatMessageDeleted reports that a message was taken down, by its author or by the Teacher.
func ChatMessageDeleted(sessionID, id, deletedByID string, at time.Time) Report {
	return report(sessionID, "chat-messages/deleted", map[string]string{
		"id":          id,
		"deletedById": deletedByID,
		"deletedAt":   stamp(at),
	})
}

// TranscriptSegment reports one transcribed stretch of speech.
func TranscriptSegment(sessionID, id, speakerID, text, language string, startedAt, endedAt time.Time) Report {
	return report(sessionID, "transcript-segments", map[string]string{
		"id":        id,
		"speakerId": speakerID,
		"text":      text,
		"language":  language,
		"startedAt": stamp(startedAt),
		"endedAt":   stamp(endedAt),
	})
}

// Recording reports a finished recording of a session: a file Egress left in the spool, named relative to it.
// The Egress id makes a retried delivery harmless.
func Recording(sessionID, egressID, file string, startedAt, endedAt time.Time, sizeBytes int64) Report {
	return report(sessionID, "recordings", map[string]any{
		"egressId":  egressID,
		"file":      file,
		"startedAt": stamp(startedAt),
		"endedAt":   stamp(endedAt),
		"sizeBytes": sizeBytes,
	})
}
