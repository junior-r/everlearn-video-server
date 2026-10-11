package sfu

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"

	"github.com/everlearn/video-server/internal/transcription"
)

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

// fakeTrack plays a script: each step is a packet payload, nil for a read that times out.
// The clock advances one second per read.
type fakeTrack struct {
	steps [][]byte
	clock time.Time
	reads int
}

func (f *fakeTrack) SetReadDeadline(time.Time) error { return nil }

func (f *fakeTrack) ReadRTP() (*rtp.Packet, interceptor.Attributes, error) {
	if f.reads == len(f.steps) {
		return nil, nil, io.EOF
	}
	payload := f.steps[f.reads]
	f.reads++
	f.clock = f.clock.Add(time.Second)
	if payload == nil {
		return nil, nil, timeoutError{}
	}
	sequence := uint16(f.reads)
	return &rtp.Packet{Header: rtp.Header{SequenceNumber: sequence, Timestamp: uint32(sequence) * 960}, Payload: payload}, nil, nil
}

func collect(t *testing.T, track *fakeTrack, interval time.Duration) ([]transcription.Chunk, error) {
	t.Helper()
	out := make(chan transcription.Chunk, 16)
	err := chunkAudio(context.Background(), track, 48000, 2, interval, func() time.Time { return track.clock }, out)
	close(out)
	var chunks []transcription.Chunk
	for chunk := range out {
		chunks = append(chunks, chunk)
	}
	return chunks, err
}

func TestChunkAudioCutsTheTrackIntoOggFiles(t *testing.T) {
	voice := []byte{0xfc, 0x01, 0x02}
	track := &fakeTrack{steps: [][]byte{voice, voice, voice, voice, voice}, clock: time.Unix(1000, 0)}

	chunks, err := collect(t, track, 2*time.Second)

	if err != io.EOF {
		t.Fatalf("err = %v, want the end of the track", err)
	}
	// Packets at seconds 1-3 fill one chunk, 4-5 are flushed when the track ends
	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want 2", len(chunks))
	}
	for _, chunk := range chunks {
		if !bytes.HasPrefix(chunk.Audio, []byte("OggS")) || chunk.ContentType != "audio/ogg" {
			t.Errorf("chunk is not an Ogg file: %q, %s", chunk.Audio[:4], chunk.ContentType)
		}
		if !chunk.EndedAt.After(chunk.StartedAt) {
			t.Errorf("chunk spans %v to %v", chunk.StartedAt, chunk.EndedAt)
		}
	}
	if !chunks[0].StartedAt.Equal(time.Unix(1001, 0)) || !chunks[0].EndedAt.Equal(time.Unix(1003, 0)) {
		t.Errorf("first chunk spans %v to %v", chunks[0].StartedAt, chunks[0].EndedAt)
	}
}

func TestChunkAudioProducesNothingWhileTheMicrophoneIsSilent(t *testing.T) {
	track := &fakeTrack{steps: [][]byte{nil, nil, {}, nil}, clock: time.Unix(1000, 0)}

	chunks, _ := collect(t, track, time.Second)

	if len(chunks) != 0 {
		t.Fatalf("got %d chunks from silence", len(chunks))
	}
}

func TestChunkAudioStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	track := &fakeTrack{steps: [][]byte{{1}}, clock: time.Unix(1000, 0)}

	if err := chunkAudio(ctx, track, 48000, 2, time.Second, time.Now, make(chan transcription.Chunk)); err != nil {
		t.Fatalf("err = %v", err)
	}
	if track.reads != 0 {
		t.Errorf("read %d packets after being cancelled", track.reads)
	}
}
