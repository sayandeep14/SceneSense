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

**Hosted gate evidence:** Approved real excerpts (75-second dialogue, 90-second action, 90-second emotional, and 125-second dialogue) completed the production pipeline. Their proposed moments were withheld because none met the verified-pause and uninterrupted-scene requirements. An earlier Phase 3 run of the clearly labeled 66-second test fixture produced one accepted moment; the current Phase 4 analysis re-scored it and correctly returned zero potential/selected breaks under the current AI evidence, while still allowing one by the duration-based policy cap. The fixture currently reports 13 proposals and scene moods; this is a fail-closed outcome, not a claim that the fixture contains a usable ad point. The [saved production demo job](https://scenesense-production-9320.up.railway.app/?job=43621362-8791-42f5-8ba2-08856af5eefa) opens directly after demo authentication. A naturally occurring selected break in an unmodified real excerpt has not yet been demonstrated; broader editorial validation remains useful before treating recommendations as production-ready.

## Phase 4 — Scene continuity, contextual brand fit, and review controls

**Status: implemented and deployed; local and hosted API gates pass.**

- The existing OpenAI vision model now receives up to 16 sparse full-programme frames plus paired frames around a bounded, time-spread set of shot cuts. It classifies each pair as a camera-only cut, setting/activity/story change, or uncertain, and gives visible evidence. The same model remains in use; no new provider or key is required. Transition probe sampling is bounded to 12 pairs, and the ASR/evidence cache is reused while refreshing scene evidence.
- Scene analysis returns mood, activity, sensitive context, and per-brand fit scores from the supplied catalogue. A deterministic context taxonomy blocks brand recommendations that conflict with a scene's negative-context labels, even when the model reports a high fit. Uncertain scenes block all brand recommendations. The catalogue remains separate from creative video files; Phase 5 supplies clearly labelled local placeholders for playback testing.
- Break candidates carry the preceding scene's mood and its safe/blocked brand matches. The reviewer UI distinguishes all FFmpeg shot cuts (`n`), AI-qualified potential breaks (`m`), and selected markers (`k`). The `k` slider and marker controls support choosing AI options or adding/removing a marker at any precise playhead time; job edits persist in that browser's local storage.
- Go enforces a planning cap of four breaks per 30 minutes, a 300-second gap, programme edge guards, and the 20% planned ad-load cap. Reviewer-added timestamps still obey those mechanical limits and are labeled for contextual review.

**Local gate evidence:** `go test -race ./...`, `go vet ./...`, all 34 Python tests, JavaScript syntax validation, and cache-key parity pass. Python tests cover scene-transition evidence, cache migration, brand negative-context blocking, uncertain scenes, and correct scene-at-marker mood attribution. A generated two-color clip verified before/after extraction around a hard cut.

**Hosted gate evidence:** Commit `f6698e7` is active on Railway; `/healthz` returns 200, the demo returns 401 without credentials and 200 with them. The approved 66-second fixture completed the V4 visual pipeline: the model classified two sampled transitions as new-scene changes (80% and 85% confidence) and one as camera-only/same-scene (70%). Candidate cards now carry the active scene mood—for example, `reflective, curious` before 26s and `informative, formal` after 26s. In this rerun no candidate qualified as an AI potential, and no safe brand fit was returned; the reviewer can still add a manual marker after inspecting the video. This demonstrates the fail-closed path, not an editorially validated ad insertion. The page and API load in production; a full browser interaction test of marker editing remains a useful follow-up.

## Phase 5 — VMAP, playback, and ad-resume

**Status: deployed; local contract tests and production API/static-asset smoke tests pass. Full browser ad-interruption/resume rehearsal remains open.**

- `go run ./cmd/generate-demo-ads` generates 21 black-screen placeholder MP4s locally from the catalogue (15/20/30 seconds). The sample insertion time and mood are configurable with `-time` and `-mood`; `-force` is required to overwrite files. These files are static demo assets. The running server contains no FFmpeg creative-generation path.
- The plan API validates completed analysis, AI/manual placement provenance, preceding-scene brand-safety evidence, selected catalogue creative, minimum gap, edge guards, and actual-duration ad load. It emits ordered VMAP/VAST and downloadable debug JSON.
- Browser playback pauses the programme at each selected marker, plays its creative once, reports break/ad/complete/resume/error/skip events, and resumes the programme at the exact marker. Missing or unplayable ad files fail open to the programme.
- Targeting and negative-context metadata remain catalogue data separate from ad MP4s; runtime creative or metadata upload controls are not part of this increment.

**Local gate evidence:** `GOCACHE=/private/tmp/hoichoi-gocache go test -race ./...`, `go vet ./...`, all 34 Python tests, `node --check web/app.js`, and `git diff --check` pass. Go tests probe all 21 files with ffprobe, verify their catalogue durations, validate VMAP/VAST and event persistence, exercise static creative serving, and confirm a missing file is not generated by the server. The generated slate was visually inspected. Local Docker-daemon access is unavailable; GitHub Actions built and smoke-tested the container successfully.

**Hosted gate evidence:** Commit `cb8e172` is deployed on Railway. CI run `36246343752` passed, including the Docker build and container smoke test. Production `/healthz` returns 200; the demo gate returns 401 without authentication and the studio returns 200 when authenticated. `/api/brands` returned all 8 brands and 21 creatives. The ad endpoint returned HTTP 206 for a byte-range request, with an MP4 `ftyp` header, confirming static media packaging and browser seek support. The authenticated UI and JavaScript loaded successfully. The stored analysis currently has no naturally eligible brand-safe insertion, so a complete ad-interruption/resume playthrough still needs an appropriate reviewed fixture and a browser rehearsal.

## Ad library and cut-level ad review

**Status: implemented and verified locally; not yet deployed.**

- The Ad library panel uploads an MP4 ad (up to 120 seconds, 200 MB) with brand name, category, target contexts, negative contexts, and language. Uploads are probed with ffprobe and stored under `AD_LIBRARY_DIR`, which defaults to `<UPLOAD_DIR>/ads` (`/app/data/uploads/ads` on Railway), with a `catalog.json`, so they persist on the mounted volume. The built-in catalogue remains read-only.
- Clicking any shot cut, AI potential marker, or selected marker opens a review window with the scene before and after the cut (activities, mood, caution tags, confidence), AI break evidence, and every brand ranked for that moment. The reviewer picks a brand and one of its creative durations; the playback planner now summarises each marker's ad and reopens the same window to change it.
- Brands scored by the scene model keep their AI fit; brands added later get a deterministic context match against the scene's activities and description. Negative contexts are enforced in Go for every brand through the shared `ai/context_taxonomy.json` aliases plus literal phrase matches, so a new brand's unseen negative context (for example `alcohol`) still blocks. Uncertain scenes block every brand, and the VMAP build re-applies the same check.

**Local gate evidence:** Go race tests cover upload validation, persistence across a server restart, static serving of uploaded creatives, suggestion ranking, unseen negative-context blocking, and VMAP acceptance/rejection of an uploaded brand. A headless-Chrome run against a synthetic three-scene fixture uploaded an ad, saw it suggested after a tea scene, saw every brand blocked after a funeral scene, placed two breaks, built the VMAP, and confirmed both persisted after reload. The uploaded creative served with HTTP 200 and 206 range responses.

## Player screen, skip rules, and website links

**Status: implemented and verified locally; not yet deployed.**

- A dedicated player screen (`web/player.js`) replaces the inline ad overlay. It shows a programme timeline with yellow ad markers, an "Ad in N" countdown, and an ad state with a sponsor card and a skip button that counts down and unlocks at the configured second. It also has an ad progress bar, a paused state after the website opens, a break list with per-break outcomes, keyboard shortcuts, full-screen mode, and an end-of-session summary.
- Each break stores allow-skip, skip-after seconds, and an optional website link and button label. The ad library keeps a default link and label per uploaded brand. The plan API validates them: skip must unlock before the ad ends, and links must be absolute http(s) addresses. VAST emits `skipoffset` and `VideoClicks/ClickThrough`.
- Preview plays one break in context before it is added. Full playback uses the same player for the VMAP plan and records `click_through` alongside the existing events.

**Local gate evidence:** Go race tests cover skip and link validation, library-link fallback and per-break override, omission of `skipoffset` for non-skippable ads, and upload-time link validation. A headless-Chrome run previewed a break and confirmed the following. The countdown appeared. Skip unlocked at 5.1 seconds. The website opened in a new tab and paused the ad. Skip returned to the exact break time, and the summary recorded the skip and the click. Full playback also worked: a non-skippable ad hid the skip button, jumping from the break list played the chosen break, and events were persisted.
