package transcription

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"
)

// TimedSource stands in for the room listener when the service may not join rooms: while a
// track is open it emits a chunk with no audio every interval, so the rest of the pipeline
// runs end to end without a media connection.
type TimedSource struct {
	Interval time.Duration
}

func (s TimedSource) Listen(ctx context.Context, _ Track) (<-chan Chunk, error) {
	chunks := make(chan Chunk)
	go func() {
		defer close(chunks)
		ticker := time.NewTicker(s.Interval)
		defer ticker.Stop()
		startedAt := time.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case endedAt := <-ticker.C:
				select {
				case chunks <- Chunk{StartedAt: startedAt, EndedAt: endedAt}:
				case <-ctx.Done():
					return
				}
				startedAt = endedAt
			}
		}
	}()
	return chunks, nil
}

// MockSpeechToText answers every chunk with a numbered placeholder sentence, for running without a Deepgram key.
type MockSpeechToText struct {
	count atomic.Int64
}

func (m *MockSpeechToText) Transcribe(context.Context, Chunk, string) (string, error) {
	return fmt.Sprintf("Transcripción simulada n.º %d", m.count.Add(1)), nil
}
