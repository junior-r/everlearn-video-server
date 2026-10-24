# Everlearn video server

## The Need

LiveKit moves audio and video, but knows nothing about classes. Something must turn what happens in a room into attendance, chat history and transcripts.

## The Goal

A small Go service that listens to LiveKit, applies the video business logic, and reports to the NestJS backend, which is the only one that writes to the database.

## The Reason

- **Webhooks for who is in a class.** Attendance and room lifecycle come from LiveKit's signed webhooks, so they work without any media connection.
- **Chat goes through this service over HTTP.** The browser posts the message with its room access token; the service takes the sender from the token, publishes the message to the room and captures it. Browsers cannot publish data themselves, so nobody can write as someone else.
- **Moderation goes through this service too.** A hand, a silenced participant (microphone and screen sound) and the end of a class are requests carrying the same room token. Whether the caller is the Teacher is read from the role the backend signed into that token, never from the request, and browsers hold no LiveKit administration of their own.
- **A shared lab is state in the room metadata, not video.** The Teacher sends the lab key and its parameters; every browser draws the same lab from them, sharp and cheap. LiveKit hands room metadata to late joiners and announces each change, so nothing is kept here. Which labs the Teacher may share is the `labs` attribute the backend signed into their token (keys separated by commas).
- **Who wrote a chat message is signed, not stored.** Each message carries an HMAC of its room, id and sender. Taking it down sends that back, so the service knows the author without keeping any state.
- **A Redis outbox between this service and the backend.** Every report is buffered in a Redis list and delivered in order, so a backend restart loses nothing. Reports the backend refuses (unknown session, invalid data) are dropped; anything else is retried.
- **Speech-to-text behind two ports.** `AudioSource` (audio of a track) and `SpeechToText` (text of a chunk) keep the pipeline independent of both LiveKit and Deepgram.
- **A hidden participant for audio, behind a switch.** Hearing what is said needs a WebRTC connection, which webhooks cannot give. The service joins the room with the LiveKit server SDK as a hidden agent that subscribes only to microphones. `ENABLE_STT` (default `false`) guards it: while off, no room is ever joined, so a network without media reachability or a missing API key cannot break a class.
- **Audio is sent to Deepgram in 6-second Ogg/Opus files.** Opus packets are copied into an Ogg container as they arrive, with no decoding. Captions lag speech by about that long; a streaming connection would lower it at the cost of a socket per speaker.

## The Implementation

- **HTTP:** `GET /healthz`; `POST /webhooks/livekit` (signature checked with the API secret); and the browser endpoints below (`Authorization: Bearer <room token>`, CORS for `ALLOWED_ORIGIN`, `401` without a valid token, `403` for a token that may not).

  | Endpoint | Body | Who |
  | --- | --- | --- |
  | `POST /chat` | `{ content }` | Any member of the class |
  | `POST /chat/delete` | `{ id, senderId, signature }` | The author, or the Teacher |
  | `POST /hand` | `{ raised }` | Any member, for their own hand; sets the `hand` attribute to when it went up (UTC, milliseconds), or clears it |
  | `POST /participants/lower-hand` | `{ identity }` | The Teacher |
  | `POST /participants/mute` | `{ identity }` | The Teacher |
  | `POST /class/end` | none | The Teacher |
  | `POST /lab` | `{ labKey, state }` | The Teacher, for a lab listed in their token |
  | `POST /lab/stop` | none | The Teacher |
- **Names:** a room is `class-<sessionId>` and a participant identity is a `Membership.id`. Rooms and participants that do not fit are ignored.
- **Packages (`internal/`):**
  - `webhook` — verifies and dispatches `room_started`, `room_finished`, `participant_joined`, `participant_left`, `track_published`, `track_unpublished`.
  - `attendance` — reports each join and leave, and adds up each member's seconds in the session.
  - `metrics` — reports when a room opens and closes.
  - `access` — who a browser request comes from, read from its room token.
  - `chat` — sending a chat message and taking one down.
  - `moderation` — hands, silencing a participant and ending a class.
  - `lab` — sharing a lab with the class, and taking it off show.
  - `transcription` — `Pipeline` (one goroutine per open microphone), `Deepgram`, `MockSpeechToText`, `TimedSource` (a timer standing in for audio).
  - `backend` — report bodies, the internal API client and the Redis outbox.
  - `sfu` — the LiveKit adapter: webhook and token verification, publishing data to a room, participant attributes, room metadata, silencing (microphone and screen sound), closing a room, and `Listener`, the `AudioSource` that joins a room as `everlearn-listener` and cuts each microphone into chunks. It leaves when the last microphone it hears closes.
  - `room`, `config` — naming rules and environment.
- **To the room:** data messages on topic `chat` (`id, senderId, senderName, content, sentAt, signature`), `chat-deleted` (`id`) and `captions` (`id, speakerId, speakerName, text, startedAt, endedAt`); the participant attribute `hand` (when it went up, empty when down); and the room metadata `{ lab: { labKey, state, sharedBy, updatedAt } | null }` (the state is a JSON object of at most 8 KiB).
- **To the backend:** `POST /internal/classroom/sessions/:id/{started|finished|participants/joined|participants/left|chat-messages|chat-messages/deleted|transcript-segments}` with `X-Internal-Secret`.
- **Config:** `LIVEKIT_URL`, `LIVEKIT_API_KEY`, `LIVEKIT_API_SECRET`, `BACKEND_INTERNAL_URL`, `INTERNAL_SERVICE_SECRET`, `REDIS_URL`, `ALLOWED_ORIGIN`, `TRANSCRIPTION_PROVIDER` (`off` default, `mock`, `deepgram`, `http`), `TRANSCRIPTION_LANGUAGE` (`es`), `DEEPGRAM_API_KEY`, `TRANSCRIPTION_HTTP_URL` and `TRANSCRIPTION_HTTP_TOKEN` (for `http`), `ENABLE_STT` (`false` default).
- **Transcription modes:**

  | `ENABLE_STT` | `TRANSCRIPTION_PROVIDER` | Result |
  | --- | --- | --- |
  | any | `off` | No captions. |
  | `false` | `mock` | Placeholder captions on a timer; no room is joined. |
  | `false` | `deepgram` | No captions (a warning is logged). |
  | `true` | `mock` | Placeholder captions, one per chunk of real audio: checks the media path without a key. |
  | `false` | `http` | No captions (a warning is logged). |
  | `true` | `deepgram` or `http` | Real captions. |
- **The `http` provider** is any speech-to-text service that speaks this contract, so a new service is a URL and a token, not new code. Avra's `/api/live/transcribe` is the first:

  ```
  POST <TRANSCRIPTION_HTTP_URL>?language=es
  Authorization: Bearer <TRANSCRIPTION_HTTP_TOKEN>
  Content-Type: audio/ogg          (the chunk: 6 s of Opus, as the browser sent it)
  → 200 {"text": "..."}            (empty text: nothing was said; any other status: logged, chunk skipped)
  ```
- **LiveKit side:** `livekit.yaml` lists this service under `webhook.urls`.

## Running

```bash
docker compose up -d --build redis livekit backend video-server   # from the workspace root
docker build --target test ./video-server                         # go vet + go test
```

## Known gaps

- **The `http` provider has run for real against Avra** on the local stack: a browser's microphone playing Spanish speech produced the expected captions and stored segments. It has not run against a production Avra over the internet.
- **Deepgram has never been called for real.** The listener is verified against the local LiveKit (it joins, subscribes and produces Ogg chunks from a browser's microphone), and the Deepgram adapter against a fake server; the two have not run together with a real key.
- The listener needs a media path to LiveKit. It works inside the local compose network; a deployment where LiveKit advertises an address the service cannot reach leaves captions empty (`microphone not transcribed` in the log) without affecting the class.
- A listener that loses its room is not reconnected beyond what the SDK retries; captions resume with the next microphone published.
- Attendance totals are kept in memory for logging; the stored truth is the backend's join and leave rows.
- Chat and hands have no rate limit. The only moderation is taking a message down.
- A shared lab stays on show if the Teacher leaves without taking it off; it ends with the room. Updates are not rate limited here: the browser sends them at most every few hundred milliseconds.
- A microphone the Teacher silenced can be opened again by its owner, and nobody can be removed from a class.
- Chat signatures are made with the LiveKit API secret: rotating it makes the messages of classes in progress impossible to take down.
- A chat message is shown before it is buffered: if Redis is down at that moment it is logged but not saved.
- The webhook API key in `livekit.yaml` is the development key; production needs its own file.
