# Implementation Status

This file records the current release gate and what must pass before work moves to the next phase.

## Phase 0 — Foundation

**Status: complete locally.**

- Go HTTP service and embedded responsive UI
- Health endpoint and structured request logs
- Docker image definition with FFmpeg/ffprobe
- Local Make targets and CI workflow
- Fake-free intake path scaffold; analysis is not claimed as implemented

**Gate evidence:** formatting, `go vet ./...`, `go test ./...`, and the binary build pass locally. The Docker image builds and a temporary local container returned healthy status, served the UI, and included ffprobe. Railway production is configured with the repository and persistent upload volume. CI builds and smoke-tests the image on each pull request.

## Phase 1 — Media intake

**Status: implemented, locally verified, and deployed to the protected production demo.**

- MP4 upload with 500 MB and duration limits
- Real media inspection through ffprobe
- Video and audio track validation
- SHA-256 content hash and duplicate-job reuse
- Uploaded source served to the browser player
- Responsive upload, progress, error, and media-detail UI

**Local gate evidence:** generated MP4 fixture upload returns accurate metadata, duplicate upload reuses the same job, corrupt and silent videos are rejected, the source playback and job-detail endpoints work, and the studio page is served.

**Local release gate evidence:** generated MP4 fixture upload returns accurate metadata, duplicate upload reuses the same job, corrupt and silent videos are rejected, the source playback and job-detail endpoints work, and the studio page is served. The same container image builds, starts, serves health/UI, and includes ffprobe.

**Hosted release gate:** the Railway production service is online with persistent upload storage. The public app is protected by the demo access gate, and authenticated upload, processing, library listing, and media playback have been verified end to end.

## Phase 2 — AI evidence layer

**Status: local implementation and verification complete; protected production demo is live.**

- Go remains responsible for upload, API, job lifecycle, and serving the current UI.
- A dependency-free Python worker supports Groq Whisper Large v3 Turbo and Sarvam Saaras v4 for Bengali ASR. The Sarvam path extracts lossless mono 16 kHz WAV, chunks at 25 seconds for the REST limit, and maps phrase-level timestamps into the shared transcript format. The worker also validates timestamps, detects low-audio intervals and visual shot cuts with FFmpeg, and samples at most 16 low-resolution frames.
- OpenAI Responses API scene analysis uses `gpt-4o-mini` by default with strict JSON Schema output. It returns chronological scene windows, short summaries, activities, tone, sensitive-context tags, confidence, and evidence. `OPENAI_VISION_MODEL` can override the default.
- Shot-cut timestamps are included in scene prompts; scene edges snap to a nearby cut only within a 1.25-second tolerance, and cut positions appear as seekable UI evidence. Scene/pause output and model/prompt identifiers are validated in Python and Go.
- Analysis cache keys include SHA-256 content hash, ASR and vision model IDs, scene prompt version, pipeline version, frame-sampling limit, cut threshold, prompt cut-sampling limit, and silence-detector parameters. Cache artifacts are written atomically with mode 0600; duplicate uploads queue a refresh when the current pipeline key differs.
- Job metadata and AI results are atomically persisted as mode-0600 JSON sidecars. Completed jobs and content-hash dedupe restore across restarts; interrupted queued/processing jobs restore as retryable failures.
- The worker receives and validates `assets/brands.json`: 8 brands, positive and negative context lists, and 21 creative metadata entries. Referenced ad-video files are not currently present in the supplied `assets/` tree, so creative playback remains a later dependency.
- AI processing runs asynchronously with a single in-process slot; failed transcripts can be retried.
- Transcript segments are visible in the UI and their timestamps seek the source player.
- `.env` is ignored by Git and excluded from the Docker build context; local `make run` loads it. Railway should receive secrets as runtime variables.
- The production demo is protected by HTTP Basic Auth using Railway's `DEMO_ACCESS_PASSWORD` variable and fixed username `demo`; Railway startup fails closed if the password is missing. `/healthz` remains public for Railway healthchecks.
- The future Next.js/Three.js/GSAP visual redesign is recorded in `Docs/UI-REDESIGN-ROADMAP.md` and deferred.

**Local verification:** `go test -race ./...`, `go vet ./...`, Python worker tests, JavaScript syntax check, container build, and container health/UI-marker/Python/FFmpeg smoke checks pass. Go/Python cache-key parity, cache hits and prompt-version invalidation, shot-cut parsing, scene-edge snapping, the JSON boundary, persistence, restoration, and interrupted-job behavior have deterministic coverage. Headless Chrome rendered the studio page at 1440×1100 for visual review. FFmpeg detected 400 shot cuts in the cleared 20-minute `bhojon_bilashi.mp4` sample. The user selected Sarvam Saaras v4 after reviewing its output from a 20-second approved excerpt. External model calls are excluded from CI. Railway production remains on its existing Groq configuration until the Sarvam secret and provider variable are added.

**Live AI verification:** direct Groq synthetic smoke test returns HTTP 200. The full cleared asset completed the integrated local pipeline with 203 Bengali transcript segments, 14 structured scene summaries, and 137 low-audio intervals. The API exposed the scene evidence in the UI response, and a service restart restored the job and its evidence sidecar (mode 0600). The upload asset is explicitly approved for external processing by the user.

**Production demo:** [https://scenesense-production-9320.up.railway.app](https://scenesense-production-9320.up.railway.app). `/healthz` is public for Railway healthchecks; all other routes require HTTP Basic Auth (username `demo`, password supplied separately to the project owner). The unauthenticated app responds 401 and authenticated access responds 200. An approved 75-second excerpt from `bhojon_bilashi.mp4` completed hosted AI processing with the prior Groq configuration: 6 Bengali transcript segments, 3 scene summaries, 20 shot cuts, and 3 low-audio intervals. The job appears in the authenticated library, and its media endpoint serves the uploaded video. Railway reports the production service online with its persistent volume mounted. Saaras is not yet configured in Railway; do not switch the production provider until `ASR_PROVIDER=sarvam` and `SARVAM_API_KEY` are set there. `.env`, `.DS_Store`, supplied MP4s, and local `.railway/` link metadata are excluded from Git. A real external-model call is intentionally not part of CI.
