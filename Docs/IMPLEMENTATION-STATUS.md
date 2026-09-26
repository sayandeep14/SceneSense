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

**Local verification:** `go test -race ./...`, `go vet ./...`, Python worker tests, JavaScript syntax check, container build, and container health/UI-marker/Python/FFmpeg smoke checks pass. Go/Python cache-key parity, cache hits and prompt-version invalidation, shot-cut parsing, scene-edge snapping, the JSON boundary, persistence, restoration, and interrupted-job behavior have deterministic coverage. Headless Chrome rendered the studio page at 1440×1100 for visual review. FFmpeg detected 400 shot cuts in the cleared 20-minute `bhojon_bilashi.mp4` sample. The user selected Sarvam Saaras v4 after reviewing its output from a 20-second approved excerpt. External model calls are excluded from CI. Railway production has `ASR_PROVIDER=sarvam` and `SARVAM_API_KEY` configured; hosted Saaras output was reviewed and approved by the owner.

**Live AI verification:** direct Groq synthetic smoke test returns HTTP 200. The full cleared asset completed the integrated local pipeline with 203 Bengali transcript segments, 14 structured scene summaries, and 137 low-audio intervals. The API exposed the scene evidence in the UI response, and a service restart restored the job and its evidence sidecar (mode 0600). The upload asset is explicitly approved for external processing by the user.

**Production demo:** [https://scenesense-production-9320.up.railway.app](https://scenesense-production-9320.up.railway.app). `/healthz` is public for Railway healthchecks; all other routes require HTTP Basic Auth (username `demo`, password supplied separately to the project owner). The unauthenticated app responds 401 and authenticated access responds 200. An approved 75-second excerpt from `bhojon_bilashi.mp4` completed hosted AI processing. The owner reran it with Sarvam Saaras v4 and confirmed that the Bengali transcript is correct. Railway reports the service online with its persistent volume mounted. `.env`, `.DS_Store`, supplied MP4s, and local `.railway/` link metadata are excluded from Git. A real external-model call is intentionally not part of CI.

## Phase 3 — Break scoring and safety policy

**Status: implemented, deployed, and end-to-end tested on the protected production demo.**

- Python proposes at most 32 moments from actual scene ends, low-audio intervals, nearby shot cuts, and sentence endings, then asks an OpenAI model for structured naturalness, disruption-risk, confidence, and a short reason for each proposal. Model output is checked for matching candidate IDs and bounded scores.
- Go applies the versioned `break-policy-v1` safety rules to the scored proposals. It requires a verified pause and scene coverage on both sides, rejects overlapping speech, unfinished or ongoing dialogue, sensitive or uncertain contexts, low confidence, and poor AI scores. It then selects the strongest eligible moments under edge, gap, hourly-count, and planned ad-load limits. Every proposal carries an accepted/withheld verdict with exact reasons.
- The UI shows a seekable timeline with selected and withheld markers, model scores, and policy explanations. Phase 3 cache entries include the break model and prompt version. A matching Phase 2 evidence cache is reused so a rerun can add scores without paying for ASR and scene analysis again.
- The 15-second ad duration is a planning value; Phase 4–5 must reapply ad-load policy with the actual selected creative duration before playback.

**Local gate evidence:** Go race tests and Python tests cover policy safety, pacing, cache reuse, model-score schema errors, and Go/Python cache-key parity. A synthetic candidate was scored successfully by the configured live OpenAI model. The container builds, and CI checks run without external model calls.

**Hosted gate evidence:** Approved real excerpts (75-second dialogue, 90-second action, 90-second emotional, and 125-second dialogue) completed the production pipeline. Their proposed moments were all withheld because none met the verified-pause and uninterrupted-scene requirements; this is the intended fail-closed result, not evidence of a usable ad opportunity in those clips. For the positive path, a clearly labeled 66-second test fixture composed of two approved excerpts separated by a quiet black transition produced 13 proposals and exactly one selected moment at 25.515 seconds. Its AI scores were naturalness 0.85, disruption risk 0.15, and confidence 0.8; the Go policy accepted it. The [saved production demo job](https://scenesense-production-9320.up.railway.app/?job=43621362-8791-42f5-8ba2-08856af5eefa) opens directly after demo authentication. A headless browser confirmed the selected timeline marker, verdict card, and explanation render. A naturally occurring selected break in an unmodified real excerpt has not yet been demonstrated; broader editorial validation remains useful before treating recommendations as production-ready.

## Phase 4 — Scene continuity, contextual brand fit, and review controls

**Status: implementation underway; local gates pass, hosted gate pending.**

- The existing OpenAI vision model now receives up to 16 sparse full-programme frames plus paired frames around a bounded, time-spread set of shot cuts. It classifies each pair as a camera-only cut, setting/activity/story change, or uncertain, and gives visible evidence. The same model remains in use; no new provider or key is required. Transition probe sampling is bounded to 12 pairs, and the ASR/evidence cache is reused while refreshing scene evidence.
- Scene analysis returns mood, activity, sensitive context, and per-brand fit scores from the supplied catalogue. A deterministic context taxonomy blocks brand recommendations that conflict with a scene's negative-context labels, even when the model reports a high fit. Uncertain scenes block all brand recommendations. No creative video files are present under `assets/`, so this increment reports fit and safety evidence without claiming ad playback.
- Break candidates carry the preceding scene's mood and its safe/blocked brand matches. The reviewer UI distinguishes all FFmpeg shot cuts (`n`), AI-qualified potential breaks (`m`), and selected markers (`k`). The `k` slider and marker controls support choosing AI options or adding/removing a marker at any precise playhead time; job edits persist in that browser's local storage.
- Go enforces a planning cap of four breaks per 30 minutes, a 300-second gap, programme edge guards, and the 20% planned ad-load cap. Reviewer-added timestamps still obey those mechanical limits and are labeled for contextual review.

**Local gate evidence:** Go race tests, Python tests, JavaScript syntax checks, and cache-key parity pass. Python tests cover scene-transition evidence, cache migration, brand negative-context blocking, uncertain scenes, and preceding-scene mood. A generated two-color clip verified before/after extraction around a hard cut. The hosted model/UI gate and browser editing flow remain to be completed.
