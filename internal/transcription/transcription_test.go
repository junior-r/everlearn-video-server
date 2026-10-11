package transcription

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// feed is an audio source the test pushes chunks into.
type feed struct {
	mu     sync.Mutex
	tracks map[string]chan Chunk
	closed map[string]bool
	err    error
}

func newFeed() *feed {
	return &feed{tracks: map[string]chan Chunk{}, closed: map[string]bool{}}
}

func (f *feed) Listen(ctx context.Context, track Track) (<-chan Chunk, error) {
	if f.err != nil {
		return nil, f.err
	}
	source := make(chan Chunk)
	out := make(chan Chunk)
	f.mu.Lock()
	f.tracks[track.SID] = source
	f.mu.Unlock()
	go func() {
		defer close(out)
		defer func() {
			f.mu.Lock()
			f.closed[track.SID] = true
			f.mu.Unlock()
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case chunk := <-source:
				select {
				case out <- chunk:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

func (f *feed) send(t *testing.T, sid string, chunk Chunk) {
	t.Helper()
	f.mu.Lock()
	source := f.tracks[sid]
	f.mu.Unlock()
	select {
	case source <- chunk:
	case <-time.After(time.Second):
		t.Fatalf("track %s is not being listened to", sid)
	}
}

func (f *feed) isClosed(sid string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed[sid]
}

// echo transcribes a chunk as its own bytes; "fail" is an error and "" is silence.
type echo struct{}

func (echo) Transcribe(_ context.Context, chunk Chunk, _ string) (string, error) {
	if string(chunk.Audio) == "fail" {
		return "", errors.New("provider error")
	}
	return string(chunk.Audio), nil
}

type collector struct {
	segments chan Segment
}

func (c *collector) sink(_ context.Context, segment Segment) { c.segments <- segment }

func (c *collector) next(t *testing.T) Segment {
	t.Helper()
	select {
	case segment := <-c.segments:
		return segment
	case <-time.After(time.Second):
		t.Fatal("no segment arrived")
		return Segment{}
	}
}

func newTestPipeline(source AudioSource) (*Pipeline, *collector) {
	out := &collector{segments: make(chan Segment, 8)}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewPipeline(source, echo{}, out.sink, "es", log), out
}

var alan = Track{SessionID: "math", SpeakerID: "alan", SpeakerName: "Alan Turing", SID: "TR_alan"}

func TestPipelineTurnsSpeechIntoSegmentsAndSkipsSilenceAndErrors(t *testing.T) {
	source := newFeed()
	pipeline, out := newTestPipeline(source)
	defer pipeline.Close()
	if err := pipeline.Start(alan); err != nil {
		t.Fatal(err)
	}
	startedAt := time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)

	source.send(t, alan.SID, Chunk{Audio: nil})
	source.send(t, alan.SID, Chunk{Audio: []byte("fail")})
	source.send(t, alan.SID, Chunk{Audio: []byte("Buenos días"), StartedAt: startedAt, EndedAt: startedAt.Add(4 * time.Second)})

	segment := out.next(t)
	if uuid.Validate(segment.ID) != nil {
		t.Errorf("segment id %q is not a UUID", segment.ID)
	}
	segment.ID = ""
	want := Segment{
		SessionID: "math", SpeakerID: "alan", SpeakerName: "Alan Turing", Text: "Buenos días", Language: "es",
		StartedAt: startedAt, EndedAt: startedAt.Add(4 * time.Second),
	}
	if segment != want {
		t.Fatalf("segment = %+v, want %+v", segment, want)
	}
	if len(out.segments) != 0 {
		t.Fatalf("silence or a failed chunk produced a segment")
	}
}

func TestPipelineStopsByTrackBySpeakerAndBySession(t *testing.T) {
	source := newFeed()
	pipeline, _ := newTestPipeline(source)
	defer pipeline.Close()
	grace := Track{SessionID: "math", SpeakerID: "grace", SID: "TR_grace"}
	emmy := Track{SessionID: "physics", SpeakerID: "emmy", SID: "TR_emmy"}
	linus := Track{SessionID: "math", SpeakerID: "linus", SID: "TR_linus"}
	for _, track := range []Track{alan, grace, emmy, linus} {
		if err := pipeline.Start(track); err != nil {
			t.Fatal(err)
		}
	}
	// Announced twice by LiveKit: still one transcription
	if err := pipeline.Start(alan); err != nil {
		t.Fatal(err)
	}
	stopped := func(sid string) bool {
		deadline := time.Now().Add(time.Second)
		for !source.isClosed(sid) && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		return source.isClosed(sid)
	}

	pipeline.Stop(alan.SID)
	if !stopped(alan.SID) || source.isClosed(grace.SID) {
		t.Fatal("Stop must end that track only")
	}
	pipeline.StopSpeaker("math", "grace")
	if !stopped(grace.SID) || source.isClosed(linus.SID) {
		t.Fatal("StopSpeaker must end that member's tracks only")
	}
	pipeline.StopSession("math")
	if !stopped(linus.SID) || source.isClosed(emmy.SID) {
		t.Fatal("StopSession must leave other sessions running")
	}
	pipeline.Close()
	if !stopped(emmy.SID) {
		t.Fatal("Close must end everything")
	}
}

func TestPipelineReportsATrackItCannotListenTo(t *testing.T) {
	source := newFeed()
	source.err = errors.New("cannot subscribe")
	pipeline, _ := newTestPipeline(source)
	defer pipeline.Close()

	if err := pipeline.Start(alan); err == nil {
		t.Fatal("want the source's error")
	}
}

func TestMockPipelineProducesCaptionsWhileAMicrophoneIsOpen(t *testing.T) {
	out := &collector{segments: make(chan Segment, 8)}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pipeline := NewPipeline(TimedSource{Interval: 5 * time.Millisecond}, &MockSpeechToText{}, out.sink, "es", log)
	if err := pipeline.Start(alan); err != nil {
		t.Fatal(err)
	}

	first, second := out.next(t), out.next(t)
	pipeline.Close()

	if first.Text != "Transcripción simulada n.º 1" || second.Text != "Transcripción simulada n.º 2" {
		t.Fatalf("texts = %q, %q", first.Text, second.Text)
	}
	if !second.StartedAt.Equal(first.EndedAt) || !first.EndedAt.After(first.StartedAt) {
		t.Fatalf("segments must follow each other in time: %+v then %+v", first, second)
	}
}

func TestDeepgramSendsTheAudioAndReadsTheTranscript(t *testing.T) {
	var gotAuth, gotType, gotQuery, gotBody string
	answer, status := `{"results":{"channels":[{"alternatives":[{"transcript":" Buenos días a todos. "}]}]}}`, http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotAuth, gotType, gotQuery, gotBody = r.Header.Get("Authorization"), r.Header.Get("Content-Type"), r.URL.RawQuery, string(raw)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, answer)
	}))
	defer server.Close()
	deepgram := NewDeepgram(server.URL, "dg-key")
	chunk := Chunk{Audio: []byte("opus-bytes"), ContentType: "audio/ogg"}

	text, err := deepgram.Transcribe(context.Background(), chunk, "es")

	if err != nil || text != "Buenos días a todos." {
		t.Fatalf("text = %q, err = %v", text, err)
	}
	if gotAuth != "Token dg-key" || gotType != "audio/ogg" || gotBody != "opus-bytes" {
		t.Fatalf("request: auth %q, type %q, body %q", gotAuth, gotType, gotBody)
	}
	if gotQuery != "language=es&model=nova-3&smart_format=true" {
		t.Fatalf("query = %q", gotQuery)
	}

	// Nothing recognised is silence, not an error
	answer = `{"results":{"channels":[]}}`
	if text, err = deepgram.Transcribe(context.Background(), chunk, "es"); err != nil || text != "" {
		t.Fatalf("empty result: text = %q, err = %v", text, err)
	}
	status = http.StatusUnauthorized
	if _, err = deepgram.Transcribe(context.Background(), chunk, "es"); err == nil {
		t.Fatal("want an error for a refused key")
	}
	// No audio is never sent
	gotBody = "untouched"
	if text, err = deepgram.Transcribe(context.Background(), Chunk{}, "es"); err != nil || text != "" || gotBody != "untouched" {
		t.Fatalf("empty chunk: text = %q, err = %v, called = %v", text, err, gotBody != "untouched")
	}
}
