// Package transcription runs the real-time speech-to-text pipeline for live rooms.
package transcription

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Track is one microphone published in a class session.
type Track struct {
	SessionID   string
	SpeakerID   string
	SpeakerName string
	SID         string
}

// Chunk is a few seconds of audio from one track.
type Chunk struct {
	Audio       []byte
	ContentType string
	StartedAt   time.Time
	EndedAt     time.Time
}

// Segment is one transcribed stretch of speech, ready to show as a caption and to store.
type Segment struct {
	ID          string
	SessionID   string
	SpeakerID   string
	SpeakerName string
	Text        string
	Language    string
	StartedAt   time.Time
	EndedAt     time.Time
}

// AudioSource delivers the audio of a track in chunks until ctx is cancelled, then closes the channel.
type AudioSource interface {
	Listen(ctx context.Context, track Track) (<-chan Chunk, error)
}

// SpeechToText transcribes one chunk. An empty text means nothing was said.
type SpeechToText interface {
	Transcribe(ctx context.Context, chunk Chunk, language string) (string, error)
}

// Sink receives each finished segment.
type Sink func(ctx context.Context, segment Segment)

// Pipeline transcribes every open microphone: audio source → speech-to-text → sink.
type Pipeline struct {
	source   AudioSource
	stt      SpeechToText
	sink     Sink
	language string
	log      *slog.Logger

	mu      sync.Mutex
	running map[string]runningTrack
	wg      sync.WaitGroup
}

type runningTrack struct {
	track  Track
	cancel context.CancelFunc
}

// NewPipeline builds a pipeline. Nothing runs until a track is started.
func NewPipeline(source AudioSource, stt SpeechToText, sink Sink, language string, log *slog.Logger) *Pipeline {
	return &Pipeline{source: source, stt: stt, sink: sink, language: language, log: log, running: make(map[string]runningTrack)}
}

// Start begins transcribing a track. Starting a track that is already running does nothing.
func (p *Pipeline) Start(track Track) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.running[track.SID]; exists {
		return nil
	}
	// Not tied to the webhook request: the track outlives the request that announced it
	ctx, cancel := context.WithCancel(context.Background())
	chunks, err := p.source.Listen(ctx, track)
	if err != nil {
		cancel()
		return err
	}
	p.running[track.SID] = runningTrack{track: track, cancel: cancel}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.transcribe(ctx, track, chunks)
	}()
	return nil
}

func (p *Pipeline) transcribe(ctx context.Context, track Track, chunks <-chan Chunk) {
	for chunk := range chunks {
		text, err := p.stt.Transcribe(ctx, chunk, p.language)
		if err != nil {
			if ctx.Err() == nil {
				p.log.Warn("transcribing a chunk", "session", track.SessionID, "track", track.SID, "error", err)
			}
			continue
		}
		if text == "" {
			continue
		}
		p.sink(ctx, Segment{
			ID:          uuid.Must(uuid.NewV7()).String(),
			SessionID:   track.SessionID,
			SpeakerID:   track.SpeakerID,
			SpeakerName: track.SpeakerName,
			Text:        text,
			Language:    p.language,
			StartedAt:   chunk.StartedAt,
			EndedAt:     chunk.EndedAt,
		})
	}
}

// Stop ends the transcription of one track.
func (p *Pipeline) Stop(trackSID string) {
	p.stopWhere(func(track Track) bool { return track.SID == trackSID })
}

// StopSpeaker ends the transcription of everything a member publishes in a session.
func (p *Pipeline) StopSpeaker(sessionID, speakerID string) {
	p.stopWhere(func(track Track) bool { return track.SessionID == sessionID && track.SpeakerID == speakerID })
}

// StopSession ends every transcription of a session.
func (p *Pipeline) StopSession(sessionID string) {
	p.stopWhere(func(track Track) bool { return track.SessionID == sessionID })
}

// Close stops everything and waits for the running transcriptions to end.
func (p *Pipeline) Close() {
	p.stopWhere(func(Track) bool { return true })
	p.wg.Wait()
}

func (p *Pipeline) stopWhere(matches func(Track) bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for sid, running := range p.running {
		if matches(running.track) {
			running.cancel()
			delete(p.running, sid)
		}
	}
}
