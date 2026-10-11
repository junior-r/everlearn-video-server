# Everlearn video server

## The Need

LiveKit moves audio and video, but knows nothing about classes. Something must turn what happens in a room into attendance, chat history and transcripts.

## The Goal

A small Go service that listens to LiveKit, applies the video business logic, and reports to the NestJS backend, which is the only one that writes to the database.

## The Reason

- **Webhooks for who is in a class.** Attendance and room lifecycle come from LiveKit's signed webhooks, so they work without any media connection.
- **Chat goes through this service over HTTP.** The browser posts the message with its room access token; the service takes the sender from the token, publishes the message to the room and captures it. Browsers cannot publish data themselves, so nobody can write as someone else.
- **A Redis outbox between this service and the backend.** Every report is buffered in a Redis list and delivered in order, so a backend restart loses nothing. Reports the backend refuses (unknown session, invalid data) are dropped; anything else is retried.
- **Speech-to-text behind two ports.** `AudioSource` (audio of a track) and `SpeechToText` (text of a chunk) keep the pipeline independent of both LiveKit and Deepgram.
- **A hidden participant for audio, behind a switch.** Hearing what is said needs a WebRTC connection, which webhooks cannot give. The service joins the room with the LiveKit server SDK as a hidden agent that subscribes only to microphones. `ENABLE_STT` (default `false`) guards it: while off, no room is ever joined, so a network without media reachability or a missing API key cannot break a class.
- **Audio is sent to Deepgram in 6-second Ogg/Opus files.** Opus packets are copied into an Ogg container as they arrive, with no decoding. Captions lag speech by about that long; a streaming connection would lower it at the cost of a socket per speaker.

## The Implementation

- **HTTP:** `GET /healthz`; `POST /webhooks/livekit` (signature checked with the API secret); `POST /chat` (`Authorization: Bearer <room token>`, body `{ "content" }`, CORS for `ALLOWED_ORIGIN`).
- **Names:** a room is `class-<sessionId>` and a participant identity is a `Membership.id`. Rooms and participants that do not fit are ignored.
- **Packages (`internal/`):**
  - `webhook` — verifies and dispatches `room_started`, `room_finished`, `participant_joined`, `participant_left`, `track_published`, `track_unpublished`.
  - `attendance` — reports each join and leave, and adds up each member's seconds in the session.
  - `metrics` — reports when a room opens and closes.
  - `chat` — the chat endpoint.
  - `transcription` — `Pipeline` (one goroutine per open microphone), `Deepgram`, `MockSpeechToText`, `TimedSource` (a timer standing in for audio).
  - `backend` — report bodies, the internal API client and the Redis outbox.
  - `sfu` — the LiveKit adapter: webhook and token verification, publishing data to a room, and `Listener`, the `AudioSource` that joins a room as `everlearn-listener` and cuts each microphone into chunks. It leaves when the last microphone it hears closes.
  - `room`, `config` — naming rules and environment.
- **To the room:** data messages on topic `chat` (`id, senderId, senderName, content, sentAt`) and `captions` (`id, speakerId, speakerName, text, startedAt, endedAt`).
- **To the backend:** `POST /internal/classroom/sessions/:id/{started|finished|participants/joined|participants/left|chat-messages|transcript-segments}` with `X-Internal-Secret`.
- **Config:** `LIVEKIT_URL`, `LIVEKIT_API_KEY`, `LIVEKIT_API_SECRET`, `BACKEND_INTERNAL_URL`, `INTERNAL_SERVICE_SECRET`, `REDIS_URL`, `ALLOWED_ORIGIN`, `TRANSCRIPTION_PROVIDER` (`off` default, `mock`, `deepgram`), `TRANSCRIPTION_LANGUAGE` (`es`), `DEEPGRAM_API_KEY`, `ENABLE_STT` (`false` default).
- **Transcription modes:**

  | `ENABLE_STT` | `TRANSCRIPTION_PROVIDER` | Result |
  | --- | --- | --- |
  | any | `off` | No captions. |
  | `false` | `mock` | Placeholder captions on a timer; no room is joined. |
  | `false` | `deepgram` | No captions (a warning is logged). |
  | `true` | `mock` | Placeholder captions, one per chunk of real audio: checks the media path without a key. |
  | `true` | `deepgram` | Real captions. |
- **LiveKit side:** `livekit.yaml` lists this service under `webhook.urls`.

## Running

```bash
docker compose up -d --build redis livekit backend video-server   # from the workspace root
docker build --target test ./video-server                         # go vet + go test
```

## Known gaps

- **Deepgram has never been called for real.** The listener is verified against the local LiveKit (it joins, subscribes and produces Ogg chunks from a browser's microphone), and the Deepgram adapter against a fake server; the two have not run together with a real key.
- The listener needs a media path to LiveKit. It works inside the local compose network; a deployment where LiveKit advertises an address the service cannot reach leaves captions empty (`microphone not transcribed` in the log) without affecting the class.
- A listener that loses its room is not reconnected beyond what the SDK retries; captions resume with the next microphone published.
- Attendance totals are kept in memory for logging; the stored truth is the backend's join and leave rows.
- Chat has no rate limit and no moderation.
- A chat message is shown before it is buffered: if Redis is down at that moment it is logged but not saved.
- The webhook API key in `livekit.yaml` is the development key; production needs its own file.
