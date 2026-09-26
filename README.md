# SceneSense — Contextual Ad Lab

An AI-first contextual ad placement MVP for Bengali video. This repository is being built in gated phases so each increment stays runnable and demonstrable.

## Current release

**Phases 0–5 are deployed.** Go handles uploads, jobs, and deterministic policy. The Python worker supports Bengali ASR via Sarvam Saaras v4 or Groq Whisper, detects low-audio intervals and shot cuts, and asks OpenAI for structured scene evidence and break naturalness scores. A Go policy rejects breaks during speech, uncertain or sensitive scenes, and unfinished thoughts, and enforces pacing and ad-load limits. The UI shows accepted and withheld moments with seekable markers and exact reasons; Phase 5 adds VMAP planning and playback with local placeholder creatives. A saved analysis can be opened with `?job=<id>`. The hosted positive selection was verified on a constructed two-scene test fixture; the tested unmodified real excerpts had no eligible break and were correctly withheld.

## Run locally

Requirements: Go 1.25+, Python 3.10+, and FFmpeg/ffprobe on `PATH`.

```sh
make run
```

`make run` loads the ignored `.env` file in the project root and starts the Go app. Set `ASR_PROVIDER=groq` (default) with `GROQ_API_KEY`, or `ASR_PROVIDER=sarvam` with `SARVAM_API_KEY` for Saaras v4 Bengali transcription. Sarvam REST requests use mono 16 kHz WAV chunks of at most 25 seconds; its transcript timestamps are phrase-level. Set `OPENAI_API_KEY` for scene understanding and break scoring; optionally set `OPENAI_VISION_MODEL` and `OPENAI_BREAK_MODEL` (both default to `gpt-4o-mini`). Never commit or share these keys. Set `DEMO_ACCESS_PASSWORD` to enable the demo's HTTP Basic Auth gate (username `demo`); Railway deployments refuse to start if it is missing. `/healthz` remains available to the platform healthcheck. Without the selected ASR provider's key, uploads stop after media intake. Without OpenAI, successful transcripts are retained and the UI reports that scene analysis is unavailable. Open [http://localhost:8080](http://localhost:8080) and upload an MP4 with an audio track. Videos and private JSON job artifacts are kept in `data/uploads`.

Set `ADDR` to change the listen address, `UPLOAD_DIR` to change the upload directory, and `AD_LIBRARY_DIR` (default `<UPLOAD_DIR>/ads`, so ads share the videos' persistent volume) to change where uploaded ads and their catalogue are stored.

The demo ad MP4s are generated on a developer machine and stored at the creative URLs in `assets/brands.json`; the server only serves those static files. Generate them with `go run ./cmd/generate-demo-ads` (requires `rsvg-convert` and FFmpeg). The default slate time and mood are sample annotations; pass `-time` and `-mood` to change them, and use `-force` only when you intend to replace the local files. Brand targeting and negative-context metadata remain separate catalogue data, not video content.

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
- `GET /api/brands` — built-in catalogue merged with uploaded ads (`source`: `builtin` or `custom`)
- `POST /api/ads` — multipart ad upload: `video` (MP4, up to 120 s / 200 MB), `brand_name`, `category`, `target_contexts`, `negative_contexts` (comma-separated), `language`
- `GET /api/jobs/{id}/ad-suggestions?time=<seconds>` — scene before/after a cut and every brand ranked for it, with hard blocks
- `POST /api/jobs/{id}/playback-plan` — each break also takes `allow_skip`, `skip_after_sec`, `click_through_url`, and `cta_label`; VAST carries them as `skipoffset` and `ClickThrough`

## Ad library and cut review

Ads uploaded in the Ad library panel are stored under `AD_LIBRARY_DIR` with a `catalog.json` beside them, so they persist across restarts on the mounted volume. The built-in `assets/brands.json` stays read-only; reusing an uploaded brand's name adds another duration to it. Clicking any shot cut or marker on the timeline opens a review window showing the scene before and after the cut (activities, mood, caution tags) and the ads ranked for it. Brands that the scene model scored use their AI fit; brands added after analysis get a deterministic context match against the scene's activities and description. Negative contexts are always enforced in Go — through the shared `ai/context_taxonomy.json` aliases and literal phrase matches — and an uncertain scene blocks every ad. The server re-applies the same check when the VMAP is built.

Each break also has viewer options: allow skip (default on), skip after N seconds (default 5, and it must unlock before the ad ends), and a website link with a button label. The link defaults to the one saved with the ad in the library and can be overridden per break. **Preview** in the cut window, and on each planner row, opens the player screen about 8 seconds before the break. It plays the ad with its skip countdown and website button, then resumes the programme and shows a short summary. Opening the website starts a new tab and pauses the ad. **Play programme with breaks** uses the same player for the whole VMAP plan and records break, ad, skip, click-through and resume events.

Job metadata, transcript, scene evidence, and pause intervals are atomically stored as private `.job.json` sidecars alongside the uploaded videos. Interrupted jobs restore as retryable failures; active inference is not resumed automatically.

The current release uses a 15-second planning ad, at least 15 seconds of programme before a break, at least 10 seconds after, at most 4 breaks per 30 minutes, a 300-second minimum gap, and at most 20% planned ad load. The Go policy also requires a verified low-audio pause, no overlapping speech, supported scene context on both sides, and passing AI scores. A break is withheld when evidence is missing or uncertain. Phase 4 adds reviewer-controlled `k` markers within these limits. Once actual creative durations exist in Phase 5, the policy must be rechecked using each selected creative's duration before playback.

## Project notes

- [Problem statement](Statements/problem.md)
- [Phased implementation plan](Docs/PROBLEM1-MVP-IMPLEMENTATION-PLAN.md)
- [Implementation status and release gates](Docs/IMPLEMENTATION-STATUS.md)
- [Deferred UI redesign direction](Docs/UI-REDESIGN-ROADMAP.md)
