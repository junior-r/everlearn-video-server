package sfu

import (
	"bytes"
	"context"
	"errors"
	"net"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4/pkg/media/oggwriter"

	"github.com/everlearn/video-server/internal/transcription"
)

// How long a read waits for audio before checking whether the listener was told to stop.
const readPoll = time.Second

// oggContentType is what a chunk holds: Opus packets, as the browser sent them, in an Ogg container.
const oggContentType = "audio/ogg"

// rtpTrack is the part of a subscribed WebRTC track the chunker reads.
type rtpTrack interface {
	ReadRTP() (*rtp.Packet, interceptor.Attributes, error)
	SetReadDeadline(deadline time.Time) error
}

// chunkAudio reads a track until it ends or ctx is cancelled and sends its audio to out as one
// Ogg file per interval. A muted microphone sends no packets, so silence produces no chunks.
func chunkAudio(
	ctx context.Context,
	track rtpTrack,
	sampleRate uint32,
	channels uint16,
	interval time.Duration,
	now func() time.Time,
	out chan<- transcription.Chunk,
) error {
	var (
		buffer    *bytes.Buffer
		writer    *oggwriter.OggWriter
		startedAt time.Time
	)
	flush := func() bool {
		if writer == nil {
			return true
		}
		_ = writer.Close()
		writer = nil
		chunk := transcription.Chunk{Audio: buffer.Bytes(), ContentType: oggContentType, StartedAt: startedAt, EndedAt: now()}
		select {
		case out <- chunk:
			return true
		case <-ctx.Done():
			return false
		}
	}
	for ctx.Err() == nil {
		_ = track.SetReadDeadline(time.Now().Add(readPoll))
		packet, _, err := track.ReadRTP()
		var timeout net.Error
		switch {
		case errors.As(err, &timeout) && timeout.Timeout():
		case err != nil:
			flush()
			return err
		case len(packet.Payload) > 0:
			if writer == nil {
				buffer = new(bytes.Buffer)
				if writer, err = oggwriter.NewWith(buffer, sampleRate, channels); err != nil {
					return err
				}
				startedAt = now()
			}
			if err := writer.WriteRTP(packet); err != nil {
				return err
			}
		}
		if writer != nil && now().Sub(startedAt) >= interval && !flush() {
			return nil
		}
	}
	return nil
}
