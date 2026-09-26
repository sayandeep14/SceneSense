# Implementation Status

This file records the current release gate and what must pass before work moves to the next phase.

## Phase 0 — Foundation

**Status: complete locally.**

- Go HTTP service and embedded responsive UI
- Health endpoint and structured request logs
- Docker image definition with FFmpeg/ffprobe
- Local Make targets and CI workflow
- Fake-free intake path scaffold; analysis is not claimed as implemented

**Gate evidence:** formatting, `go vet ./...`, `go test ./...`, and the binary build pass locally. The Docker image builds and a temporary local container returned healthy status, served the UI, and included ffprobe. The public staging target and deploy credentials are not present in the workspace. CI now builds and smoke-tests the image on each pull request.

## Phase 1 — Media intake

**Status: implemented and locally verified in the app and container. Hosted deployment gate pending.**

- MP4 upload with 500 MB and duration limits
- Real media inspection through ffprobe
- Video and audio track validation
- SHA-256 content hash and duplicate-job reuse
- Uploaded source served to the browser player
- Responsive upload, progress, error, and media-detail UI

**Local gate evidence:** generated MP4 fixture upload returns accurate metadata, duplicate upload reuses the same job, corrupt and silent videos are rejected, the source playback and job-detail endpoints work, and the studio page is served.

**Local release gate evidence:** generated MP4 fixture upload returns accurate metadata, duplicate upload reuses the same job, corrupt and silent videos are rejected, the source playback and job-detail endpoints work, and the studio page is served. The same container image builds, starts, serves health/UI, and includes ffprobe.

**Hosted release gate:** deploy to an agreed staging target, then upload and play a fixture through that deployed URL. Hosting is not configured in this workspace.

## Phase 2 — AI evidence layer

**Status: local implementation and verification complete; staging/E2E hosted gate pending.**

- Go remains responsible for upload, API, job lifecycle, and serving the current UI.
- A dependency-free Python worker extracts mono 16 kHz audio, calls Groq Whisper Large v3 Turbo, validates timestamps, detects low-audio intervals and visual shot cuts with FFmpeg, and samples at most 16 low-resolution frames.
- OpenAI Responses API scene analysis uses `gpt-4o-mini` by default with strict JSON Schema output. It returns chronological scene windows, short summaries, activities, tone, sensitive-context tags, confidence, and evidence. `OPENAI_VISION_MODEL` can override the default.
- Shot-cut timestamps are included in scene prompts; scene edges snap to a nearby cut only within a 1.25-second tolerance, and cut positions appear as seekable UI evidence. Scene/pause output and model/prompt identifiers are validated in Python and Go.
- Analysis cache keys include SHA-256 content hash, ASR and vision model IDs, scene prompt version, pipeline version, frame-sampling limit, cut threshold, prompt cut-sampling limit, and silence-detector parameters. Cache artifacts are written atomically with mode 0600; duplicate uploads queue a refresh when the current pipeline key differs.
- Job metadata and AI results are atomically persisted as mode-0600 JSON sidecars. Completed jobs and content-hash dedupe restore across restarts; interrupted queued/processing jobs restore as retryable failures.
- The worker receives and validates `assets/brands.json`: 8 brands, positive and negative context lists, and 21 creative metadata entries. Referenced ad-video files are not currently present in the supplied `assets/` tree, so creative playback remains a later dependency.
- AI processing runs asynchronously with a single in-process slot; failed transcripts can be retried.
- Transcript segments are visible in the UI and their timestamps seek the source player.
- `.env` is ignored by Git and excluded from the Docker build context; local `make run` loads it. Railway should receive secrets as runtime variables.
- The future Next.js/Three.js/GSAP visual redesign is recorded in `Docs/UI-REDESIGN-ROADMAP.md` and deferred.

**Local verification:** `go test -race ./...`, `go vet ./...`, 17 Python worker tests, JavaScript syntax check, container build, and container health/UI-marker/Python/FFmpeg smoke checks pass. Go/Python cache-key parity, cache hits and prompt-version invalidation, shot-cut parsing, scene-edge snapping, the JSON boundary, persistence, restoration, and interrupted-job behavior have deterministic coverage. Headless Chrome rendered the studio page at 1440×1100 for visual review. FFmpeg detected 400 shot cuts in the cleared 20-minute `bhojon_bilashi.mp4` sample. No new paid model call was needed for these checks. Hosted deployment is not configured in this workspace.

**Live AI verification:** direct Groq synthetic smoke test returns HTTP 200. The full cleared asset completed the integrated local pipeline with 203 Bengali transcript segments, 14 structured scene summaries, and 137 low-audio intervals. The API exposed the scene evidence in the UI response, and a service restart restored the job and its evidence sidecar (mode 0600). The upload asset is explicitly approved for external processing by the user.

**Remaining Phase 2 gate:** deploy to staging and repeat the upload/evidence flow after a Git remote/initial commit and Railway project/service binding are configured. This workspace currently has no commits or Git remote, and no Railway CLI/project binding. `.env`, `.DS_Store`, and supplied MP4s are excluded from Git. The browser review covered the rendered studio shell; scene evidence rendering is covered by UI code and data tests, but should also be inspected against a completed staging job. A real external-model call is intentionally not part of CI.
