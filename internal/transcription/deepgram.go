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

// DeepgramURL is Deepgram's speech-to-text endpoint for a complete piece of audio.
const DeepgramURL = "https://api.deepgram.com/v1/listen"

const deepgramModel = "nova-3"

// Deepgram transcribes chunks with the Deepgram API.
type Deepgram struct {
	endpoint string
	apiKey   string
	http     *http.Client
}

// NewDeepgram builds the adapter. endpoint is DeepgramURL outside tests.
func NewDeepgram(endpoint, apiKey string) *Deepgram {
	return &Deepgram{endpoint: endpoint, apiKey: apiKey, http: &http.Client{Timeout: 20 * time.Second}}
}

type deepgramResponse struct {
	Results struct {
		Channels []struct {
			Alternatives []struct {
				Transcript string `json:"transcript"`
			} `json:"alternatives"`
		} `json:"channels"`
	} `json:"results"`
}

func (d *Deepgram) Transcribe(ctx context.Context, chunk Chunk, language string) (string, error) {
	if len(chunk.Audio) == 0 {
		return "", nil
	}
	query := url.Values{"model": {deepgramModel}, "language": {language}, "smart_format": {"true"}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, d.endpoint+"?"+query.Encode(), bytes.NewReader(chunk.Audio))
	if err != nil {
		return "", err
	}
	request.Header.Set("Authorization", "Token "+d.apiKey)
	request.Header.Set("Content-Type", chunk.ContentType)

	response, err := d.http.Do(request)
	if err != nil {
		return "", fmt.Errorf("reaching Deepgram: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, response.Body)
		return "", fmt.Errorf("Deepgram answered status %d", response.StatusCode)
	}
	var body deepgramResponse
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("reading Deepgram's answer: %w", err)
	}
	if len(body.Results.Channels) == 0 || len(body.Results.Channels[0].Alternatives) == 0 {
		return "", nil
	}
	return strings.TrimSpace(body.Results.Channels[0].Alternatives[0].Transcript), nil
}
