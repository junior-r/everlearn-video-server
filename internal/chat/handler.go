// Package chat receives the messages participants send during a class, shows them to the room and captures them as history.
package chat

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/everlearn/video-server/internal/backend"
	"github.com/everlearn/video-server/internal/room"
)

const (
	maxMessageLength = 2000
	maxBodyBytes     = 16 << 10
	bearerPrefix     = "Bearer "
)

// Sender is who a room access token says its holder is.
type Sender struct {
	Room     string
	Identity string
	Name     string
}

// Authenticator checks a room access token issued by the backend.
type Authenticator interface {
	Authenticate(token string) (Sender, error)
}

// Publisher delivers a data message to everyone in a room.
type Publisher interface {
	Publish(ctx context.Context, roomName, topic string, payload []byte) error
}

// Reporter buffers a report for the backend.
type Reporter interface {
	Enqueue(ctx context.Context, report backend.Report) error
}

// Message is a chat message as the room receives it.
type Message struct {
	ID         string    `json:"id"`
	SenderID   string    `json:"senderId"`
	SenderName string    `json:"senderName"`
	Content    string    `json:"content"`
	SentAt     time.Time `json:"sentAt"`
}

// Handler accepts chat messages from browsers. The sender is always taken from the room
// access token, never from the request body, so nobody can write as someone else.
type Handler struct {
	auth      Authenticator
	publisher Publisher
	reports   Reporter
	log       *slog.Logger
	now       func() time.Time
}

// NewHandler builds the chat endpoint.
func NewHandler(auth Authenticator, publisher Publisher, reports Reporter, log *slog.Logger) *Handler {
	return &Handler{auth: auth, publisher: publisher, reports: reports, log: log, now: time.Now}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	token, found := strings.CutPrefix(request.Header.Get("Authorization"), bearerPrefix)
	if !found {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	sender, err := h.auth.Authenticate(token)
	if err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	sessionID, ok := room.SessionID(sender.Room)
	if !ok || !room.IsMembership(sender.Identity) {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	var body struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, request.Body, maxBodyBytes)).Decode(&body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	content := strings.TrimSpace(body.Content)
	if content == "" || utf8.RuneCountInString(content) > maxMessageLength {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	message := Message{
		ID:         uuid.Must(uuid.NewV7()).String(),
		SenderID:   sender.Identity,
		SenderName: sender.Name,
		Content:    content,
		SentAt:     h.now().UTC().Truncate(time.Millisecond),
	}
	payload, err := json.Marshal(message)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if err := h.publisher.Publish(request.Context(), sender.Room, room.TopicChat, payload); err != nil {
		h.log.Error("publishing a chat message", "session", sessionID, "error", err)
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	// The room already saw the message, so a failure to buffer it is logged rather than returned
	report := backend.ChatMessage(sessionID, message.ID, message.SenderID, message.Content, message.SentAt)
	if err := h.reports.Enqueue(request.Context(), report); err != nil {
		h.log.Error("chat message shown but not saved", "session", sessionID, "message", message.ID, "error", err)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write(payload)
}
