# SceneSense — Contextual Ad Lab

An AI-first contextual ad placement MVP for Bengali video. This repository is being built in gated phases so each increment stays runnable and demonstrable.

## Current release

**Phases 0–3 are deployed and end-to-end tested.** Go handles uploads, jobs, and deterministic policy. The Python worker supports Bengali ASR via Sarvam Saaras v4 or Groq Whisper, detects low-audio intervals and shot cuts, and asks OpenAI for structured scene evidence and break naturalness scores. A Go policy then rejects breaks during speech, uncertain or sensitive scenes, and unfinished thoughts, and enforces pacing and ad-load limits. The UI shows accepted and withheld moments with seekable markers and exact reasons; a saved analysis can be opened with `?job=<id>`. Phase 3 reuses previous evidence when available, so adding break scoring need not require another ASR pass on the same source. The hosted positive selection was verified on a constructed two-scene test fixture; the tested unmodified real excerpts had no eligible break and were correctly withheld. Brand matching, VMAP, and ad playback remain ahead.

## Run locally

Requirements: Go 1.25+, Python 3.10+, and FFmpeg/ffprobe on `PATH`.

```sh
make run
```

`make run` loads the ignored `.env` file in the project root and starts the Go app. Set `ASR_PROVIDER=groq` (default) with `GROQ_API_KEY`, or `ASR_PROVIDER=sarvam` with `SARVAM_API_KEY` for Saaras v4 Bengali transcription. Sarvam REST requests use mono 16 kHz WAV chunks of at most 25 seconds; its transcript timestamps are phrase-level. Set `OPENAI_API_KEY` for scene understanding and break scoring; optionally set `OPENAI_VISION_MODEL` and `OPENAI_BREAK_MODEL` (both default to `gpt-4o-mini`). Never commit or share these keys. Set `DEMO_ACCESS_PASSWORD` to enable the demo's HTTP Basic Auth gate (username `demo`); Railway deployments refuse to start if it is missing. `/healthz` remains available to the platform healthcheck. Without the selected ASR provider's key, uploads stop after media intake. Without OpenAI, successful transcripts are retained and the UI reports that scene analysis is unavailable. Open [http://localhost:8080](http://localhost:8080) and upload an MP4 with an audio track. Videos and private JSON job artifacts are kept in `data/uploads`.

Set `ADDR` to change the listen address and `UPLOAD_DIR` to change the upload directory.

## Checks

```sh
gofmt -w *.go
go vet ./...
go test ./...
python3 -m unittest discover -s ai -p 'test_*.py'
go build ./...
```

The end-to-end media intake test uses FFmpeg to generate a short video fixture. It skips when FFmpeg or ffprobe is unavailable; the container includes both tools.

## Container

```sh
docker build -t contextual-ad-lab:local .
docker run --rm -p 8080:8080 -v contextual-ad-data:/app/data contextual-ad-lab:local
```

The container includes Go, Python 3, and FFmpeg. Set the selected ASR provider and its key (`ASR_PROVIDER=sarvam` with `SARVAM_API_KEY`, or the Groq defaults) plus `OPENAI_API_KEY` as runtime environment variables (never Docker build arguments); the worker inherits them. Optionally set `SARVAM_ASR_MODEL` or `OPENAI_VISION_MODEL`. The container listens on port `8080`. Mount persistent storage at `/app/data` for uploaded videos and analysis sidecars.

## API in this phase

- `GET /healthz` — service health
- `GET /api/jobs` — saved and current jobs
- `POST /api/jobs` — multipart form upload with field name `video`
- `GET /api/jobs/{id}` — intake metadata and job state
- `GET /media/{id}` — source video playback

Job metadata, transcript, scene evidence, and pause intervals are atomically stored as private `.job.json` sidecars alongside the uploaded videos. Interrupted jobs restore as retryable failures; active inference is not resumed automatically.

Phase 3 policy uses a 15-second planning ad, at least 15 seconds of programme before a break, at least 10 seconds after, a 180-second gap between selected breaks, at most 4 breaks per hour (rounded up for shorter clips), and at most 20% planned ad load. The Go policy also requires a verified low-audio pause, no overlapping speech, supported scene context on both sides, and passing AI scores. A break is withheld when evidence is missing or uncertain. Once actual creative durations exist in Phase 4–5, the policy must be rechecked using each selected creative's duration before playback.

## Project notes

- [Problem statement](Statements/problem.md)
- [Phased implementation plan](Docs/PROBLEM1-MVP-IMPLEMENTATION-PLAN.md)
- [Implementation status and release gates](Docs/IMPLEMENTATION-STATUS.md)
- [Deferred UI redesign direction](Docs/UI-REDESIGN-ROADMAP.md)
