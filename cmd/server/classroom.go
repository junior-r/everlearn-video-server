package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/everlearn/video-server/internal/attendance"
	"github.com/everlearn/video-server/internal/backend"
	"github.com/everlearn/video-server/internal/metrics"
	"github.com/everlearn/video-server/internal/room"
	"github.com/everlearn/video-server/internal/transcription"
	"github.com/everlearn/video-server/internal/webhook"
)

// classroom routes each room event to the package responsible for it.
type classroom struct {
	rooms      *metrics.Rooms
	attendance *attendance.Recorder
	// Nil when transcription is off
	transcript *transcription.Pipeline
}

func (c *classroom) RoomStarted(ctx context.Context, sessionID string, at time.Time) error {
	return c.rooms.Started(ctx, sessionID, at)
}

func (c *classroom) RoomFinished(ctx context.Context, sessionID string, at time.Time) error {
	if err := c.rooms.Finished(ctx, sessionID, at); err != nil {
		return err
	}
	c.attendance.SessionFinished(sessionID)
	if c.transcript != nil {
		c.transcript.StopSession(sessionID)
	}
	return nil
}

func stayOf(participant webhook.Participant) attendance.Stay {
	return attendance.Stay{
		SessionID:      participant.SessionID,
		MembershipID:   participant.MembershipID,
		ParticipantSID: participant.SID,
		JoinedAt:       participant.JoinedAt,
	}
}

func (c *classroom) ParticipantJoined(ctx context.Context, participant webhook.Participant) error {
	return c.attendance.Joined(ctx, stayOf(participant))
}

func (c *classroom) ParticipantLeft(ctx context.Context, participant webhook.Participant, at time.Time) error {
	if c.transcript != nil {
		c.transcript.StopSpeaker(participant.SessionID, participant.MembershipID)
	}
	return c.attendance.Left(ctx, stayOf(participant), at)
}

func (c *classroom) MicrophoneOpened(_ context.Context, microphone webhook.Microphone) error {
	if c.transcript == nil {
		return nil
	}
	return c.transcript.Start(transcription.Track{
		SessionID:   microphone.SessionID,
		SpeakerID:   microphone.SpeakerID,
		SpeakerName: microphone.SpeakerName,
		SID:         microphone.TrackSID,
	})
}

func (c *classroom) MicrophoneClosed(_ context.Context, microphone webhook.Microphone) error {
	if c.transcript != nil {
		c.transcript.Stop(microphone.TrackSID)
	}
	return nil
}

// roomPublisher delivers a data message to everyone in a room.
type roomPublisher interface {
	Publish(ctx context.Context, roomName, topic string, payload []byte) error
}

// reporter buffers a report for the backend.
type reporter interface {
	Enqueue(ctx context.Context, report backend.Report) error
}

// captions sends each transcript segment to its room as a live caption and to the backend as history.
type captions struct {
	publisher roomPublisher
	reports   reporter
	log       *slog.Logger
}

func newCaptions(publisher roomPublisher, reports reporter, log *slog.Logger) *captions {
	return &captions{publisher: publisher, reports: reports, log: log}
}

type caption struct {
	ID          string    `json:"id"`
	SpeakerID   string    `json:"speakerId"`
	SpeakerName string    `json:"speakerName"`
	Text        string    `json:"text"`
	StartedAt   time.Time `json:"startedAt"`
	EndedAt     time.Time `json:"endedAt"`
}

func (c *captions) publish(ctx context.Context, segment transcription.Segment) {
	payload, err := json.Marshal(caption{
		ID:          segment.ID,
		SpeakerID:   segment.SpeakerID,
		SpeakerName: segment.SpeakerName,
		Text:        segment.Text,
		StartedAt:   segment.StartedAt.UTC(),
		EndedAt:     segment.EndedAt.UTC(),
	})
	if err != nil {
		c.log.Error("encoding a caption", "error", err)
		return
	}
	if err := c.publisher.Publish(ctx, room.RoomName(segment.SessionID), room.TopicCaptions, payload); err != nil {
		c.log.Warn("publishing a caption", "session", segment.SessionID, "error", err)
	}
	report := backend.TranscriptSegment(
		segment.SessionID, segment.ID, segment.SpeakerID, segment.Text, segment.Language, segment.StartedAt, segment.EndedAt,
	)
	if err := c.reports.Enqueue(ctx, report); err != nil {
		c.log.Error("transcript segment not saved", "session", segment.SessionID, "error", err)
	}
}
