package config

import (
	"strings"
	"testing"
)

func env(overrides map[string]string) func(string) string {
	values := map[string]string{
		"LIVEKIT_URL":             "ws://livekit:7880",
		"LIVEKIT_API_KEY":         "devkey",
		"LIVEKIT_API_SECRET":      "devsecret",
		"BACKEND_INTERNAL_URL":    "http://backend:4000/",
		"INTERNAL_SERVICE_SECRET": "shared",
		"REDIS_URL":               "redis://redis:6379",
	}
	for key, value := range overrides {
		values[key] = value
	}
	return func(key string) string { return values[key] }
}

func TestLoadAppliesDefaultsAndNormalisesAddresses(t *testing.T) {
	cfg, err := Load(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != "8080" || cfg.LiveKitURL != "http://livekit:7880" || cfg.BackendURL != "http://backend:4000" {
		t.Fatalf("cfg = %+v", cfg)
	}
	if cfg.TranscriptionProvider != TranscriptionOff || cfg.TranscriptionLanguage != "es" || cfg.AllowedOrigin != "http://localhost:3000" {
		t.Fatalf("cfg = %+v", cfg)
	}
	secure, err := Load(env(map[string]string{"LIVEKIT_URL": "wss://video.example.com"}))
	if err != nil || secure.LiveKitURL != "https://video.example.com" {
		t.Fatalf("LiveKitURL = %q, err = %v", secure.LiveKitURL, err)
	}
}

func TestLoadNamesEveryMissingVariable(t *testing.T) {
	_, err := Load(env(map[string]string{"LIVEKIT_API_SECRET": "", "REDIS_URL": ""}))
	if err == nil || !strings.Contains(err.Error(), "LIVEKIT_API_SECRET") || !strings.Contains(err.Error(), "REDIS_URL") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadChecksTheTranscriptionProvider(t *testing.T) {
	if _, err := Load(env(map[string]string{"TRANSCRIPTION_PROVIDER": "deepgram"})); err == nil {
		t.Error("deepgram without a key was accepted")
	}
	if _, err := Load(env(map[string]string{"TRANSCRIPTION_PROVIDER": "whisper"})); err == nil {
		t.Error("an unknown provider was accepted")
	}
	cfg, err := Load(env(map[string]string{"TRANSCRIPTION_PROVIDER": "deepgram", "DEEPGRAM_API_KEY": "dg"}))
	if err != nil || cfg.DeepgramAPIKey != "dg" {
		t.Errorf("cfg = %+v, err = %v", cfg, err)
	}
}
