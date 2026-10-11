# Everlearn video server

## The Need

LiveKit moves audio and video, but knows nothing about classes. Something must turn what happens in a room into attendance, chat history and transcripts.

## The Goal

A small Go service that listens to LiveKit, applies the video business logic, and reports to the NestJS backend, which is the only one that writes to the database.

## The Reason

- **Webhooks, not a bot in the room.** Attendance and room lifecycle come from LiveKit's signed webhooks, so the service needs no WebRTC connection of its own to know who is in a class.
- **Chat goes through this service over HTTP.** The browser posts the message with its room access token; the service takes the sender from the token, publishes the message to the room and captures it. Browsers cannot publish data themselves, so nobody can write as someone else.
- **A Redis outbox between this service and the backend.** Every report is buffered in a Redis list and delivered in order, so a backend restart loses nothing. Reports the backend refuses (unknown session, invalid data) are dropped; anything else is retried.
- **Speech-to-text behind two ports.** `AudioSource` (audio of a track) and `SpeechToText` (text of a chunk) keep the pipeline independent of both LiveKit and Deepgram.
- **`livekit/protocol` only.** It verifies webhooks and tokens and calls the room API; the heavier server SDK is not needed.

## The Implementation

- **HTTP:** `GET /healthz`; `POST /webhooks/livekit` (signature checked with the API secret); `POST /chat` (`Authorization: Bearer <room token>`, body `{ "content" }`, CORS for `ALLOWED_ORIGIN`).
- **Names:** a room is `class-<sessionId>` and a participant identity is a `Membership.id`. Rooms and participants that do not fit are ignored.
- **Packages (`internal/`):**
  - `webhook` — verifies and dispatches `room_started`, `room_finished`, `participant_joined`, `participant_left`, `track_published`, `track_unpublished`.
  - `attendance` — reports each join and leave, and adds up each member's seconds in the session.
  - `metrics` — reports when a room opens and closes.
  - `chat` — the chat endpoint.
  - `transcription` — `Pipeline` (one goroutine per open microphone), `Deepgram`, `MockSpeechToText`, `TimedSource`.
  - `backend` — report bodies, the internal API client and the Redis outbox.
  - `sfu` — the LiveKit adapter: webhook and token verification, publishing data to a room.
  - `room`, `config` — naming rules and environment.
- **To the room:** data messages on topic `chat` (`id, senderId, senderName, content, sentAt`) and `captions` (`id, speakerId, speakerName, text, startedAt, endedAt`).
- **To the backend:** `POST /internal/classroom/sessions/:id/{started|finished|participants/joined|participants/left|chat-messages|transcript-segments}` with `X-Internal-Secret`.
- **Config:** `LIVEKIT_URL`, `LIVEKIT_API_KEY`, `LIVEKIT_API_SECRET`, `BACKEND_INTERNAL_URL`, `INTERNAL_SERVICE_SECRET`, `REDIS_URL`, `ALLOWED_ORIGIN`, `TRANSCRIPTION_PROVIDER` (`off` default, `mock`, `deepgram`), `TRANSCRIPTION_LANGUAGE` (`es`), `DEEPGRAM_API_KEY`.
- **LiveKit side:** `livekit.yaml` lists this service under `webhook.urls`.

## Running

```bash
docker compose up -d --build redis livekit backend video-server   # from the workspace root
docker build --target test ./video-server                         # go vet + go test
```

## Known gaps

- **No real audio reaches speech-to-text yet.** The only `AudioSource` is `TimedSource`, which emits empty chunks on a timer. With `mock` this yields placeholder captions ("Transcripción simulada n.º 1") while a microphone is published, muted or not. With `deepgram` the adapter is wired but receives no audio, so nothing is transcribed. The missing piece is an `AudioSource` that subscribes to the room's audio tracks.
- The Deepgram adapter is tested against a fake server only, never against the real API.
- Attendance totals are kept in memory for logging; the stored truth is the backend's join and leave rows.
- Chat has no rate limit and no moderation.
- A chat message is shown before it is buffered: if Redis is down at that moment it is logged but not saved.
- The webhook API key in `livekit.yaml` is the development key; production needs its own file.
