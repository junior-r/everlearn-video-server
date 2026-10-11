// Package config loads and validates the service configuration from the environment.
package config

import (
	"fmt"
	"strings"
)

// Transcription providers.
const (
	TranscriptionOff      = "off"
	TranscriptionMock     = "mock"
	TranscriptionDeepgram = "deepgram"
)

// Config is everything the service needs from its environment.
type Config struct {
	Port string
	// LiveKitURL is the HTTP address of the LiveKit server API.
	LiveKitURL       string
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
		Port:                  optional("PORT", "8080"),
		LiveKitURL:            httpURL(required("LIVEKIT_URL")),
		LiveKitAPIKey:         required("LIVEKIT_API_KEY"),
		LiveKitAPISecret:      required("LIVEKIT_API_SECRET"),
		BackendURL:            strings.TrimRight(required("BACKEND_INTERNAL_URL"), "/"),
		InternalSecret:        required("INTERNAL_SERVICE_SECRET"),
		RedisURL:              required("REDIS_URL"),
		AllowedOrigin:         optional("ALLOWED_ORIGIN", "http://localhost:3000"),
		TranscriptionProvider: optional("TRANSCRIPTION_PROVIDER", TranscriptionOff),
		TranscriptionLanguage: optional("TRANSCRIPTION_LANGUAGE", "es"),
		DeepgramAPIKey:        getenv("DEEPGRAM_API_KEY"),
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}
	switch cfg.TranscriptionProvider {
	case TranscriptionOff, TranscriptionMock:
	case TranscriptionDeepgram:
		if cfg.DeepgramAPIKey == "" {
			return Config{}, fmt.Errorf("TRANSCRIPTION_PROVIDER=deepgram requires DEEPGRAM_API_KEY")
		}
	default:
		return Config{}, fmt.Errorf("TRANSCRIPTION_PROVIDER must be off, mock or deepgram, got %q", cfg.TranscriptionProvider)
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
