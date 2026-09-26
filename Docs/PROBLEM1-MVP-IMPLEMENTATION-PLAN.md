# Problem 1 MVP: Implementation, Testing, and Deployment Plan

## 1. Objective

Build an AI-first contextual ad-break engine for Bengali long-form video:

```text
Video upload
  -> media evidence extraction
  -> AI scene/context understanding
  -> safe break-candidate scoring
  -> deterministic pacing and safety policy
  -> AI brand/creative matching
  -> VMAP + debug JSON
  -> playable demo that inserts an ad and resumes the programme
```

The MVP is successful only when the entire path works on a video that was not used to hard-code timestamps or brand assignments.

### MVP promise

> Given a Bengali drama clip and a synthetic brand catalogue, the system finds natural, policy-compliant break points, chooses a contextually appropriate brand, emits a standards-compliant VMAP manifest, and plays the programme → ad → programme sequence live.

### Explicit non-goals

- Real ad-network integration or production billing
- Perfect understanding of every Bengali dialect
- Automatic replacement of the video’s original edit
- A large-scale distributed media platform
- Supporting every video format before the core path works

Keep the first implementation narrow: one accepted video format, a small synthetic catalogue, short ads, and one player. Generalisation must come from the data contracts and AI decisions—not from hard-coded sample timestamps.

## 2. Design principles

1. **AI is the core.** ASR, scene description, emotional/context classification, boundary reasoning, and brand matching are model-driven.
2. **Hard safety rules remain deterministic.** Maximum ad load, minimum gap, negative-context blocks, manifest validity, and playback state must never depend on an unconstrained model response.
3. **Every AI decision is structured and inspectable.** Store the model name/version, prompt version, confidence, evidence, and decision reason.
4. **Fail closed.** If context is uncertain or a hard block is detected, skip the break or brand instead of guessing.
5. **Each phase produces a deployable increment.** Do not begin the next phase until CI, staging deployment, and an end-to-end smoke test pass.
6. **The demo path and evaluation path are the same path.** Rehearsal shortcuts may improve speed, but they must not bypass the actual pipeline.

## 3. Proposed MVP architecture

### Components

- **Web UI:** upload video, show job progress, display timeline/debug evidence, launch the player.
- **API:** accepts jobs, exposes status/results, serves manifests and debug data.
- **Media/API layer:** Go handles upload validation, job lifecycle, media serving, and deterministic application behavior.
- **AI worker:** Python owns all model-facing/media-analysis logic (audio extraction, ASR, multimodal scene analysis, structured JSON, and evaluation). The Go API starts it as a bounded subprocess using a versioned JSON stdin/stdout contract; keep one service/container for the MVP.
- **Policy engine:** deterministic validator for time, pacing, negative contexts, ad load, and confidence thresholds.
- **Brand catalogue:** synthetic brands in JSON/SQLite with creative metadata, positive contexts, negative contexts, and ad URLs.
- **Artifact store:** local disk in development; object storage or a mounted volume in deployment.
- **Metadata store:** SQLite for the first deploy, with a clean repository boundary so it can be replaced later.

### Keep the first deployment simple

Use one Dockerized service containing the API, static frontend, and worker process. This reduces deployment and networking failures during the hackathon. Split the worker into a separate service only after the single-container path is stable.

The deployment target may be any managed container service available to the team. The required environments are:

- **Local:** fast development with fake AI adapters and real media fixtures.
- **Staging:** real AI adapters, safe synthetic data, public demo URL.
- **Production/demo:** the exact version used for presentation, promoted from staging.

## 4. AI decision pipeline

### 4.1 Evidence extraction

For every uploaded video, create a content hash and extract:

- Duration, frame rate, dimensions, audio presence, and codec
- Shot boundaries
- Bengali transcript with word-level timestamps
- Silence/pauses and sentence-ending punctuation
- Optional speaker turns or voice activity segments
- Representative frames for each candidate scene

The media layer supplies evidence; it does not decide whether an ad should play.

### 4.2 Scene understanding

For each scene window, the AI adapter returns strict structured data:

```json
{
  "scene_id": "scene-07",
  "start": 412.2,
  "end": 486.8,
  "summary": "Two characters discuss a train journey in a quiet room.",
  "activities": ["conversation", "indoor"],
  "tone": ["reflective", "calm"],
  "sensitive_contexts": [],
  "dialogue_state": "complete_thought",
  "confidence": 0.88,
  "evidence": ["pause after sentence", "shot change", "calm dialogue"]
}
```

The model must be instructed to distinguish explicit evidence from inference. JSON schema validation rejects malformed or unexplained responses.

### 4.3 Break-candidate scoring

Generate candidates from several independent signals:

- Shot change
- Sentence completion
- Silence or natural pause
- End of a scene or action beat
- No overlapping speech
- Distance from previous and next possible break

The AI scores naturalness and disruption risk. The policy engine then rejects candidates that violate hard rules. A candidate is eligible only when all of the following hold:

- It is not inside a word or active sentence.
- It is not inside overlapping dialogue.
- It is not inside a hard-blocked sensitive context.
- It satisfies the configured minimum gap.
- It keeps total ad load within the configured limit.
- It meets the minimum evidence/confidence threshold.

### 4.4 Brand matching

Each synthetic brand has a catalogue record such as:

```json
{
  "brand_id": "brand-09",
  "name": "Nabob Tea",
  "categories": ["beverage", "tea"],
  "positive_contexts": ["morning", "family", "conversation"],
  "negative_contexts": ["funeral", "injury", "mourning"],
  "creative_url": "/ads/nabob-tea.mp4"
}
```

The matcher first applies deterministic negative-context blocking, then uses AI/embeddings to rank the remaining brands by scene fit. The catalogue is the only place where brand-specific information may live. Adding the ninth held-out brand must require a data change only, with zero code changes.

### 4.5 Explainability

For every selected or rejected candidate, expose:

- Scene summary and transcript excerpt
- Naturalness score and supporting evidence
- Policy rules that passed or failed
- Brands considered
- Hard blocks applied
- Final brand rationale

This is useful for debugging, judging, and the final presentation. It also demonstrates that AI is driving the decision rather than merely generating a label.

## 5. Phased implementation and release gates

Every phase follows the same loop:

```text
Implement -> local tests -> CI -> deploy staging -> E2E smoke test -> record evidence -> continue
```

If a gate fails, fix or revert that phase before adding new functionality.

### Phase 0 — Repository and delivery foundation

**Build**

- Create API, worker, frontend, and shared-schema directories.
- Add Dockerfile and local development command.
- Add configuration for AI provider, model, prompt version, storage path, and policy profile.
- Add health endpoint, structured logging, request/job IDs, and a minimal UI shell.
- Create a provider interface with both `FakeAIProvider` and `RealAIProvider`.

**CI checks**

- Formatting and linting
- Type checking
- Unit test discovery
- Docker image build
- Health endpoint smoke test using the fake provider

**Deployment**

- Deploy the empty vertical slice to staging.
- Confirm the public URL, environment variables, logs, and rollback method.

**E2E gate**

`GET /health` succeeds in staging, the UI loads, and a fake job can move from `queued` to `completed`.

**Stop condition**

Do not proceed until a fresh clone can build and the staging deployment can be rolled back.

### Phase 1 — Media intake and artefact handling

**Build**

- Upload one supported video format, preferably MP4/H.264 with AAC audio.
- Validate size, duration, codec, and audio presence.
- Compute a content hash for idempotency.
- Extract media metadata, audio, low-resolution preview, and shot boundaries.
- Add job states: `queued`, `processing`, `failed`, `completed`.

**Tests**

- Valid upload and invalid codec tests
- Duplicate content hash test
- Missing-audio test
- Corrupt/truncated file test
- FFmpeg failure propagation test
- Job retry and failure-state test

**Deployment**

- Deploy to staging with a small fixture video.
- Verify storage permissions and cleanup of failed temporary files.

**E2E gate**

Upload a new video through the UI, observe progress, and download its metadata and preview from the staging URL.

### Phase 2 — AI evidence layer

**Build**

- Integrate Bengali ASR with word-level timestamps.
- Keep ASR and all future model/media-analysis logic in the Python worker; Go owns HTTP, upload, job lifecycle, and persistence only.
- Add silence and pause detection.
- Create scene windows from shot boundaries plus transcript/topic changes.
- Add structured multimodal scene analysis.
- Display transcript evidence and seekable timestamps in the current UI; defer the Next.js/Three.js/GSAP redesign until the end-to-end product path is stable (see `Docs/UI-REDESIGN-ROADMAP.md`).
- Persist transcript, scene summaries, confidence, model version, and prompt version.
- Add retry, timeout, JSON-schema validation, and content-hash/model-version caching.

**Tests**

- ASR response schema tests using recorded fixtures
- Timestamp monotonicity and duration-bound tests
- Silence must not create invented dialogue
- Malformed model output must fail safely
- Provider timeout and retry tests
- Cache hit/miss tests when the model version changes
- Bengali code-switched sentence fixture

**AI evaluation gate**

Use a small labelled set with expected scene boundaries and context tags. Accept approximate boundaries within a defined tolerance, but require all hard sensitive contexts to be surfaced in the test fixtures.

**Deployment**

- Deploy real AI adapters to staging.
- Run one real video end to end and save the resulting debug artefacts.

**E2E gate**

The staging UI displays a transcript, scene timeline, context tags, and model evidence for a newly uploaded video.

### Phase 3 — Break scoring and policy engine

**Build**

- Generate candidate boundaries from media evidence.
- Add AI naturalness/disruption scoring.
- Implement deterministic rules for sentence integrity, active speech, minimum gap, maximum breaks per hour, and maximum ad-load percentage.
- Add a fail-closed rule for uncertain or sensitive scenes.
- Return both accepted and rejected candidates with reasons.

**Tests**

- Mid-sentence candidate is rejected
- Active-dialogue candidate is rejected
- Natural scene-end candidate is accepted
- Minimum gap is always respected
- Maximum ad count and ad-load percentage are always respected
- Unknown or low-confidence context is not treated as safe
- Multiple candidates near one another resolve deterministically
- Same input and policy version produce the same policy result

**Evaluation gate**

The golden fixture suite must have zero hard-policy violations. A model score may be imperfect, but the policy engine may never output an unsafe break.

**Deployment**

- Deploy with a policy version shown in the UI.
- Run staging analysis on at least three different pacing styles: dialogue-heavy, action-heavy, and quiet/emotional.

**E2E gate**

The UI shows a timeline containing accepted breaks, rejected candidates, scores, and exact rule explanations.

### Phase 4 — Synthetic brand catalogue and contextual matching

**Build**

- Treat visual shot cuts as evidence to inspect, not automatic scene boundaries. Sample paired frames before/after a bounded, time-spread set of cuts and classify camera-only changes versus setting, activity, or story changes with the existing vision model. Keep sparse full-programme frames so dissolves and transitions without a hard cut can still inform semantic scene segmentation.
- Store scene mood, activities, sensitive context, transition type, confidence, and reviewer-facing evidence. Use the preceding scene's mood when scoring interruption quality and contextual brand fit.
- Use the existing synthetic catalogue (`assets/brands.json`) for AI brand ranking by scene, tone, and positive contexts. Apply a deterministic context taxonomy to block catalogue negative contexts even when the model gives a high fit score. Never recommend a brand for an uncertain scene.
- Keep missing creative files explicit: the supplied asset tree currently contains catalogue metadata only. Brand fit can be reviewed now; ad playback must wait until valid creative files exist.
- Show three timeline layers: all detected shot cuts (`n`), AI-qualified safe/contextual opportunities (`m`), and the chosen ad markers (`k`). Let the reviewer set `k`, review by an accessible precise seek slider, toggle AI suggestions, add/remove markers at any playhead time, and save edits in the browser for that job.
- Bound `k` by four breaks per 30 minutes, a five-minute minimum gap, programme edge guards, and the planned ad-load cap. If the reviewer asks for more markers than AI found, allow explicit manual placements within those mechanical limits and label them for human review.

**Tests**

- Food/beverage brand blocked after funeral or mourning context
- Injury-sensitive brands blocked after injury context
- Eligible brand selected when multiple safe brands exist
- Unknown brand added through data only
- Empty eligible-brand set produces “no ad selected,” not a forced match
- Brand selection is reproducible with a fixed model/policy version
- A reverse-angle camera cut remains in the same scene; a setting/activity change is surfaced as a distinct transition
- Mood from the preceding scene is present in the break and brand explanation
- Four markers can be selected in a 30-minute fixture; a fifth is rejected
- Near-duplicate placements and placements less than five minutes apart are blocked
- Reviewer can seek, select/deselect AI markers, add a marker outside the AI list, remove it, and reload the saved job without losing edits
- No real company names are present in synthetic data

**Evaluation gate**

Zero negative-context brand recommendations in the hard-negative suite. The held-out ninth brand must be matched or safely rejected without changing application code. Human-authored placement edits remain within count, spacing, edge, and ad-load limits.

**Deployment**

- Deploy the scene/brand evidence version with visible model and prompt provenance.
- Verify that the browser can add a human marker and restore it when reopening the same job.
- Verify the VLM call reuses the ASR cache instead of retranscribing.

**E2E gate**

For one uploaded video, the UI shows all shot cuts, distinguishes camera-only cuts from scene changes, explains preceding-scene mood, shows a safe brand fit and a context-blocked brand, lets the reviewer choose `k` within limits, and supports precise manual edits outside AI suggestions.

### Phase 5 — VMAP, playback, and ad-resume path

**Status:** Deployed; CI, production static-asset/API smoke tests, and local contract tests pass. Full browser ad-interruption/resume rehearsal remains open.

**Build**

- Generate VMAP XML from accepted breaks and selected creatives.
- Generate debug JSON containing the full decision graph.
- Implement the player sequence: programme segment → ad → programme continuation.
- Track player events: break start, ad start, ad complete, resume, error, and skip/fail-closed behaviour.
- Make the programme/ad transition visible in the demo UI.
- Generate black-screen placeholder MP4s on a developer machine from `assets/brands.json`; the service never creates or transcodes ad media. Brand targeting and negative-context metadata remain separate from the video files.

**Tests**

- VMAP XML schema/contract validation
- Break order and duration consistency
- Ad URL and programme URL validation
- Playback starts at the correct programme time
- Ad plays exactly once per selected break
- Programme resumes at the correct post-break time
- Missing ad asset fails safely and resumes or skips according to policy
- Browser smoke test with a short fixture
- Verify the container includes every catalogue creative and that the static ad route supports normal browser video requests.

**Deployment**

- Package locally generated static creatives with the deployed player image and verify the production manifest/API routes.
- Test on the presentation browser and one backup browser (full playback rehearsal still open).

**E2E gate**

An evaluator can upload a video, wait for analysis, press play, see the selected ad interrupt at a generated boundary, and watch the programme resume.

This is the first true MVP gate. If this phase is not stable, stop adding features and simplify the demo.

### Phase 6 — Judge-facing UI and observability

**Build**

- Add a job progress view.
- Add timeline markers for scenes, candidate breaks, accepted breaks, and rejected candidates.
- Add an “AI evidence” panel with transcript, scene summary, confidence, model/prompt version, and brand rationale.
- Add a policy panel showing ad-load and minimum-gap calculations.
- Add downloadable VMAP and debug JSON.
- Add clear empty/error states.

**Tests**

- UI loading, success, empty, and failure states
- API/UI contract tests
- Long transcript rendering test
- Manifest and debug download tests
- Redaction test ensuring secrets are not shown in logs or UI

**Deployment**

- Promote the last passing staging image to the demo environment.
- Freeze the model, prompts, catalogue, policy, and container image versions.

**E2E gate**

Run the complete presentation script from a clean browser session with no developer tools open.

### Phase 7 — Final hardening and release rehearsal

**Build**

- Add a held-out test video not used for prompt tuning.
- Add provider timeout fallback and clear fail-closed behaviour.
- Add job cancellation and retry controls.
- Add performance timing for upload, media extraction, AI analysis, manifest creation, and first playback.
- Add a one-command reset for demo data.

**Final test suite**

- Golden clips: dialogue, silence, scene transition, emotional/mourning context, code-switched Bengali, and multiple candidate breaks
- Held-out video and held-out ninth brand
- Full browser E2E test
- Restart/retry test
- Fresh-container deployment test
- Demo browser and network rehearsal

**Release gate**

Release only when all of the following are true:

- No hard negative-context violations
- No mid-sentence breaks in the golden suite
- All pacing rules pass
- Held-out brand requires no code change
- VMAP validates
- Programme → ad → programme playback works
- A failed AI call fails closed and is visible to the operator
- The deployed version is the same version tested in rehearsal

## 6. CI/CD pipeline

### Pull request pipeline

Run on every pull request:

1. Format and lint
2. Type check
3. Unit tests
4. Schema and API contract tests
5. Fake-provider integration tests
6. Media fixture tests
7. Frontend build
8. Docker image build

Do not call paid or rate-limited AI APIs on every pull request. Use recorded model responses for deterministic tests.

### Main branch pipeline

After merge:

1. Build an immutable image tagged with the commit SHA.
2. Run the full integration suite.
3. Deploy that exact image to staging.
4. Run browser smoke tests against the deployed URL.
5. Publish test artefacts: VMAP, debug JSON, screenshots, logs, and timings.

### Demo release pipeline

Promote only the tested staging image:

1. Freeze configuration and model/prompt versions.
2. Tag the release.
3. Deploy the same image to the demo environment.
4. Run health, upload, analysis, manifest, and playback smoke tests.
5. Rehearse the presentation flow.
6. Keep the previous image available for one-click rollback.

### Required secrets and configuration

Keep these in the deployment secret store, never in Git:

- AI provider credentials
- Storage credentials, if applicable
- Allowed origins
- Maximum upload size
- Model identifiers
- Prompt version identifiers
- Policy profile identifier

Log identifiers and timings, not credentials, raw prompts containing secrets, or unnecessary video/audio data.

## 7. Evaluation scorecard

Maintain this scorecard as a machine-readable report and as a judge-facing UI view:

| Criterion | Target |
|---|---|
| Scene evidence | Every candidate has transcript/frame/shot evidence |
| Natural boundaries | No mid-sentence break in golden tests |
| Pacing | 100% compliance with configured max breaks, gap, and ad load |
| Context safety | Zero hard negative-context violations |
| Brand generalisation | Ninth brand loaded without code change |
| Explainability | Selected and rejected decisions have reasons |
| Manifest | Valid VMAP-shaped output with consistent timings |
| Playback | Ad interrupts once and programme resumes correctly |
| Live demo | New upload can complete through the deployed system |
| AI centrality | Model outputs materially determine scenes, safety scores, and brand ranking |

## 8. Time-box for an 11-hour build

This is the order to follow if implementation happens in one day:

| Time | Goal | Gate |
|---:|---|---|
| 0:00–0:45 | Phase 0 foundation and deployed health check | Staging URL works |
| 0:45–2:00 | Phase 1 upload, media metadata, fixture handling | Upload/download works |
| 2:00–4:00 | Phase 2 ASR, scenes, structured AI output | Scene evidence appears |
| 4:00–5:30 | Phase 3 break scoring and hard policy rules | Unsafe candidates rejected |
| 5:30–6:30 | Phase 4 catalogue and brand matching | Held-out brand data works |
| 6:30–8:00 | Phase 5 VMAP and playback | Programme → ad → resume works |
| 8:00–9:00 | Phase 6 judge-facing UI | Evidence is easy to inspect |
| 9:00–10:00 | Phase 7 held-out tests and hardening | Scorecard passes |
| 10:00–11:00 | Deploy freeze, rehearsal, backup demo | Presentation-ready release |

If Phase 5 is not working by 8:00, stop adding functionality. Remove optional features, shorten the demo clip, and make the core path reliable.

## 9. Creative stretch features

Only attempt these after the complete MVP passes the release gate.

### A. Emotional pacing map and “do not break here” zones

Have the model produce a tension/emotion curve across the episode. Visualise peaks, cliffhangers, revelations, and quiet valleys. The break planner should prefer low-disruption valleys while explicitly explaining why it avoided a dramatic peak.

This makes the system feel like it understands storytelling rather than merely detecting silence.

### B. Counterfactual break simulator

Add sliders for ad-load percentage, minimum gap, and maximum breaks per hour. Recompute the policy layer instantly over the same AI evidence and show how the selected breaks change. No new model call is needed.

This demonstrates that the AI produces reusable understanding while business rules remain configurable.

### C. Cultural-context safety shield

Create a Bengali entertainment-specific sensitivity taxonomy: mourning, ritual, illness, domestic conflict, religious moments, celebrations, food, and children. Show a “blocked adjacency” explanation such as:

> “Tea creative withheld: scene contains mourning context; next eligible safe valley is 38 seconds later.”

This is a memorable differentiator for a regional streaming platform.

### D. Brand adjacency and diversity planner

For a complete episode, avoid repeating the same category or creative mood too closely. The AI can propose a balanced sequence across safe brands while deterministic rules enforce no-repeat windows. Show the resulting brand-diversity timeline.

### E. Contextual bumper generation

Use a generative model to create a short synthetic transition bumper whose tone matches the selected scene without copying the programme. For example, a calm tea-brand bumper after a reflective family conversation. Keep this synthetic and clearly labelled; it is an optional visual flourish, not part of the core safety path.

## 10. Presentation narrative

The final demo should follow this order:

1. Upload a Bengali drama clip.
2. Show the AI-generated scene and context timeline.
3. Show one candidate accepted because it is a natural scene boundary.
4. Show one candidate rejected because it cuts active dialogue.
5. Show a brand blocked by a hard negative context.
6. Show the selected brand and the evidence for the match.
7. Download or inspect the generated VMAP.
8. Play the programme, let the ad interrupt, and show programme resumption.
9. Change the ad-load policy using the counterfactual simulator, if implemented.

The central message is:

> “The AI understands the story and the brand context; deterministic policy protects the viewer and the business.”
