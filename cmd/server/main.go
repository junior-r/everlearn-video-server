// Command server is the entrypoint of the Everlearn video controller.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/everlearn/video-server/internal/attendance"
	"github.com/everlearn/video-server/internal/backend"
	"github.com/everlearn/video-server/internal/chat"
	"github.com/everlearn/video-server/internal/config"
	"github.com/everlearn/video-server/internal/metrics"
	"github.com/everlearn/video-server/internal/moderation"
	"github.com/everlearn/video-server/internal/sfu"
	"github.com/everlearn/video-server/internal/transcription"
	"github.com/everlearn/video-server/internal/webhook"
)

// How much audio is gathered before each transcription, which is also how far captions lag behind speech.
const chunkInterval = 6 * time.Second

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("video-server stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	redisOptions, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return err
	}
	redisClient := redis.NewClient(redisOptions)
	defer redisClient.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	outbox := backend.NewOutbox(
		backend.NewRedisQueue(redisClient),
		backend.NewClient(cfg.BackendURL, cfg.InternalSecret),
		logger,
	)
	outboxDone := make(chan struct{})
	go func() {
		defer close(outboxDone)
		outbox.Run(ctx)
	}()

	livekit := sfu.NewClient(cfg.LiveKitURL, cfg.LiveKitAPIKey, cfg.LiveKitAPISecret)
	captions := newCaptions(livekit, outbox, logger)
	pipeline := newPipeline(cfg, livekit, captions.publish, logger)
	classes := &classroom{
		rooms:      metrics.NewRooms(outbox),
		attendance: attendance.NewRecorder(attendance.NewTracker(), outbox, logger),
		transcript: pipeline,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.Handle("POST /webhooks/livekit", webhook.NewHandler(livekit.ReceiveWebhook, classes, logger))
	messages := chat.NewHandler(livekit, livekit, outbox, []byte(cfg.LiveKitAPISecret), logger)
	order := moderation.NewHandler(livekit, livekit, logger)
	for path, handler := range map[string]http.HandlerFunc{
		"/chat":                    messages.ServeHTTP,
		"/chat/delete":             messages.Delete,
		"/hand":                    order.Hand,
		"/participants/lower-hand": order.LowerHand,
		"/participants/mute":       order.Mute,
		"/class/end":               order.End,
	} {
		mux.Handle(path, browserAccess(cfg.AllowedOrigin, handler))
	}

	server := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logger.Info("video-server listening", "port", cfg.Port, "transcription", cfg.TranscriptionProvider, "stt", cfg.EnableSTT)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server failed", "error", err)
			stop()
		}
	}()

	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown failed", "error", err)
	}
	if pipeline != nil {
		pipeline.Close()
	}
	<-outboxDone
	return nil
}

// newPipeline chooses the audio source and the speech-to-text adapter from configuration.
// It returns nil when there is nothing to transcribe with.
func newPipeline(cfg config.Config, livekit *sfu.Client, sink transcription.Sink, logger *slog.Logger) *transcription.Pipeline {
	var stt transcription.SpeechToText
	switch cfg.TranscriptionProvider {
	case config.TranscriptionMock:
		stt = &transcription.MockSpeechToText{}
	case config.TranscriptionDeepgram:
		if !cfg.EnableSTT {
			logger.Warn("transcription is off: Deepgram needs real audio, which needs ENABLE_STT=true")
			return nil
		}
		stt = transcription.NewDeepgram(transcription.DeepgramURL, cfg.DeepgramAPIKey)
	default:
		return nil
	}
	// Without ENABLE_STT no room is ever joined: the mock is fed by a timer instead of audio
	var source transcription.AudioSource = transcription.TimedSource{Interval: chunkInterval}
	if cfg.EnableSTT {
		source = sfu.NewListener(livekit, cfg.LiveKitRoomURL, chunkInterval, logger)
	}
	return transcription.NewPipeline(source, stt, sink, cfg.TranscriptionLanguage, logger)
}

// browserAccess lets the frontend origin call a handler from a browser, and only with POST.
func browserAccess(origin string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
		switch request.Method {
		case http.MethodOptions:
			w.Header().Set("Access-Control-Allow-Methods", "POST")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
		case http.MethodPost:
			next.ServeHTTP(w, request)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
}
