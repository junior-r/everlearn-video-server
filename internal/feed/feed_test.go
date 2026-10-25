package feed

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/everlearn/video-server/internal/room"
)

const (
	session = "0199d3c2-7b1e-7c3a-9f41-2f6f5e0c1a11"
	member  = "0199d3c2-8000-7c3a-9f41-2f6f5e0c1a22"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// memoryLog plays Redis: one trimmed list per room and stream.
type memoryLog struct {
	lists     map[string][]Packet
	appendErr error
	recentErr error
}

func newMemoryLog() *memoryLog {
	return &memoryLog{lists: map[string][]Packet{}}
}

func (l *memoryLog) Append(_ context.Context, roomName string, stream Stream, packet Packet) error {
	if l.appendErr != nil {
		return l.appendErr
	}
	list := append(l.lists[roomName+":"+stream.Name], packet)
	if len(list) > stream.Keep {
		list = list[len(list)-stream.Keep:]
	}
	l.lists[roomName+":"+stream.Name] = list
	return nil
}

func (l *memoryLog) Recent(_ context.Context, roomName string) ([]Packet, error) {
	if l.recentErr != nil {
		return nil, l.recentErr
	}
	var packets []Packet
	for _, stream := range Streams {
		packets = append(packets, l.lists[roomName+":"+stream.Name]...)
	}
	return packets, nil
}

type publisher struct {
	err   error
	count int
}

func (p *publisher) Publish(context.Context, string, string, []byte) error {
	if p.err != nil {
		return p.err
	}
	p.count++
	return nil
}

type authenticator struct{ member room.Member }

func (a authenticator) Authenticate(token string) (room.Member, error) {
	if token != "valid-token" {
		return room.Member{}, errors.New("bad token")
	}
	return a.member, nil
}

func TestTheRecorderKeepsChatAndCaptionsOnlyAfterTheRoomGotThem(t *testing.T) {
	log := newMemoryLog()
	next := &publisher{}
	recorder := NewRecorder(next, log, quiet)
	ctx := context.Background()
	roomName := room.RoomName(session)

	for _, topic := range []string{room.TopicChat, room.TopicChatDeleted, room.TopicCaptions, "something-else"} {
		if err := recorder.Publish(ctx, roomName, topic, []byte(`{"id":"1"}`)); err != nil {
			t.Fatal(err)
		}
	}
	if next.count != 4 {
		t.Errorf("published %d packets, want 4", next.count)
	}
	packets, _ := log.Recent(ctx, roomName)
	topics := []string{}
	for _, packet := range packets {
		topics = append(topics, packet.Topic)
	}
	if len(topics) != 3 || topics[0] != room.TopicChat || topics[1] != room.TopicChatDeleted || topics[2] != room.TopicCaptions {
		t.Errorf("kept %v, want chat, its deletion and the caption", topics)
	}

	failing := NewRecorder(&publisher{err: errors.New("livekit down")}, log, quiet)
	if err := failing.Publish(ctx, roomName, room.TopicChat, []byte(`{"id":"2"}`)); err == nil {
		t.Error("a message the room never got was reported as published")
	}
	if packets, _ := log.Recent(ctx, roomName); len(packets) != 3 {
		t.Errorf("kept a message the room never got: %d packets", len(packets))
	}
}

func TestTheRecorderStillPublishesWhenTheLogIsDown(t *testing.T) {
	log := newMemoryLog()
	log.appendErr = errors.New("redis down")
	next := &publisher{}
	if err := NewRecorder(next, log, quiet).Publish(context.Background(), "class-1", room.TopicChat, []byte(`{}`)); err != nil {
		t.Errorf("err = %v, want the publish to succeed", err)
	}
	if next.count != 1 {
		t.Error("the message was not published")
	}
}

func TestOnlyTheLastFewCaptionsAreKept(t *testing.T) {
	log := newMemoryLog()
	recorder := NewRecorder(&publisher{}, log, quiet)
	for _, id := range []string{"1", "2", "3", "4", "5"} {
		_ = recorder.Publish(context.Background(), "class-1", room.TopicCaptions, []byte(`{"id":"`+id+`"}`))
	}
	packets, _ := log.Recent(context.Background(), "class-1")
	if len(packets) != captionsStream.Keep || string(packets[0].Payload) != `{"id":"3"}` {
		t.Errorf("kept %v, want the newest %d", packets, captionsStream.Keep)
	}
}

func get(handler http.Handler, token string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "/feed", nil)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestAMemberIsReplayedTheirOwnRoomOnly(t *testing.T) {
	log := newMemoryLog()
	ctx := context.Background()
	_ = log.Append(ctx, room.RoomName(session), chatStream, Packet{Topic: room.TopicChat, Payload: json.RawMessage(`{"id":"mine"}`)})
	_ = log.Append(ctx, "class-another", chatStream, Packet{Topic: room.TopicChat, Payload: json.RawMessage(`{"id":"theirs"}`)})
	handler := NewHandler(authenticator{room.Member{Room: room.RoomName(session), Identity: member}}, log, quiet)

	response := get(handler, "valid-token")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	var body struct{ Packets []Packet }
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Packets) != 1 || string(body.Packets[0].Payload) != `{"id":"mine"}` {
		t.Errorf("packets = %v, want only the caller's room", body.Packets)
	}
}

func TestAnEmptyRoomReplaysAnEmptyList(t *testing.T) {
	handler := NewHandler(authenticator{room.Member{Room: room.RoomName(session), Identity: member}}, newMemoryLog(), quiet)
	if body := get(handler, "valid-token").Body.String(); body != "{\"packets\":[]}\n" {
		t.Errorf("body = %q", body)
	}
}

func TestTheFeedRefusesWithoutAValidTokenAndSaysWhenItCannotRead(t *testing.T) {
	log := newMemoryLog()
	handler := NewHandler(authenticator{room.Member{Room: room.RoomName(session), Identity: member}}, log, quiet)
	if code := get(handler, "").Code; code != http.StatusUnauthorized {
		t.Errorf("without a token: %d", code)
	}
	if code := get(handler, "forged").Code; code != http.StatusUnauthorized {
		t.Errorf("with a bad token: %d", code)
	}
	log.recentErr = errors.New("redis down")
	if code := get(handler, "valid-token").Code; code != http.StatusServiceUnavailable {
		t.Errorf("with redis down: %d", code)
	}
}
