// Package chat receives the messages participants send during a class, shows them to the room and captures them as history.
package chat

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/everlearn/video-server/internal/access"
	"github.com/everlearn/video-server/internal/backend"
	"github.com/everlearn/video-server/internal/room"
)

const maxMessageLength = 2000

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
	// Signature proves this service published the message for that sender; taking it down asks for it back.
	Signature string `json:"signature"`
}

// Handler accepts chat messages from browsers. The sender is always taken from the room
// access token, never from the request body, so nobody can write as someone else.
type Handler struct {
	auth      access.Authenticator
	publisher Publisher
	reports   Reporter
	key       []byte
	log       *slog.Logger
	now       func() time.Time
}

// NewHandler builds the chat endpoints. key signs who wrote each message.
func NewHandler(auth access.Authenticator, publisher Publisher, reports Reporter, key []byte, log *slog.Logger) *Handler {
	return &Handler{auth: auth, publisher: publisher, reports: reports, key: key, log: log, now: time.Now}
}

// sign ties a message to its room and its sender, so no state is kept to know who wrote what.
func (h *Handler) sign(roomName, messageID, senderID string) string {
	mac := hmac.New(sha256.New, h.key)
	mac.Write([]byte("chat-author\n" + roomName + "\n" + messageID + "\n" + senderID))
	return hex.EncodeToString(mac.Sum(nil))
}

// ServeHTTP shows a message to the room and captures it.
func (h *Handler) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	sender, ok := access.Identify(h.auth, w, request)
	if !ok {
		return
	}
	var body struct {
		Content string `json:"content"`
	}
	if !access.Decode(w, request, &body) {
		return
	}
	content := strings.TrimSpace(body.Content)
	if content == "" || utf8.RuneCountInString(content) > maxMessageLength {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	id := uuid.Must(uuid.NewV7()).String()
	message := Message{
		ID:         id,
		SenderID:   sender.Identity,
		SenderName: sender.Name,
		Content:    content,
		SentAt:     h.now().UTC().Truncate(time.Millisecond),
		Signature:  h.sign(sender.Room, id, sender.Identity),
	}
	payload, err := json.Marshal(message)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if err := h.publisher.Publish(request.Context(), sender.Room, room.TopicChat, payload); err != nil {
		h.log.Error("publishing a chat message", "session", sender.SessionID, "error", err)
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	// The room already saw the message, so a failure to buffer it is logged rather than returned
	report := backend.ChatMessage(sender.SessionID, message.ID, message.SenderID, message.Content, message.SentAt)
	if err := h.reports.Enqueue(request.Context(), report); err != nil {
		h.log.Error("chat message shown but not saved", "session", sender.SessionID, "message", message.ID, "error", err)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write(payload)
}

// Delete takes a message down for the whole room. Only its author and the Teacher of the class may.
func (h *Handler) Delete(w http.ResponseWriter, request *http.Request) {
	caller, ok := access.Identify(h.auth, w, request)
	if !ok {
		return
	}
	var body struct {
		ID        string `json:"id"`
		SenderID  string `json:"senderId"`
		Signature string `json:"signature"`
	}
	if !access.Decode(w, request, &body) {
		return
	}
	if uuid.Validate(body.ID) != nil || !room.IsMembership(body.SenderID) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	// A message of another room, or one never sent, cannot be told from a forged one
	expected := h.sign(caller.Room, body.ID, body.SenderID)
	if !hmac.Equal([]byte(expected), []byte(body.Signature)) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if !caller.Teacher && caller.Identity != body.SenderID {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	payload, err := json.Marshal(map[string]string{"id": body.ID})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if err := h.publisher.Publish(request.Context(), caller.Room, room.TopicChatDeleted, payload); err != nil {
		h.log.Error("taking down a chat message", "session", caller.SessionID, "error", err)
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	report := backend.ChatMessageDeleted(caller.SessionID, body.ID, caller.Identity, h.now())
	if err := h.reports.Enqueue(request.Context(), report); err != nil {
		h.log.Error("chat message hidden but still stored", "session", caller.SessionID, "message", body.ID, "error", err)
	}
	w.WriteHeader(http.StatusNoContent)
}
