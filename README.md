# SceneSense — Contextual Ad Lab

An AI-first contextual ad placement MVP for Bengali video. This repository is being built in gated phases so each increment stays runnable and demonstrable.

## Current release

**Phases 0–5 are deployed.** Go handles uploads, jobs, and deterministic policy. The Python worker supports Bengali ASR via Sarvam Saaras v4 or Groq Whisper, detects low-audio intervals and shot cuts, and asks OpenAI for structured scene evidence and break naturalness scores. A Go policy rejects breaks during speech, uncertain or sensitive scenes, and unfinished thoughts, and enforces pacing and ad-load limits. The UI shows accepted and withheld moments with seekable markers and exact reasons; Phase 5 adds VMAP planning and playback with local placeholder creatives. A saved analysis can be opened with `?job=<id>`. The hosted positive selection was verified on a constructed two-scene test fixture; the tested unmodified real excerpts had no eligible break and were correctly withheld.

## Run locally

Requirements: Go 1.25+, Python 3.12+, and FFmpeg/ffprobe on `PATH`.

```sh
make setup   # .venv with the pinned worker dependencies, plus the ONNX models (SHA-256 verified)
make run
```

`make run` loads the ignored `.env` file and starts the Go app with the `.venv` Python. Set `ASR_PROVIDER=groq` (default) with `GROQ_API_KEY`, or `ASR_PROVIDER=sarvam` with `SARVAM_API_KEY`. Set `OPENAI_API_KEY` for the boundary judge, scene descriptions, and transcript embeddings; optional overrides are `OPENAI_VISION_MODEL`, `OPENAI_BREAK_MODEL`, and `OPENAI_EMBEDDING_MODEL`. Never commit these keys. `DEMO_ACCESS_PASSWORD` enables HTTP Basic Auth (username `demo`); Railway deployments refuse to start without it, and `/healthz` stays public. Uploads and job sidecars live in `UPLOAD_DIR` (default `data/uploads`); `AD_LIBRARY_DIR` defaults to `<UPLOAD_DIR>/ads`, and `MODELS_DIR` to `models`.

The container starts a small Rust observability collector alongside Go. Locally, Rust 1.91+ is optional: run `cargo run --manifest-path telemetry/Cargo.toml -- data/uploads/observability.json` in a second terminal to populate Pipeline pulse. Go drops telemetry into a bounded, best-effort queue; if the collector is absent or fails, uploads, inference, and playback continue. The collector persists aggregate stage timings and delivery-event counts to the mounted volume; `GET /api/observability` serves a read-only snapshot behind the same demo access gate. No API keys or transcript contents are sent to it.

## How ad breaks are chosen

The pipeline compresses the video before any expensive model sees it, then narrows `n` shot boundaries to `m` real scene changes and `k` ad breaks:

1. **Shots (n).** One FFmpeg decode feeds PySceneDetect (`AdaptiveDetector` for cuts, `ThresholdDetector` for fades) and a twin-comparison dissolve detector, and fills a 2 fps keyframe pool. Keyframes are the middle of each shot, plus one every 4 s in long shots.
2. **Signals per boundary.** CLIP ViT-B/32 (ONNX, int8) compares consecutive shots and a three-shot window on each side, so shot/reverse-shot dialogue is not mistaken for a scene change. YAMNet (ONNX) tracks the audio scene, speech, and music. The worker also measures silence and lulls in dialogue, and a transcript-embedding distance before and after the cut. ASR text that YAMNet does not hear as speech is ignored, because Whisper-style models can invent text over music.
3. **Fusion.** A weighted score keeps the strongest boundary per 15 s and shortlists at most 60.
4. **Model judgement (m).** Only the shortlist reaches the vision model: one frame before and one after, plus the dialogue. It returns new/same scene, the from/to context, topic shift, tension, whether the dialogue is complete, and sensitive contexts. The resulting scenes are described once, with brand fit.
5. **Ad-friendliness.** Each scene change gets a score, a High/Medium/Low tier, and a rationale such as "Setting change from family kitchen to courtyard (dissolve) + 1.2-second silence; dialogue wraps up; calm moment."
6. **Emotional pacing map.** A tension curve on a 2 s grid blends the vision model's per-scene emotional intensity, audio arousal (loudness plus YAMNet's crying, shouting, and sad/scary/angry/exciting music classes), and editing pace. It marks dramatic peaks (a "cliffhanger" when a scene change follows the high stretch within 15 s) and calm valleys, is drawn as a lane inside the zoomable timeline, and tells each break card when it follows a dramatic beat. It informs reviewers; it does not change the policy.
7. **Policy and k.** The Go policy blocks speech across the cut, tense moments, unfinished dialogue, sensitive or uncertain context, and the programme edges. A dynamic-programming optimiser then picks exactly `k` breaks, at least 5 minutes apart, maximising ad-friendliness while spreading the breaks evenly. It recommends `k` from the High and Medium spots (at most 8 per hour and 20% ad load) and explains every choice.
8. **Human review and finalize.** The dashboard shows the n → m → k funnel on a zoomable timeline (−/+/Fit, keyboard +/−/0, or Ctrl/⌘ + scroll), which zooms around the playhead, with a time ruler and scene bands. Reviewers can change `k`, remove or force-include spots, place breaks anywhere, choose ads and viewer options, and **Finalize**. That writes a versioned OTT manifest (`/api/jobs/{id}/manifest.json`, schema at `/schema/ad-manifest-v1.json`) alongside VMAP/VAST. The review is saved on the server; editing after finalizing marks it as a draft until you finalize again.

The policy sidebar updates with the selected break count, closest spacing, programme edge time, and ad-load calculation. It uses each chosen creative's duration and labels any still-unselected duration as an estimate; Finalize rechecks the limits and brand safety on the server.

`python ai/synthetic_fixture.py out.mp4` builds a clip with known cuts, a fade, a dissolve, shot/reverse-shot, and camera motion; the tests use it as ground truth.

## Checks

```sh
gofmt -w *.go
go vet ./...
go test ./...
cargo test --manifest-path telemetry/Cargo.toml --offline
.venv/bin/python -m unittest discover -s ai -p 'test_*.py'
go build ./...
```

The end-to-end media intake test uses FFmpeg to generate a short video fixture. It skips when FFmpeg or ffprobe is unavailable; the container includes both tools.

## Container

```sh
docker build -t contextual-ad-lab:local .
docker run --rm -p 8080:8080 -v contextual-ad-data:/app/data contextual-ad-lab:local
```

The container includes Go, Python 3, FFmpeg, and a standalone Rust telemetry collector. Set the selected ASR provider and its key (`ASR_PROVIDER=sarvam` with `SARVAM_API_KEY`, or the Groq defaults) plus `OPENAI_API_KEY` as runtime environment variables (never Docker build arguments); the worker inherits them. Optionally set `SARVAM_ASR_MODEL` or `OPENAI_VISION_MODEL`. The container listens on port `8080`. Mount persistent storage at `/app/data` for uploaded videos, analysis sidecars, and the observability snapshot.

## API in this phase

- `GET /healthz` — service health
- `GET /api/observability` — read-only Rust-collected pipeline timings and delivery counts (or `unavailable` if collector is down)
- `GET /api/jobs` — saved and current jobs
- `POST /api/jobs` — multipart form upload with field name `video`
- `GET /api/jobs/{id}` — intake metadata and job state
- `GET /media/{id}` — source video playback
- `GET /api/brands` — built-in catalogue merged with uploaded ads (`source`: `builtin` or `custom`)
- `POST /api/ads` — multipart ad upload: `video` (MP4, up to 120 s / 200 MB), `brand_name`, `category`, `target_contexts`, `negative_contexts` (comma-separated), `language`
- `POST /api/jobs/{id}/retry` — `{"from": "transcription" | "scene_analysis"}` restarts from that phase; a scene-analysis retry reuses the saved transcript (it is saved as soon as ASR finishes)
- `POST /api/jobs/{id}/cancel` — stop queued or running analysis, persist a retryable cancelled state, and terminate the active worker process
- `DELETE /api/jobs/{id}` — delete a video with its analysis, review, manifests, and cached transcript/analysis (refused while it is being analysed)
- `DELETE /api/ads/{brandID}` and `DELETE /api/ads/{brandID}/{creativeID}` — remove an uploaded brand or one of its ads; built-in brands cannot be removed
- `POST /api/jobs/{id}/optimize` — `{"k", "pinned", "excluded"}` → the best-spaced `k` breaks with an explanation for every candidate
- `PUT /api/jobs/{id}/review` — save the reviewer's working plan
- `POST /api/jobs/{id}/finalize` — validate and publish a new manifest revision; `GET /api/jobs/{id}/manifest.json[?revision=n]` downloads it
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
