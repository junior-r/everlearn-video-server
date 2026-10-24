package transcription

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// HTTPSpeechToText transcribes chunks with any service that speaks this contract:
//
//	POST <endpoint>?language=<code>   Authorization: Bearer <token>   Content-Type: <the chunk's type>
//	body: the chunk's audio   →   200 {"text": "..."}
//
// Avra's /api/live/transcribe is one such service; another is a new URL and token, not new code.
type HTTPSpeechToText struct {
	endpoint string
	token    string
	http     *http.Client
}

// NewHTTPSpeechToText builds the adapter for the service at endpoint, authenticated with token.
func NewHTTPSpeechToText(endpoint, token string) *HTTPSpeechToText {
	return &HTTPSpeechToText{endpoint: endpoint, token: token, http: &http.Client{Timeout: 20 * time.Second}}
}

type httpSpeechAnswer struct {
	Text *string `json:"text"`
}

func (h *HTTPSpeechToText) Transcribe(ctx context.Context, chunk Chunk, language string) (string, error) {
	if len(chunk.Audio) == 0 {
		return "", nil
	}
	target, err := url.Parse(h.endpoint)
	if err != nil {
		return "", fmt.Errorf("speech-to-text URL: %w", err)
	}
	query := target.Query()
	query.Set("language", language)
	target.RawQuery = query.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(chunk.Audio))
	if err != nil {
		return "", err
	}
	request.Header.Set("Authorization", "Bearer "+h.token)
	request.Header.Set("Content-Type", chunk.ContentType)

	response, err := h.http.Do(request)
	if err != nil {
		return "", fmt.Errorf("reaching the speech-to-text service: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		// The service's own message says why (a revoked token, an engine that is down); it is only logged
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return "", fmt.Errorf("speech-to-text service answered %d: %s", response.StatusCode, strings.TrimSpace(string(detail)))
	}
	var answer httpSpeechAnswer
	if err := json.NewDecoder(response.Body).Decode(&answer); err != nil || answer.Text == nil {
		return "", fmt.Errorf("reading the speech-to-text answer: missing text")
	}
	return strings.TrimSpace(*answer.Text), nil
}
