package webhook

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/livekit/protocol/livekit"
)

const (
	session = "0199d3c2-7b1e-7c3a-9f41-2f6f5e0c1a11"
	member  = "0199d3c2-8000-7c3a-9f41-2f6f5e0c1a22"
)

// recorder notes every call it receives as one line.
type recorder struct {
	calls []string
	err   error
}

func (r *recorder) note(format string, args ...any) error {
	r.calls = append(r.calls, fmt.Sprintf(format, args...))
	return r.err
}

func (r *recorder) RoomStarted(_ context.Context, sessionID string, at time.Time) error {
	return r.note("started %s %d", sessionID, at.Unix())
}

func (r *recorder) RoomFinished(_ context.Context, sessionID string, at time.Time) error {
	return r.note("finished %s %d", sessionID, at.Unix())
}

func (r *recorder) ParticipantJoined(_ context.Context, p Participant) error {
	return r.note("joined %s %s %s %d", p.SessionID, p.MembershipID, p.SID, p.JoinedAt.Unix())
}

func (r *recorder) ParticipantLeft(_ context.Context, p Participant, at time.Time) error {
	return r.note("left %s %s %s %d %d", p.SessionID, p.MembershipID, p.SID, p.JoinedAt.Unix(), at.Unix())
}

func (r *recorder) MicrophoneOpened(_ context.Context, m Microphone) error {
	return r.note("mic-on %s %s %s %s", m.SessionID, m.SpeakerID, m.SpeakerName, m.TrackSID)
}

func (r *recorder) MicrophoneClosed(_ context.Context, m Microphone) error {
	return r.note("mic-off %s %s", m.SessionID, m.TrackSID)
}

func (r *recorder) RecordingFinished(_ context.Context, rec Recording) error {
	return r.note("recorded %s %s %s %d %d %d", rec.SessionID, rec.EgressID, rec.Path, rec.StartedAt.Unix(), rec.EndedAt.Unix(), rec.SizeBytes)
}

func deliver(listener *recorder, event *livekit.WebhookEvent, receiveErr error) int {
	receive := func(*http.Request) (*livekit.WebhookEvent, error) { return event, receiveErr }
	handler := NewHandler(receive, listener, slog.New(slog.NewTextHandler(io.Discard, nil)))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/webhooks/livekit", nil))
	return response.Code
}

func classRoom() *livekit.Room { return &livekit.Room{Name: "class-" + session} }

func student() *livekit.ParticipantInfo {
	return &livekit.ParticipantInfo{Identity: member, Sid: "PA_1", Name: "Grace Hopper", JoinedAt: 1000}
}

func TestDispatchesEachClassEventToItsListener(t *testing.T) {
	microphone := &livekit.TrackInfo{Sid: "TR_mic", Type: livekit.TrackType_AUDIO, Source: livekit.TrackSource_MICROPHONE}
	cases := []struct {
		event *livekit.WebhookEvent
		want  string
	}{
		{&livekit.WebhookEvent{Event: "room_started", Room: classRoom(), CreatedAt: 990}, "started " + session + " 990"},
		{&livekit.WebhookEvent{Event: "room_finished", Room: classRoom(), CreatedAt: 5000}, "finished " + session + " 5000"},
		{
			&livekit.WebhookEvent{Event: "participant_joined", Room: classRoom(), Participant: student(), CreatedAt: 1001},
			"joined " + session + " " + member + " PA_1 1000",
		},
		{
			&livekit.WebhookEvent{Event: "participant_left", Room: classRoom(), Participant: student(), CreatedAt: 4600},
			"left " + session + " " + member + " PA_1 1000 4600",
		},
		{
			&livekit.WebhookEvent{Event: "track_published", Room: classRoom(), Participant: student(), Track: microphone},
			"mic-on " + session + " " + member + " Grace Hopper TR_mic",
		},
		{
			&livekit.WebhookEvent{Event: "track_unpublished", Room: classRoom(), Participant: student(), Track: microphone},
			"mic-off " + session + " TR_mic",
		},
	}
	for _, tc := range cases {
		listener := &recorder{}
		if status := deliver(listener, tc.event, nil); status != http.StatusOK {
			t.Errorf("%s: status %d", tc.event.Event, status)
		}
		if len(listener.calls) != 1 || listener.calls[0] != tc.want {
			t.Errorf("%s: calls = %q, want %q", tc.event.Event, listener.calls, tc.want)
		}
	}
}

func TestIgnoresWhatIsNotAClassOrNotAMember(t *testing.T) {
	camera := &livekit.TrackInfo{Sid: "TR_cam", Type: livekit.TrackType_VIDEO, Source: livekit.TrackSource_CAMERA}
	screenAudio := &livekit.TrackInfo{Sid: "TR_sa", Type: livekit.TrackType_AUDIO, Source: livekit.TrackSource_SCREEN_SHARE_AUDIO}
	stranger := &livekit.ParticipantInfo{Identity: "recorder-bot", Sid: "PA_9"}
	events := []*livekit.WebhookEvent{
		{Event: "room_started", Room: &livekit.Room{Name: "someone-elses-room"}},
		{Event: "room_started", Room: &livekit.Room{Name: "class-not-a-uuid"}},
		{Event: "participant_joined", Room: &livekit.Room{Name: "lobby"}, Participant: student()},
		{Event: "participant_joined", Room: classRoom(), Participant: stranger},
		{Event: "participant_left", Room: classRoom(), Participant: stranger},
		{Event: "track_published", Room: classRoom(), Participant: student(), Track: camera},
		{Event: "track_published", Room: classRoom(), Participant: student(), Track: screenAudio},
		{Event: "egress_started", Room: classRoom()},
		{Event: "participant_joined"},
	}
	for _, event := range events {
		listener := &recorder{}
		if status := deliver(listener, event, nil); status != http.StatusOK {
			t.Errorf("%s: status %d, want 200 so LiveKit does not retry", event.Event, status)
		}
		if len(listener.calls) != 0 {
			t.Errorf("%s in %q: unexpected calls %q", event.Event, event.GetRoom().GetName(), listener.calls)
		}
	}
}

func TestRejectsUnsignedWebhooksAndAsksForRedeliveryOnFailure(t *testing.T) {
	listener := &recorder{}
	if status := deliver(listener, nil, errors.New("bad signature")); status != http.StatusUnauthorized {
		t.Errorf("unsigned webhook: status %d, want 401", status)
	}
	if len(listener.calls) != 0 {
		t.Errorf("an unsigned webhook reached the listener: %q", listener.calls)
	}

	failing := &recorder{err: errors.New("redis down")}
	event := &livekit.WebhookEvent{Event: "participant_joined", Room: classRoom(), Participant: student()}
	if status := deliver(failing, event, nil); status != http.StatusInternalServerError {
		t.Errorf("failed handling: status %d, want 500", status)
	}
}

func TestPassesOnTheFileOfAFinishedRecording(t *testing.T) {
	ended := func(status livekit.EgressStatus, files ...*livekit.FileInfo) *livekit.WebhookEvent {
		return &livekit.WebhookEvent{Event: "egress_ended", EgressInfo: &livekit.EgressInfo{
			EgressId: "EG_1", RoomName: "class-" + session, Status: status, FileResults: files,
		}}
	}
	file := &livekit.FileInfo{Filename: "/recordings/" + session + "/a.mp4", StartedAt: 1000e9, EndedAt: 4600e9, Size: 2048}
	empty := &livekit.FileInfo{Filename: "/recordings/" + session + "/b.mp4"}
	cases := []struct {
		event *livekit.WebhookEvent
		want  []string
	}{
		{ended(livekit.EgressStatus_EGRESS_COMPLETE, file), []string{"recorded " + session + " EG_1 /recordings/" + session + "/a.mp4 1000 4600 2048"}},
		{ended(livekit.EgressStatus_EGRESS_FAILED, empty, file), []string{"recorded " + session + " EG_1 /recordings/" + session + "/a.mp4 1000 4600 2048"}},
		{ended(livekit.EgressStatus_EGRESS_FAILED, empty), nil},
		{&livekit.WebhookEvent{Event: "egress_ended", EgressInfo: &livekit.EgressInfo{RoomName: "lobby", FileResults: []*livekit.FileInfo{file}}}, nil},
	}
	for _, c := range cases {
		listener := &recorder{}
		if code := deliver(listener, c.event, nil); code != http.StatusOK {
			t.Fatalf("status = %d", code)
		}
		if fmt.Sprint(listener.calls) != fmt.Sprint(c.want) {
			t.Errorf("calls = %v, want %v", listener.calls, c.want)
		}
	}
}
