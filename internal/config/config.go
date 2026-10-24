// Package config loads and validates the service configuration from the environment.
package config

import (
	"fmt"
	"net/url"
	"strings"
)

// Transcription providers.
const (
	TranscriptionOff      = "off"
	TranscriptionMock     = "mock"
	TranscriptionDeepgram = "deepgram"
	// TranscriptionHTTP is any service speaking the contract in transcription.HTTPSpeechToText (Avra, for one).
	TranscriptionHTTP = "http"
)

// Config is everything the service needs from its environment.
type Config struct {
	Port string
	// LiveKitURL is the HTTP address of the LiveKit server API.
	LiveKitURL string
	// LiveKitRoomURL is the address participants connect to, as given in LIVEKIT_URL.
	LiveKitRoomURL   string
	LiveKitAPIKey    string
	LiveKitAPISecret string
	BackendURL       string
	InternalSecret   string
	RedisURL         string
	// AllowedOrigin is the frontend origin that may post chat messages from a browser.
	AllowedOrigin         string
	TranscriptionProvider string
	TranscriptionLanguage string
	DeepgramAPIKey        string
	// TranscriptionHTTPURL and TranscriptionHTTPToken reach the service behind TRANSCRIPTION_PROVIDER=http.
	TranscriptionHTTPURL   string
	TranscriptionHTTPToken string
	// EnableSTT lets the service join rooms as a hidden participant to hear real audio.
	EnableSTT bool
}

// Load reads the configuration through getenv and fails on a missing or inconsistent value.
func Load(getenv func(string) string) (Config, error) {
	var missing []string
	required := func(key string) string {
		value := getenv(key)
		if value == "" {
			missing = append(missing, key)
		}
		return value
	}
	optional := func(key, fallback string) string {
		if value := getenv(key); value != "" {
			return value
		}
		return fallback
	}

	cfg := Config{
		Port:                   optional("PORT", "8080"),
		LiveKitURL:             httpURL(required("LIVEKIT_URL")),
		LiveKitRoomURL:         getenv("LIVEKIT_URL"),
		LiveKitAPIKey:          required("LIVEKIT_API_KEY"),
		LiveKitAPISecret:       required("LIVEKIT_API_SECRET"),
		BackendURL:             strings.TrimRight(required("BACKEND_INTERNAL_URL"), "/"),
		InternalSecret:         required("INTERNAL_SERVICE_SECRET"),
		RedisURL:               required("REDIS_URL"),
		AllowedOrigin:          optional("ALLOWED_ORIGIN", "http://localhost:3000"),
		TranscriptionProvider:  optional("TRANSCRIPTION_PROVIDER", TranscriptionOff),
		TranscriptionLanguage:  optional("TRANSCRIPTION_LANGUAGE", "es"),
		DeepgramAPIKey:         getenv("DEEPGRAM_API_KEY"),
		TranscriptionHTTPURL:   getenv("TRANSCRIPTION_HTTP_URL"),
		TranscriptionHTTPToken: getenv("TRANSCRIPTION_HTTP_TOKEN"),
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}
	switch strings.ToLower(optional("ENABLE_STT", "false")) {
	case "true", "1":
		cfg.EnableSTT = true
	case "false", "0":
	default:
		return Config{}, fmt.Errorf("ENABLE_STT must be true or false, got %q", getenv("ENABLE_STT"))
	}
	switch cfg.TranscriptionProvider {
	case TranscriptionOff, TranscriptionMock:
	case TranscriptionDeepgram:
		if cfg.DeepgramAPIKey == "" {
			return Config{}, fmt.Errorf("TRANSCRIPTION_PROVIDER=deepgram requires DEEPGRAM_API_KEY")
		}
	case TranscriptionHTTP:
		if cfg.TranscriptionHTTPURL == "" || cfg.TranscriptionHTTPToken == "" {
			return Config{}, fmt.Errorf("TRANSCRIPTION_PROVIDER=http requires TRANSCRIPTION_HTTP_URL and TRANSCRIPTION_HTTP_TOKEN")
		}
		if target, err := url.Parse(cfg.TranscriptionHTTPURL); err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
			return Config{}, fmt.Errorf("TRANSCRIPTION_HTTP_URL must be an http(s) URL, got %q", cfg.TranscriptionHTTPURL)
		}
	default:
		return Config{}, fmt.Errorf("TRANSCRIPTION_PROVIDER must be off, mock, deepgram or http, got %q", cfg.TranscriptionProvider)
	}
	return cfg, nil
}

// httpURL turns the WebSocket address clients use into the HTTP address of the same server.
func httpURL(address string) string {
	if rest, ok := strings.CutPrefix(address, "wss://"); ok {
		return "https://" + rest
	}
	if rest, ok := strings.CutPrefix(address, "ws://"); ok {
		return "http://" + rest
	}
	return address
}
