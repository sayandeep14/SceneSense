# SceneSense — Contextual Ad Lab

An AI-first contextual ad placement MVP for Bengali video. This repository is being built in gated phases so each increment stays runnable and demonstrable.

## Current release

**Phase 0 complete; Phase 1 complete; Phase 2 evidence slice implemented and locally verified.** Go handles uploads, jobs, and deterministic validation. The standard-library Python worker transcribes Bengali with Groq, detects low-audio intervals and FFmpeg shot cuts, samples at most 16 representative frames, and asks OpenAI for schema-constrained scene evidence. Scene windows use nearby cut evidence, and the UI shows seekable transcript, scenes, confidence, pauses, and shot cuts. Private analysis cache entries are keyed by source-content hash, model IDs, prompt version, and analysis/detector versions. Job and AI evidence JSON sidecars survive service restarts. Staging remains pending repository/deployment setup; break scoring, brand matching, and VMAP/playback remain ahead.

## Run locally

Requirements: Go 1.25+, Python 3.10+, and FFmpeg/ffprobe on `PATH`.

```sh
make run
```

`make run` loads the ignored `.env` file in the project root and starts the Go app. Configure `GROQ_API_KEY` for Bengali transcription and `OPENAI_API_KEY` for scene understanding; optionally set `OPENAI_VISION_MODEL` (defaults to `gpt-4o-mini`). Never commit or share these keys. Without Groq, uploads stop after media intake. Without OpenAI, successful transcripts are retained and the UI reports that scene analysis is unavailable. Open [http://localhost:8080](http://localhost:8080) and upload an MP4 with an audio track. Videos and private JSON job artifacts are kept in `data/uploads`.

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

The container includes Go, Python 3, and FFmpeg. Set `GROQ_API_KEY` and `OPENAI_API_KEY` as runtime environment variables (never Docker build arguments); the worker inherits them. Optionally set `OPENAI_VISION_MODEL`. The container listens on port `8080`. Mount persistent storage at `/app/data` for uploaded videos and analysis sidecars.

## API in this phase

- `GET /healthz` — service health
- `GET /api/jobs` — saved and current jobs
- `POST /api/jobs` — multipart form upload with field name `video`
- `GET /api/jobs/{id}` — intake metadata and job state
- `GET /media/{id}` — source video playback

Job metadata, transcript, scene evidence, and pause intervals are atomically stored as private `.job.json` sidecars alongside the uploaded videos. Interrupted jobs restore as retryable failures; active inference is not resumed automatically.

## Project notes

- [Problem statement](Statements/problem.md)
- [Phased implementation plan](Docs/PROBLEM1-MVP-IMPLEMENTATION-PLAN.md)
- [Implementation status and release gates](Docs/IMPLEMENTATION-STATUS.md)
- [Deferred UI redesign direction](Docs/UI-REDESIGN-ROADMAP.md)
