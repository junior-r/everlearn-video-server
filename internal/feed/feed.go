// Package feed keeps the recent chat and captions of each class, so a member who joins
// late, or comes back, is shown what the room already saw.
package feed

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/everlearn/video-server/internal/access"
	"github.com/everlearn/video-server/internal/room"
)

// Packet is a data message as the room received it.
type Packet struct {
	Topic   string          `json:"topic"`
	Payload json.RawMessage `json:"payload"`
}

// Stream is one list of a room's packets, kept to its newest Keep entries.
type Stream struct {
	Name string
	Keep int
}

// Chat and its deletions share a stream so they replay in the order they happened;
// captions are for following along, so only the last few are kept.
var (
	chatStream     = Stream{Name: "chat", Keep: 300}
	captionsStream = Stream{Name: "captions", Keep: 3}
	streamOf       = map[string]Stream{
		room.TopicChat:        chatStream,
		room.TopicChatDeleted: chatStream,
		room.TopicCaptions:    captionsStream,
	}
	// Streams lists every stream, in the order Recent returns them.
	Streams = []Stream{chatStream, captionsStream}
)

// Log stores the packets of each room.
type Log interface {
	Append(ctx context.Context, roomName string, stream Stream, packet Packet) error
	Recent(ctx context.Context, roomName string) ([]Packet, error)
}

// Publisher delivers a data message to everyone in a room.
type Publisher interface {
	Publish(ctx context.Context, roomName, topic string, payload []byte) error
}

// Recorder publishes data messages and keeps the ones a late joiner needs.
type Recorder struct {
	next Publisher
	log  Log
	warn *slog.Logger
}

// NewRecorder wraps a publisher so that chat and captions are also kept for replay.
func NewRecorder(next Publisher, log Log, warn *slog.Logger) *Recorder {
	return &Recorder{next: next, log: log, warn: warn}
}

// Publish delivers the message, then keeps it. The room already saw it, so failing to keep it is only logged.
func (r *Recorder) Publish(ctx context.Context, roomName, topic string, payload []byte) error {
	if err := r.next.Publish(ctx, roomName, topic, payload); err != nil {
		return err
	}
	stream, kept := streamOf[topic]
	if !kept {
		return nil
	}
	if err := r.log.Append(ctx, roomName, stream, Packet{Topic: topic, Payload: payload}); err != nil {
		r.warn.Warn("published but not kept for late joiners", "room", roomName, "topic", topic, "error", err)
	}
	return nil
}

// Handler replays a room's recent packets to a member of that room.
type Handler struct {
	auth access.Authenticator
	log  Log
	warn *slog.Logger
}

// NewHandler builds the replay endpoint.
func NewHandler(auth access.Authenticator, log Log, warn *slog.Logger) *Handler {
	return &Handler{auth: auth, log: log, warn: warn}
}

// ServeHTTP answers with the packets of the caller's own room, oldest first in each stream, or 503 when they cannot be read.
func (h *Handler) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	caller, ok := access.Identify(h.auth, w, request)
	if !ok {
		return
	}
	packets, err := h.log.Recent(request.Context(), caller.Room)
	if err != nil {
		h.warn.Warn("reading the feed of a class", "session", caller.SessionID, "error", err)
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if packets == nil {
		packets = []Packet{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Packets []Packet `json:"packets"`
	}{packets})
}
