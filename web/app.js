(() => {
  const input = document.querySelector("#video-input");
  const dropzone = document.querySelector("#dropzone");
  const progressPanel = document.querySelector("#upload-progress");
  const progressFill = document.querySelector("#progress-fill");
  const progressPercent = document.querySelector("#progress-percent");
  const progressName = document.querySelector("#progress-name");
  const progressMessage = document.querySelector("#progress-message");
  const errorPanel = document.querySelector("#upload-error");
  const assetPanel = document.querySelector("#asset-panel");
  const toast = document.querySelector("#toast");
  let activeRequest = null;
  let toastTimer = null;
  let activeJob = null;
  let catalogBrands = [];
  let playbackPlan = null;
  let breakContext = null;
  let dialogRequest = 0;
  const pollingJobs = new Set();
  const cutDialog = document.querySelector("#cut-dialog");
  const observerReport = document.querySelector("#observer-report");

  async function refreshObserver() {
    observerReport.textContent = "Reading pipeline telemetry…";
    try {
      const response = await fetch("/api/observability", { cache: "no-store" });
      if (!response.ok) throw new Error("Telemetry is unavailable.");
      const snapshot = await response.json();
      if (snapshot.status === "unavailable") {
        observerReport.textContent = "Collector is starting, or no snapshot is available yet. Analysis and playback are unaffected.";
        return;
      }
      observerReport.replaceChildren();
      const summary = document.createElement("p");
      summary.textContent = `${snapshot.events_received || 0} events · ${snapshot.packets_rejected || 0} rejected packets`;
      observerReport.append(summary);
      for (const [name, metric] of Object.entries(snapshot.metrics || {}).sort()) {
        const row = document.createElement("div");
        row.className = "observer-metric";
        const average = metric.count ? Math.round(metric.total_ms / metric.count) : 0;
        row.textContent = `${name.replaceAll("_", " ")} · ${metric.count} run${metric.count === 1 ? "" : "s"} · ${average} ms avg · ${metric.errors} errors`;
        observerReport.append(row);
      }
      const events = document.createElement("p");
      events.textContent = Object.entries(snapshot.event_counts || {}).map(([name, count]) => `${name}: ${count}`).join(" · ") || "No delivery events yet.";
      observerReport.append(events);
    } catch (error) {
      observerReport.textContent = error.message || "Telemetry is unavailable.";
    }
  }

  document.querySelector("#refresh-observer").addEventListener("click", refreshObserver);

  const formatDuration = (seconds) => {
    const total = Math.max(0, Math.floor(seconds));
    const hours = Math.floor(total / 3600);
    const minutes = Math.floor((total % 3600) / 60);
    const remainder = total % 60;
    return hours ? `${hours}:${String(minutes).padStart(2, "0")}:${String(remainder).padStart(2, "0")}` : `${minutes}:${String(remainder).padStart(2, "0")}`;
  };
  const formatPrecise = (seconds) => {
    const tenths = Math.round(Math.max(0, seconds) * 10);
    return `${formatDuration(Math.floor(tenths / 10))}.${tenths % 10}`;
  };
  const defaultCreative = (brand) => brand?.creatives.find((creative) => creative.duration_sec === 15) || brand?.creatives[0];

  const reviewStates = new Map();
  let reviewSaveTimer = null;

  const fromServerSelection = (item) => ({
    time: item.time, source: item.source, candidateId: item.candidate_id || undefined, brandId: item.brand_id || "",
    creativeId: item.creative_id || "", allowSkip: item.allow_skip, skipAfter: item.skip_after_sec,
    clickUrl: item.click_through_url || "", ctaLabel: item.cta_label || "",
  });

  function toServerSelection(item) {
    const duration = catalogBrands.find((brand) => brand.brand_id === item.brandId)?.creatives
      .find((creative) => creative.id === item.creativeId)?.duration_sec;
    return {
      time: item.time, source: item.source, candidate_id: item.candidateId || "", brand_id: item.brandId || "",
      creative_id: item.creativeId || "", allow_skip: item.allowSkip ?? true,
      skip_after_sec: item.allowSkip === false ? 0 : Number(item.skipAfter ?? defaultSkipAfter(duration)),
      click_through_url: item.clickUrl || "", cta_label: item.ctaLabel || "",
    };
  }

  function reviewState(job, candidates, maxCount) {
    if (reviewStates.has(job.id)) return reviewStates.get(job.id);
    const accepted = candidates.filter((candidate) => candidate.decision === "accepted");
    let state = null;
    if (job.review && Array.isArray(job.review.selected)) {
      state = { target: job.review.target, selected: job.review.selected.map(fromServerSelection),
        excludedAI: job.review.excluded_ai || [], finalizedRevision: job.review.finalized_revision || 0, dirty: !!job.review.dirty };
    } else {
      try {
        // Edits made before reviews were saved on the server lived only in this browser.
        const legacy = JSON.parse(localStorage.getItem(`scenesense-break-selection:${job.id}`) || "null");
        if (legacy && Array.isArray(legacy.selected)) {
          state = { target: legacy.target, selected: legacy.selected, excludedAI: legacy.excludedAI || [], migrated: true };
        }
      } catch { /* No saved browser edits. */ }
    }
    state ||= { target: accepted.length, excludedAI: [],
      selected: accepted.map((candidate) => ({ time: candidate.time, source: "ai", candidateId: candidate.candidate_id })) };
    state.finalizedRevision ||= (job.manifests || []).length;
    state.selected = state.selected.filter((item) => Number.isFinite(item.time) &&
      (item.source === "manual" || candidates.some((candidate) => candidate.candidate_id === item.candidateId && candidate.potential)))
      .sort((left, right) => left.time - right.time);
    const manual = state.selected.filter((item) => item.source === "manual").length;
    state.target = Math.min(maxCount, Math.max(manual, Number(state.target) || state.selected.length));
    reviewStates.set(job.id, state);
    return state;
  }

  function saveReview(job, state) {
    state.dirty = state.finalizedRevision > 0;
    playbackPlan = null;
    if (activeJob?.id === job.id) activeJob.playback_plan = null;
    window.clearTimeout(reviewSaveTimer);
    reviewSaveTimer = window.setTimeout(async () => {
      try {
        const response = await fetch(`/api/jobs/${encodeURIComponent(job.id)}/review`, {
          method: "PUT", headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ breaks: state.selected.map(toServerSelection), target: state.target, excluded_ai: state.excludedAI }),
        });
        if (!response.ok) throw new Error((await response.json()).error);
        job.review = await response.json();
      } catch (error) {
        showToast(error.message || "Your review could not be saved; it stays in this tab.");
      }
    }, 350);
  }

  // Ask the server's optimiser for k breaks, keeping reviewer placements and removals, and preserve ad choices.
  async function rebalance(job, state, target) {
    const response = await fetch(`/api/jobs/${encodeURIComponent(job.id)}/optimize`, {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ k: target, pinned: state.selected.filter((item) => item.source === "manual").map((item) => item.time),
        excluded: state.excludedAI }),
    });
    const result = await response.json();
    if (!response.ok) throw new Error(result.error || "The optimiser could not place these breaks.");
    const previous = new Map(state.selected.filter((item) => item.source === "ai").map((item) => [item.candidateId, item]));
    state.selected = [...state.selected.filter((item) => item.source === "manual"),
      ...result.selected.filter((entry) => entry.source === "ai").map((entry) =>
        previous.get(entry.candidate_id) || { time: entry.time, source: "ai", candidateId: entry.candidate_id })]
      .sort((left, right) => left.time - right.time);
    state.target = result.k;
    state.plan = result;
    return result;
  }

  function placementLimitMessage(state, time, duration, policy, maxCount) {
    if (!Number.isFinite(time) || time < 15 || time > duration - 10) return "Leave at least 15 seconds at the start and 10 seconds at the end.";
    if (state.selected.length >= maxCount) return `This video allows at most ${maxCount} breaks.`;
    if (state.selected.some((item) => Math.abs(item.time - time) < policy.min_gap_seconds)) return `Keep at least ${policy.min_gap_seconds} seconds between ads.`;
    const count = state.selected.length + 1;
    if (count * policy.planned_ad_seconds / (duration + count * policy.planned_ad_seconds) > policy.max_ad_load_percent / 100) return "This would exceed the ad-load limit.";
    return "";
  }

  function precedingSceneBrandMatches(transcript, time) {
    const scenes = transcript.scenes || [];
    const transitions = transcript.transitions || [];
    const atVerifiedTransition = transitions.some((item) => item.continuity === "new_scene" && Math.abs(item.time - time) <= 1.2);
    let scene = atVerifiedTransition
      ? scenes.filter((item) => item.start < time && item.end <= time + 2).sort((left, right) => right.end - left.end)[0]
      : null;
    scene ||= scenes.filter((item) => item.start <= time && time <= item.end)
      .sort((left, right) => right.start - left.start)[0];
    scene ||= scenes.filter((item) => item.start < time).sort((left, right) => right.end - left.end)[0];
    return scene?.brand_matches || [];
  }

  function renderPlaybackPlanner(job, state, persist) {
    const rows = document.querySelector("#playback-break-config");
    const build = document.querySelector("#build-vmap");
    const play = document.querySelector("#play-programme");
    const status = document.querySelector("#playback-status");
    rows.replaceChildren();
    if (playbackPlan && (playbackPlan.breaks.length !== state.selected.length || playbackPlan.breaks.some((planned, index) => {
      const selected = state.selected[index];
      return !selected || Math.abs(planned.time - selected.time) > 0.001 || planned.brand_id !== selected.brandId || planned.creative_id !== selected.creativeId;
    }))) {
      playbackPlan = null;
      if (activeJob?.id === job.id) activeJob.playback_plan = null;
    }
    build.disabled = !state.selected.length || !catalogBrands.length;
    play.disabled = !playbackPlan?.breaks?.length;
    const revision = state.finalizedRevision;
    const published = revision > 0 && !state.dirty && playbackPlan?.breaks?.length;
    document.querySelector("#playback-downloads").classList.toggle("hidden", !published);
    document.querySelector("#finalize-state").textContent = !revision ? "Draft · not finalized"
      : state.dirty ? `Draft changes since revision ${revision}` : `Final · revision ${revision}`;
    document.querySelector("#finalize-state").className = `finalize-state${revision && !state.dirty ? " finalize-state-final" : ""}`;
    if (published) {
      document.querySelector("#manifest-download").href = `/api/jobs/${encodeURIComponent(job.id)}/manifest.json?revision=${revision}`;
      document.querySelector("#vmap-download").href = playbackPlan.vmap_url;
      document.querySelector("#debug-download").href = playbackPlan.debug_url;
    }
    if (!catalogBrands.length) {
      status.textContent = "Loading the synthetic brand catalogue…";
      return;
    }
    if (!state.selected.length) {
      status.textContent = "Click a cut on the timeline to see its scene context and choose an ad.";
      return;
    }
    let allRowsReady = true;
    for (const item of state.selected) {
      const candidate = (job.transcript.break_candidates || []).find((entry) => Math.abs(entry.time - item.time) < 0.5);
      const aiBrand = candidate?.brand_recommendations?.find((match) => !match.blocked)?.brand_id;
      if (!item.brandId && aiBrand && catalogBrands.some((brand) => brand.brand_id === aiBrand)) item.brandId = aiBrand;
      const brand = catalogBrands.find((entry) => entry.brand_id === item.brandId);
      if (brand && !brand.creatives.some((creative) => creative.id === item.creativeId)) item.creativeId = defaultCreative(brand)?.id || "";
      const creative = brand?.creatives.find((entry) => entry.id === item.creativeId);
      if (!creative) allRowsReady = false;
      const row = document.createElement("div");
      row.className = "playback-break-row";
      const label = document.createElement("strong");
      label.className = "playback-break-label";
      label.textContent = `${formatDuration(item.time)} · ${item.source === "manual" ? "manual" : "AI"}`;
      const choice = document.createElement("span");
      choice.className = `playback-break-choice${creative ? "" : " playback-break-missing"}`;
      const link = item.clickUrl || brand?.click_through_url;
      choice.textContent = creative
        ? `${brand.display_name} · ${creative.duration_sec}s${brand.source === "custom" ? " · uploaded" : ""} · ${item.allowSkip === false
          ? "no skip" : `skip after ${item.skipAfter ?? defaultSkipAfter(creative.duration_sec)}s`}${link ? " · link" : ""}`
        : "No ad chosen yet";
      const edit = document.createElement("button");
      edit.type = "button";
      edit.className = "outline-button";
      edit.textContent = creative ? "Change ad" : "Choose ad";
      edit.onclick = () => openCutDialog(item.time);
      row.append(label, choice);
      if (creative) {
        const preview = document.createElement("button");
        preview.type = "button";
        preview.className = "outline-button";
        preview.textContent = "▶ Preview";
        preview.onclick = () => previewBreakFromChoice(job, item.time, item.brandId, item.creativeId, item);
        row.append(preview);
      }
      row.append(edit);
      rows.append(row);
    }
    build.disabled = !allRowsReady;
    if (!allRowsReady) {
      status.textContent = "Choose an ad for every marker: click it on the timeline or use Choose ad. If every brand is blocked after a scene, leave that break out.";
    } else if (revision && !state.dirty && playbackPlan?.breaks?.length === state.selected.length) {
      status.textContent = `Revision ${revision} is final. Download the manifest for the OTT player, or play the programme to rehearse every break.`;
    } else {
      status.textContent = "Finalizing re-checks brand safety, spacing, skip rules, and actual ad load, then writes a new manifest revision.";
    }
    build.onclick = async () => {
      build.disabled = true;
      status.textContent = "Checking brand safety, spacing, skip rules, and actual ad load, then writing the manifest…";
      try {
        const response = await fetch(`/api/jobs/${encodeURIComponent(job.id)}/finalize`, {
          method: "POST", headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ breaks: state.selected.map(toServerSelection), target: state.target, excluded_ai: state.excludedAI }),
        });
        const payload = await response.json();
        if (!response.ok) throw new Error(payload.error || "The plan could not be finalized.");
        playbackPlan = payload.plan;
        activeJob.playback_plan = playbackPlan;
        job.review = payload.review;
        job.manifests = [...(job.manifests || []), { revision: payload.revision }];
        state.finalizedRevision = payload.revision;
        state.dirty = false;
        renderPlaybackEventHistory([]);
        renderPlaybackPlanner(job, state, persist);
        showToast(`Revision ${payload.revision} finalized. The manifest is ready to download.`);
      } catch (error) {
        showToast(error.message || "Could not finalize the plan.");
        status.textContent = error.message || "Could not finalize the plan.";
        build.disabled = false;
      }
    };
    play.onclick = () => playFullProgramme(job);
  }

  function renderPlaybackEventHistory(events) {
    const list = document.querySelector("#playback-event-list");
    list.replaceChildren();
    for (const event of events.slice(-16)) {
      const item = document.createElement("li");
      item.textContent = `${event.event.replaceAll("_", " ")} · ${formatDuration(event.programme_time_sec)}`;
      list.append(item);
    }
  }

  async function recordPlaybackEvent(event, item, detail = "") {
    const entry = { event, break_id: item.break_id, programme_time_sec: item.time, detail };
    const list = document.querySelector("#playback-event-list");
    const row = document.createElement("li");
    row.textContent = `${event.replaceAll("_", " ")} · ${formatDuration(item.time)}`;
    list.append(row);
    while (list.children.length > 16) list.firstElementChild.remove();
    try {
      await fetch(`/api/jobs/${encodeURIComponent(activeJob.id)}/playback-events`, {
        method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(entry),
      });
    } catch { /* Event persistence is best-effort; playback remains local and fail-safe. */ }
  }

  const defaultSkipAfter = (durationSec) => Math.max(0, Math.min(5, (Number(durationSec) || 1) - 1));

  function previewBreakFromChoice(job, time, brandId, creativeId, options) {
    const brand = catalogBrands.find((entry) => entry.brand_id === brandId);
    const creative = brand?.creatives.find((entry) => entry.id === creativeId);
    if (!brand || !creative) return;
    const clickUrl = options.clickUrl || brand.click_through_url || "";
    const allowSkip = options.allowSkip ?? true;
    document.querySelector("#video-preview").pause();
    window.SceneSensePlayer.open({
      mode: "preview", title: job.fileName, programmeUrl: job.videoUrl, duration: job.media.durationSeconds,
      startAt: Math.max(0, time - 8), endAt: Math.min(job.media.durationSeconds, time + 6),
      breaks: [{
        id: "preview", time, brandName: brand.display_name, creativeUrl: `/${creative.url}`, durationSec: creative.duration_sec,
        allowSkip, skipAfterSec: allowSkip ? Number(options.skipAfter ?? defaultSkipAfter(creative.duration_sec)) : 0,
        clickUrl, ctaLabel: clickUrl ? (options.ctaLabel || brand.cta_label || "Visit website") : "",
      }],
    });
  }

  function playFullProgramme(job) {
    if (!playbackPlan?.breaks?.length) return;
    document.querySelector("#video-preview").pause();
    renderPlaybackEventHistory([]);
    window.SceneSensePlayer.open({
      mode: "programme", title: job.fileName, programmeUrl: job.videoUrl, duration: job.media.durationSeconds, startAt: 0,
      breaks: playbackPlan.breaks.map((item) => ({
        id: item.break_id, time: item.time, brandName: item.brand_name, creativeUrl: item.creative_url,
        durationSec: item.duration_sec, allowSkip: item.allow_skip, skipAfterSec: item.skip_after_sec,
        clickUrl: item.click_through_url || "", ctaLabel: item.cta_label || "",
      })),
      onEvent: (type, item, detail) => recordPlaybackEvent(type, { break_id: item.id, time: item.time }, detail),
    });
  }

  function tagRow(label, values, className = "") {
    const row = document.createElement("div");
    row.className = "cut-tag-row";
    const name = document.createElement("span");
    name.className = "cut-tag-label";
    name.textContent = label;
    const tags = document.createElement("div");
    tags.className = "scene-tags";
    for (const value of values.length ? values : ["none"]) {
      const tag = document.createElement("span");
      tag.className = `scene-tag${values.length ? className : ""}`;
      tag.textContent = value.replaceAll("_", " ");
      tags.append(tag);
    }
    row.append(name, tags);
    return row;
  }

  function sceneBlock(title, scene, detailed) {
    const block = document.createElement("section");
    block.className = "cut-scene";
    const heading = document.createElement("div");
    heading.className = "cut-scene-heading";
    const name = document.createElement("strong");
    name.textContent = title;
    heading.append(name);
    if (scene) {
      const meta = document.createElement("span");
      meta.textContent = `${formatDuration(scene.start)}–${formatDuration(scene.end)} · ${Math.round(scene.confidence * 100)}% confidence`;
      heading.append(meta);
    }
    const summary = document.createElement("p");
    summary.className = "cut-scene-summary";
    summary.textContent = scene?.summary || "No scene evidence covers this side of the cut.";
    block.append(heading, summary);
    if (scene && detailed) {
      block.append(
        tagRow("Scene context", scene.activities || []),
        tagRow("Mood", scene.tone || []),
        tagRow("Caution", scene.sensitive_contexts || [], " scene-tag-sensitive"),
      );
    }
    return block;
  }

  async function openCutDialog(time) {
    const context = breakContext;
    if (!context || !Number.isFinite(time)) return;
    const video = document.querySelector("#video-preview");
    video.pause();
    video.currentTime = time;
    const existing = context.state.selected.find((item) => Math.abs(item.time - time) < 0.05);
    const potential = context.candidates.find((candidate) => Math.abs(candidate.time - time) < 0.5 &&
      (candidate.potential || (candidate.potential === undefined && candidate.decision === "accepted")));
    const content = document.querySelector("#cut-dialog-content");
    const confirm = document.querySelector("#cut-dialog-confirm");
    document.querySelector("#cut-dialog-kind").textContent = existing ? "SELECTED AD BREAK" : potential ? "AI POTENTIAL BREAK" : "SHOT CUT";
    document.querySelector("#cut-dialog-title").textContent = `Ad break at ${formatPrecise(time)}`;
    document.querySelector("#cut-dialog-status").textContent = "";
    document.querySelector("#cut-dialog-remove").classList.toggle("hidden", !existing);
    confirm.disabled = true;
    document.querySelector("#cut-dialog-preview").disabled = true;
    confirm.textContent = existing ? "Update ad break" : "Add ad break";
    const loading = document.createElement("p");
    loading.className = "cut-dialog-note";
    loading.textContent = "Reading the scene and ranking ads…";
    content.replaceChildren(loading);
    if (!cutDialog.open) cutDialog.showModal();
    const request = ++dialogRequest;
    let payload;
    try {
      const response = await fetch(`/api/jobs/${encodeURIComponent(context.job.id)}/ad-suggestions?time=${encodeURIComponent(time)}`);
      payload = await response.json();
      if (!response.ok) throw new Error(payload.error || "Ad suggestions are unavailable.");
    } catch (error) {
      if (request === dialogRequest) loading.textContent = error.message || "Ad suggestions are unavailable.";
      return;
    }
    if (request !== dialogRequest || !cutDialog.open) return;
    renderCutDialog(payload, time, existing, context);
  }

  function renderCutDialog(payload, time, existing, context) {
    const { job, state, persist, policy, maxCount, duration, candidates } = context;
    const content = document.querySelector("#cut-dialog-content");
    const status = document.querySelector("#cut-dialog-status");
    const confirm = document.querySelector("#cut-dialog-confirm");
    const remove = document.querySelector("#cut-dialog-remove");
    content.replaceChildren();

    const notes = [];
    const candidate = payload.break_candidate;
    if (candidate?.potential) {
      notes.push(`AI potential break · naturalness ${Math.round(candidate.naturalness * 100)}% · disruption ${Math.round(candidate.disruption_risk * 100)}%`);
    } else if (candidate) {
      notes.push(`AI withheld this moment: ${(candidate.reasons || []).map((reason) => reason.message).join(" ") || "it did not pass the break policy."}`);
    } else {
      notes.push("Not an AI break candidate. Adding an ad here is a human review choice.");
    }
    if (payload.transition) {
      notes.push(payload.transition.continuity === "new_scene"
        ? `Visual check: new scene (${payload.transition.kind.replaceAll("_", " ")})`
        : payload.transition.continuity === "same_scene" ? "Visual check: same scene, camera change only" : "Visual check: uncertain transition");
    }
    const note = document.createElement("p");
    note.className = `cut-dialog-note${candidate?.potential ? " cut-dialog-note-ok" : ""}`;
    note.textContent = notes.join(" · ");
    content.append(note, sceneBlock("Scene before the cut · the ad follows this", payload.scene_before, true));
    const sameScene = payload.scene_after && payload.scene_before && payload.scene_after.scene_id === payload.scene_before.scene_id;
    content.append(sceneBlock(sameScene ? "After the cut · the same scene continues" : "Scene after the cut", sameScene ? null : payload.scene_after, false));
    if (sameScene) content.lastElementChild.querySelector("p").textContent = "The cut falls inside one scene, so an ad here would interrupt it.";

    const suggestions = payload.suggestions || [];
    const recommended = suggestions.filter((item) => item.recommended);
    const blockedCount = suggestions.filter((item) => item.blocked).length;
    const heading = document.createElement("div");
    heading.className = "cut-suggestions-heading";
    const headingTitle = document.createElement("strong");
    headingTitle.textContent = "Suggested ads";
    const headingMeta = document.createElement("span");
    headingMeta.textContent = `${recommended.length} recommended · ${suggestions.length - blockedCount - recommended.length} other safe · ${blockedCount} blocked`;
    heading.append(headingTitle, headingMeta);
    const list = document.createElement("div");
    list.className = "cut-suggestions";
    list.setAttribute("role", "radiogroup");
    list.setAttribute("aria-label", "Brand for this ad break");

    let selectedBrand = existing?.brandId && suggestions.some((item) => item.brand_id === existing.brandId && !item.blocked)
      ? existing.brandId : recommended[0]?.brand_id || "";
    let selectedCreative = existing?.creativeId || "";
    const issue = existing ? "" : placementLimitMessage(state, time, duration, policy, maxCount);
    const durationLabel = document.createElement("label");
    durationLabel.className = "cut-duration";
    durationLabel.textContent = "Ad duration";
    const durationSelect = document.createElement("select");
    durationLabel.append(durationSelect);

    const viewer = document.createElement("fieldset");
    viewer.className = "cut-viewer-options";
    const legend = document.createElement("legend");
    legend.textContent = "Viewer experience";
    const skipToggle = document.createElement("label");
    skipToggle.className = "cut-switch";
    const allowSkip = document.createElement("input");
    allowSkip.type = "checkbox";
    allowSkip.checked = existing?.allowSkip ?? true;
    const switchTrack = document.createElement("span");
    switchTrack.className = "cut-switch-track";
    switchTrack.setAttribute("aria-hidden", "true");
    skipToggle.append(allowSkip, switchTrack, document.createTextNode("Allow skip"));
    const skipAfterLabel = document.createElement("label");
    skipAfterLabel.className = "cut-inline-field";
    skipAfterLabel.append(document.createTextNode("Skip after"));
    const skipAfter = document.createElement("input");
    skipAfter.type = "number";
    skipAfter.min = "0";
    skipAfter.step = "1";
    skipAfter.inputMode = "numeric";
    const seconds = document.createElement("span");
    seconds.textContent = "seconds";
    skipAfterLabel.append(skipAfter, seconds);
    const skipRow = document.createElement("div");
    skipRow.className = "cut-viewer-row";
    skipRow.append(skipToggle, skipAfterLabel);
    const linkLabel = document.createElement("label");
    linkLabel.className = "cut-field";
    linkLabel.append(document.createTextNode("Website link"));
    const linkInput = document.createElement("input");
    linkInput.type = "url";
    linkInput.inputMode = "url";
    linkInput.maxLength = 500;
    linkInput.placeholder = "https://brand.example/app";
    linkInput.value = existing?.clickUrl || "";
    const linkHint = document.createElement("span");
    linkHint.className = "field-hint";
    linkLabel.append(linkInput, linkHint);
    const ctaLabelField = document.createElement("label");
    ctaLabelField.className = "cut-field cut-field-short";
    ctaLabelField.append(document.createTextNode("Button label"));
    const ctaInput = document.createElement("input");
    ctaInput.maxLength = 24;
    ctaInput.setAttribute("list", "cta-label-options");
    ctaInput.value = existing?.ctaLabel || "";
    ctaLabelField.append(ctaInput);
    const linkRow = document.createElement("div");
    linkRow.className = "cut-viewer-row cut-viewer-row-wide";
    linkRow.append(linkLabel, ctaLabelField);
    viewer.append(legend, skipRow, linkRow);

    const preview = document.querySelector("#cut-dialog-preview");
    const creativeSeconds = () => suggestions.find((item) => item.brand_id === selectedBrand)?.creatives
      .find((creative) => creative.creative_id === selectedCreative)?.duration_sec || 0;
    let skipTouched = existing?.skipAfter !== undefined;
    skipAfter.value = String(existing?.skipAfter ?? 5);
    const viewerProblem = () => {
      const duration = creativeSeconds();
      const wait = Number(skipAfter.value);
      if (allowSkip.checked && (!Number.isInteger(wait) || wait < 0 || (duration && wait >= duration))) {
        return `Skip must unlock between 0 and ${Math.max(0, duration - 1)} seconds for this ${duration}-second ad.`;
      }
      const link = linkInput.value.trim();
      if (link) {
        try {
          const parsed = new URL(link);
          if (!/^https?:$/.test(parsed.protocol)) throw new Error();
        } catch {
          return "The website link must start with http:// or https://.";
        }
      }
      return "";
    };
    const update = () => {
      const brand = catalogBrands.find((entry) => entry.brand_id === selectedBrand);
      skipAfter.disabled = !allowSkip.checked;
      skipAfterLabel.classList.toggle("is-disabled", !allowSkip.checked);
      if (creativeSeconds()) skipAfter.max = String(creativeSeconds() - 1);
      if (!skipTouched && creativeSeconds()) skipAfter.value = String(defaultSkipAfter(creativeSeconds()));
      const libraryLink = brand?.click_through_url;
      linkHint.textContent = libraryLink
        ? `Leave blank to use the library link (${libraryLink.replace(/^https?:\/\/(www\.)?/, "").split("/")[0]}).`
        : "Optional. Opens in a new tab from the ad and pauses it.";
      ctaInput.placeholder = brand?.cta_label || "Visit website";
      const problem = viewerProblem();
      confirm.disabled = Boolean(issue) || !selectedBrand || !selectedCreative || Boolean(problem);
      preview.disabled = !selectedBrand || !selectedCreative || Boolean(problem);
      status.textContent = issue || problem || (suggestions.every((item) => item.blocked)
        ? "Every ad is blocked or uncertain after this scene, so the break stays empty."
        : selectedBrand ? "" : "Choose a brand that is not blocked.");
    };
    allowSkip.onchange = update;
    skipAfter.oninput = () => { skipTouched = true; update(); };
    linkInput.oninput = update;
    ctaInput.oninput = update;
    const viewerChoice = () => {
      const clickUrl = linkInput.value.trim();
      const hasLink = clickUrl || catalogBrands.find((entry) => entry.brand_id === selectedBrand)?.click_through_url;
      return { allowSkip: allowSkip.checked, skipAfter: allowSkip.checked ? Number(skipAfter.value) : 0,
        clickUrl, ctaLabel: hasLink ? ctaInput.value.trim() : "" };
    };
    preview.onclick = () => previewBreakFromChoice(job, time, selectedBrand, selectedCreative, viewerChoice());
    const renderDurations = () => {
      const suggestion = suggestions.find((item) => item.brand_id === selectedBrand);
      const creatives = suggestion?.creatives || [];
      durationSelect.replaceChildren();
      for (const creative of creatives) {
        const option = document.createElement("option");
        option.value = creative.creative_id;
        option.textContent = `${creative.duration_sec} seconds · ${creative.language}`;
        durationSelect.append(option);
      }
      if (!creatives.some((creative) => creative.creative_id === selectedCreative)) {
        selectedCreative = (creatives.find((creative) => creative.duration_sec === 15) || creatives[0])?.creative_id || "";
      }
      durationSelect.value = selectedCreative;
      durationSelect.disabled = !creatives.length;
      update();
    };
    durationSelect.onchange = () => { selectedCreative = durationSelect.value; update(); };

    for (const suggestion of suggestions) {
      const option = document.createElement("label");
      option.className = `cut-suggestion${suggestion.blocked ? " cut-suggestion-blocked" : ""}${suggestion.recommended ? " cut-suggestion-recommended" : ""}`;
      const radio = document.createElement("input");
      radio.type = "radio";
      radio.name = "cut-brand";
      radio.value = suggestion.brand_id;
      radio.disabled = suggestion.blocked;
      radio.checked = suggestion.brand_id === selectedBrand;
      radio.onchange = () => { selectedBrand = radio.value; renderDurations(); };
      const body = document.createElement("span");
      body.className = "cut-suggestion-body";
      const top = document.createElement("span");
      top.className = "cut-suggestion-top";
      const name = document.createElement("strong");
      name.textContent = suggestion.display_name;
      const source = document.createElement("span");
      source.className = `ad-source-badge${suggestion.source === "custom" ? " ad-source-custom" : ""}`;
      source.textContent = suggestion.source === "custom" ? "UPLOADED" : "BUILT-IN";
      const fit = document.createElement("span");
      fit.className = "cut-fit";
      fit.textContent = suggestion.blocked ? "Blocked"
        : `${Math.round(suggestion.fit_score * 100)}% ${suggestion.fit_source === "ai" ? "AI fit" : "context match"}${suggestion.recommended ? " · suggested" : ""}`;
      top.append(name, source, fit);
      const reason = document.createElement("span");
      reason.className = "cut-suggestion-reason";
      reason.textContent = suggestion.blocked
        ? `Blocked by scene context: ${(suggestion.blocked_contexts || []).map((value) => value.replaceAll("_", " ")).join(", ")}`
        : suggestion.reason;
      const detail = document.createElement("span");
      detail.className = "cut-suggestion-detail";
      const matched = suggestion.matched_contexts || [];
      detail.textContent = `${matched.length ? `Matches: ${matched.join(", ")}` : `Targets: ${(suggestion.target_contexts || []).slice(0, 5).join(", ") || "—"}`} · ${suggestion.creatives.map((creative) => `${creative.duration_sec}s`).join(" / ")}`;
      body.append(top, reason, detail);
      option.append(radio, body);
      list.append(option);
    }
    content.append(heading, list, durationLabel, viewer);
    renderDurations();

    confirm.onclick = async () => {
      if (existing) {
        Object.assign(existing, { brandId: selectedBrand, creativeId: selectedCreative, ...viewerChoice() });
      } else {
        const nearby = candidates.find((item) => (item.potential || (item.potential === undefined && item.decision === "accepted")) &&
          Math.abs(item.time - time) < 0.5);
        if (nearby) state.excludedAI = state.excludedAI.filter((id) => id !== nearby.candidate_id);
        state.selected.push({ time, source: "manual", ...(nearby ? { candidateId: nearby.candidate_id } : {}),
          brandId: selectedBrand, creativeId: selectedCreative, ...viewerChoice() });
        state.selected.sort((left, right) => left.time - right.time);
        try {
          await rebalance(job, state, Math.min(maxCount, state.target + 1));
        } catch (error) {
          showToast(error.message);
        }
      }
      persist();
      cutDialog.close();
      renderBreakDecisions(job);
      const brand = suggestions.find((item) => item.brand_id === selectedBrand);
      showToast(`${brand?.display_name || "Ad"} placed at ${formatPrecise(time)}.`);
    };
    remove.onclick = async () => {
      if (!existing) return;
      if (existing.candidateId && !state.excludedAI.includes(existing.candidateId)) state.excludedAI.push(existing.candidateId);
      state.selected = state.selected.filter((item) => item !== existing);
      try {
        await rebalance(job, state, Math.max(0, state.target - 1));
      } catch (error) {
        showToast(error.message);
      }
      persist();
      cutDialog.close();
      renderBreakDecisions(job);
    };
  }

  function renderAdLibrary() {
    const list = document.querySelector("#ad-library-list");
    const names = document.querySelector("#custom-brand-names");
    list.replaceChildren();
    names.replaceChildren();
    const custom = catalogBrands.filter((brand) => brand.source === "custom");
    document.querySelector("#ad-library-count").textContent = `${catalogBrands.length} brands · ${custom.length} uploaded`;
    for (const brand of custom) {
      const option = document.createElement("option");
      option.value = brand.display_name;
      names.append(option);
    }
    for (const brand of [...custom].reverse().concat(catalogBrands.filter((entry) => entry.source !== "custom"))) {
      const item = document.createElement("li");
      item.className = "ad-library-item";
      const top = document.createElement("div");
      top.className = "ad-library-top";
      const name = document.createElement("strong");
      name.textContent = brand.display_name;
      const source = document.createElement("span");
      source.className = `ad-source-badge${brand.source === "custom" ? " ad-source-custom" : ""}`;
      source.textContent = brand.source === "custom" ? "UPLOADED" : "BUILT-IN";
      top.append(name, source);
      if (brand.source === "custom") {
        const remove = document.createElement("button");
        remove.type = "button";
        remove.className = "ad-remove";
        remove.textContent = "Remove";
        remove.setAttribute("aria-label", `Remove ${brand.display_name} and all its ads`);
        remove.onclick = () => removeAds(brand, null);
        top.append(remove);
      }
      const meta = document.createElement("p");
      meta.className = "ad-library-meta";
      meta.textContent = `${brand.category || "uncategorised"} · ${[...new Set(brand.creatives.map((creative) => creative.duration_sec))].sort((a, b) => a - b).map((seconds) => `${seconds}s`).join(" / ")}`;
      if (brand.click_through_url) {
        const link = document.createElement("a");
        link.className = "ad-library-link";
        link.href = brand.click_through_url;
        link.target = "_blank";
        link.rel = "noopener noreferrer";
        link.textContent = `${brand.cta_label || "Visit website"} ↗ ${brand.click_through_url.replace(/^https?:\/\/(www\.)?/, "").split("/")[0]}`;
        meta.append(" · ", link);
      }
      const tags = document.createElement("div");
      tags.className = "scene-tags";
      const targets = brand.target_contexts || [];
      for (const value of targets.slice(0, 5)) {
        const tag = document.createElement("span");
        tag.className = "scene-tag";
        tag.textContent = value;
        tags.append(tag);
      }
      if (targets.length > 5) {
        const more = document.createElement("span");
        more.className = "pause-more";
        more.textContent = `+${targets.length - 5}`;
        tags.append(more);
      }
      for (const value of brand.negative_contexts || []) {
        const tag = document.createElement("span");
        tag.className = "scene-tag scene-tag-sensitive";
        tag.textContent = `not ${value}`;
        tags.append(tag);
      }
      item.append(top, meta);
      if (brand.source === "custom") {
        const creatives = document.createElement("div");
        creatives.className = "ad-creative-chips";
        for (const creative of brand.creatives) {
          const chip = document.createElement("span");
          chip.className = "ad-creative-chip";
          chip.textContent = `${creative.duration_sec}s · ${creative.language}`;
          const remove = document.createElement("button");
          remove.type = "button";
          remove.textContent = "×";
          remove.setAttribute("aria-label", `Remove the ${creative.duration_sec}-second ${brand.display_name} ad`);
          remove.onclick = () => removeAds(brand, creative);
          chip.append(remove);
          creatives.append(chip);
        }
        item.append(creatives);
      }
      item.append(tags);
      list.append(item);
    }
  }

  async function removeAds(brand, creative) {
    const last = creative && brand.creatives.length === 1;
    const question = creative
      ? `Remove the ${creative.duration_sec}-second ${brand.display_name} ad?${last ? " It is this brand's only ad, so the brand is removed too." : ""}`
      : `Remove ${brand.display_name} and all ${brand.creatives.length} of its ads?`;
    if (!window.confirm(`${question} Breaks that use it will need a new ad; finalized manifests keep their record.`)) return;
    const path = `/api/ads/${encodeURIComponent(brand.brand_id)}${creative ? `/${encodeURIComponent(creative.id)}` : ""}`;
    try {
      const response = await fetch(path, { method: "DELETE" });
      const payload = await response.json();
      if (!response.ok) throw new Error(payload.error || "The ad could not be removed.");
      await loadCatalog();
      showToast(payload.brand_removed ? `${brand.display_name} was removed from the ad library.` : "The ad was removed.");
    } catch (error) {
      showToast(error.message || "The ad could not be removed.");
    }
  }

  async function loadCatalog() {
    const response = await fetch("/api/brands");
    const payload = await response.json();
    if (!response.ok) throw new Error(payload.error || "The ad library could not be loaded.");
    catalogBrands = payload.brands || [];
    renderAdLibrary();
    if (activeJob?.transcript) renderBreakDecisions(activeJob);
  }

  // Zoomable timeline. Zoom always anchors on the playhead: it keeps its place on screen, or the
  // view centres on it when it is off screen. Markers are positioned in percent of the track.
  const timelineViewport = document.querySelector("#timeline-viewport");
  const timelineTrack = document.querySelector("#timeline-track");
  const timelinePlayhead = document.querySelector("#timeline-playhead");
  const timeline = { jobId: null, zoom: 1, duration: 0, following: false, startFraction: 0, trackWidth: 0 };
  const MIN_VISIBLE_SECONDS = 10;
  const maxZoom = () => Math.max(1, Math.min(256, timeline.duration / MIN_VISIBLE_SECONDS));
  const spanLabel = (seconds) => seconds < 90 ? `${Math.round(seconds)} s` : formatDuration(seconds);

  function renderTimeline(job) {
    const duration = Number(job.media?.durationSeconds) || 0;
    if (timeline.jobId !== job.id) {
      Object.assign(timeline, { jobId: job.id, zoom: 1 });
      timelineViewport.scrollLeft = 0;
    }
    timeline.duration = duration;
    timeline.zoom = Math.min(timeline.zoom, maxZoom());
    timelineTrack.style.width = `${timeline.zoom * 100}%`;
    timelineTrack.dataset.detail = timeline.zoom >= 3 ? "on" : "off";
    timeline.trackWidth = timelineTrack.clientWidth;
    const bands = document.querySelector("#timeline-scenes");
    bands.replaceChildren();
    for (const scene of job.transcript?.scenes || []) {
      if (!duration) break;
      const band = document.createElement("span");
      band.className = `timeline-scene${scene.sensitive_contexts?.length ? " timeline-scene-sensitive" : ""}`;
      band.style.left = `${scene.start / duration * 100}%`;
      band.style.width = `${Math.max(0, scene.end - scene.start) / duration * 100}%`;
      band.textContent = (scene.tone || []).slice(0, 2).join(", ") || scene.summary;
      bands.append(band);
    }
    renderEmotion(job);
    renderRuler();
    updatePlayhead(false);
    updateZoomControls();
  }

  // Emotional pacing lane: an SVG area stretched across the track, so it zooms with the timeline.
  function renderEmotion(job) {
    const lane = document.querySelector("#timeline-emotion");
    const summary = document.querySelector("#pacing-summary");
    lane.replaceChildren();
    const pacing = job.transcript?.pacing;
    const duration = timeline.duration;
    if (!pacing?.tension?.length || !duration) {
      summary.classList.toggle("hidden", !job.transcript);
      summary.textContent = job.transcript ? "Re-run scene analysis to see the emotional pacing map." : "";
      if (job.transcript) {
        const note = document.createElement("span");
        note.className = "emotion-empty";
        note.textContent = "Emotional pacing appears after scene analysis.";
        lane.append(note);
      }
      return;
    }
    const top = [...pacing.peaks].sort((a, b) => b.value - a.value)[0];
    summary.classList.remove("hidden");
    summary.innerHTML = "";
    const lead = document.createElement("strong");
    lead.textContent = "Emotional pacing: ";
    summary.append(lead, pacing.summary);
    const count = pacing.tension.length;
    const svgNS = "http://www.w3.org/2000/svg";
    const svg = document.createElementNS(svgNS, "svg");
    const width = count * pacing.step_sec / duration * 1000;
    svg.setAttribute("viewBox", "0 0 1000 100");
    svg.setAttribute("preserveAspectRatio", "none");
    const points = pacing.tension.map((value, index) => `${(index * pacing.step_sec / duration * 1000).toFixed(2)},${(100 - value * 96).toFixed(1)}`);
    svg.innerHTML = '<defs><linearGradient id="emotion-fill" x1="0" y1="0" x2="0" y2="1">' +
      '<stop offset="0%" stop-color="#c8553d" stop-opacity=".55"/><stop offset="55%" stop-color="#e2a360" stop-opacity=".35"/>' +
      '<stop offset="100%" stop-color="#8fbf95" stop-opacity=".2"/></linearGradient></defs>';
    const mid = document.createElementNS(svgNS, "line");
    Object.entries({ class: "emotion-mid", x1: 0, x2: 1000, y1: 52, y2: 52 }).forEach(([key, value]) => mid.setAttribute(key, value));
    const area = document.createElementNS(svgNS, "path");
    area.setAttribute("class", "emotion-area");
    area.setAttribute("d", `M0,100 L${points.join(" L")} L${Math.min(1000, width).toFixed(2)},100 Z`);
    const line = document.createElementNS(svgNS, "path");
    line.setAttribute("class", "emotion-line");
    line.setAttribute("d", `M${points.join(" L")}`);
    svg.append(mid, area, line);
    lane.append(svg);
    for (const peak of pacing.peaks) {
      if (peak.start !== undefined && peak.end !== undefined) {
        const stretch = document.createElement("span");
        stretch.className = `emotion-stretch${peak.kind === "cliffhanger" ? " emotion-stretch-cliffhanger" : ""}`;
        stretch.style.left = `${peak.start / duration * 100}%`;
        stretch.style.width = `${Math.max(0, peak.end - peak.start) / duration * 100}%`;
        lane.append(stretch);
      }
      const marker = document.createElement("span");
      marker.className = `emotion-peak${peak.kind === "cliffhanger" ? " emotion-peak-cliffhanger" : ""}`;
      marker.style.left = `${peak.time / duration * 100}%`;
      marker.style.top = `${100 - peak.value * 96}%`;
      const label = document.createElement("span");
      label.textContent = `${peak.kind === "cliffhanger" ? "cliffhanger" : "peak"}${peak.label ? ` · ${peak.label}` : ""}${peak === top ? " · most intense" : ""}`;
      marker.append(label);
      lane.append(marker);
    }
    for (const valley of pacing.valleys) {
      const marker = document.createElement("span");
      marker.className = "emotion-valley";
      marker.style.left = `${valley.time / duration * 100}%`;
      const label = document.createElement("span");
      label.textContent = "calm";
      marker.append(label);
      lane.append(marker);
    }
  }

  function pacingAt(pacing, time) {
    const index = Math.min(pacing.tension.length - 1, Math.max(0, Math.round(time / pacing.step_sec)));
    return { tension: pacing.tension[index], scene: pacing.components?.scene?.[index],
      audio: pacing.components?.audio?.[index], cuts: pacing.components?.cuts?.[index] };
  }

  // The most recent dramatic peak shortly before a break, if any: the break then lands after a dramatic beat.
  function peakBefore(pacing, time, window = 90) {
    return (pacing?.peaks || []).filter((peak) => peak.time <= time && time - (peak.end ?? peak.time) <= window)
      .sort((a, b) => b.time - a.time)[0];
  }

  function renderRuler() {
    const ruler = document.querySelector("#timeline-ruler");
    ruler.replaceChildren();
    const duration = timeline.duration;
    if (!duration) return;
    const visible = duration / timeline.zoom;
    const interval = [1, 2, 5, 10, 15, 30, 60, 120, 300, 600, 900, 1800, 3600].find((step) => visible / step <= 8) || 3600;
    for (let at = 0; at < duration; at += interval) {
      const tick = document.createElement("span");
      tick.className = "timeline-tick";
      tick.style.left = `${at / duration * 100}%`;
      const label = document.createElement("span");
      label.textContent = formatDuration(at);
      tick.append(label);
      ruler.append(tick);
    }
  }

  function updateZoomControls() {
    const duration = timeline.duration;
    const zoom = timeline.zoom;
    document.querySelector("#zoom-level").value = `${zoom < 10 ? Math.round(zoom * 10) / 10 : Math.round(zoom)}×`;
    document.querySelector("#zoom-in").disabled = !duration || zoom >= maxZoom() - 0.001;
    document.querySelector("#zoom-out").disabled = zoom <= 1.001;
    document.querySelector("#zoom-fit").disabled = zoom <= 1.001;
    const width = timelineTrack.clientWidth || 1;
    const start = timelineViewport.scrollLeft / width * duration;
    const visible = duration / zoom;
    document.querySelector("#timeline-window").textContent = !duration ? "Whole programme"
      : zoom <= 1.001 ? `Whole programme · ${formatDuration(duration)}`
        : `${formatDuration(start)}–${formatDuration(Math.min(duration, start + visible))} · ${spanLabel(visible)} visible`;
  }

  function setZoom(next) {
    const duration = timeline.duration;
    if (!duration) return;
    next = Math.min(maxZoom(), Math.max(1, next));
    const viewWidth = timelineViewport.clientWidth;
    const fraction = Math.min(1, Math.max(0, (previewVideo.currentTime || 0) / duration));
    let anchor = fraction * timelineTrack.clientWidth - timelineViewport.scrollLeft;
    if (anchor < 0 || anchor > viewWidth) anchor = viewWidth / 2;
    timeline.zoom = next;
    timelineTrack.style.width = `${next * 100}%`;
    timelineTrack.dataset.detail = next >= 3 ? "on" : "off";
    timelineViewport.scrollLeft = fraction * timelineTrack.clientWidth - anchor;
    timeline.trackWidth = timelineTrack.clientWidth;
    timeline.startFraction = timelineViewport.scrollLeft / (timeline.trackWidth || 1);
    renderRuler();
    updateZoomControls();
  }

  function updatePlayhead(follow) {
    const duration = timeline.duration;
    if (!duration) return;
    const fraction = Math.min(1, Math.max(0, (previewVideo.currentTime || 0) / duration));
    timelinePlayhead.style.left = `${fraction * 100}%`;
    if (!follow || timeline.zoom <= 1.001) return;
    const x = fraction * timelineTrack.clientWidth - timelineViewport.scrollLeft;
    const viewWidth = timelineViewport.clientWidth;
    if (x < 0 || x > viewWidth * 0.92) timelineViewport.scrollLeft = fraction * timelineTrack.clientWidth - viewWidth * 0.1;
  }

  function followPlayback() {
    if (previewVideo.paused || previewVideo.ended) {
      timeline.following = false;
      return;
    }
    updatePlayhead(true);
    window.requestAnimationFrame(followPlayback);
  }

  const humanSize = (bytes) => {
    if (bytes < 1024 * 1024) return `${Math.max(1, Math.round(bytes / 1024))} KB`;
    return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
  };

  function showToast(message) {
    toast.textContent = message;
    toast.classList.add("visible");
    window.clearTimeout(toastTimer);
    toastTimer = window.setTimeout(() => toast.classList.remove("visible"), 3400);
  }

  function setProgress(percent, message) {
    const bounded = Math.max(0, Math.min(100, percent));
    progressFill.style.width = `${bounded}%`;
    progressPercent.textContent = `${Math.round(bounded)}%`;
    progressMessage.textContent = message;
  }

  function setBusy(file) {
    errorPanel.classList.add("hidden");
    dropzone.classList.add("hidden");
    assetPanel.classList.add("hidden");
    progressPanel.classList.remove("hidden");
    progressName.textContent = file.name;
    setProgress(0, `Preparing ${humanSize(file.size)} upload…`);
  }

  function showError(message) {
    progressPanel.classList.add("hidden");
    dropzone.classList.remove("hidden");
    errorPanel.textContent = message;
    errorPanel.classList.remove("hidden");
  }

  function showJob(job, { notify = false, poll = true } = {}) {
    if (activeJob?.id !== job.id) playbackPlan = null;
    activeJob = job;
    if (job.playback_plan) playbackPlan = job.playback_plan;
    progressPanel.classList.add("hidden");
    errorPanel.classList.add("hidden");
    dropzone.classList.add("hidden");
    assetPanel.classList.remove("hidden");
    document.querySelector("#asset-filename").textContent = job.fileName;
    document.querySelector("#asset-resolution").textContent = `${job.media.width} × ${job.media.height} · ${job.media.videoCodec.toUpperCase()} · ${humanSize(job.fileSize || 0)}`;
    document.querySelector("#asset-duration").textContent = formatDuration(job.media.durationSeconds);
    document.querySelector("#asset-audio").textContent = job.media.audioCodec ? job.media.audioCodec.toUpperCase() : "Not found";
    document.querySelector("#asset-description").textContent = job.message;
    document.querySelector("#timeline-empty-title").textContent = job.transcript ? "Hear the story, moment by moment." : "The story comes first.";
    document.querySelector("#timeline-empty-copy").textContent = job.message;
    renderPhaseStatus(job);
    renderTranscript(job);
    renderSceneEvidence(job);
    renderBreakDecisions(job);
    renderTimeline(job);
    renderPlaybackEventHistory(job.playback_events || []);

    const video = document.querySelector("#video-preview");
    if (video.getAttribute("src") !== job.videoUrl) {
      video.src = job.videoUrl;
      video.load();
    }
    addToLibrary(job);
    const badge = document.querySelector("#analysis-badge");
    badge.textContent = job.status === "processing" || job.status === "queued" ? "AI WORKING" : job.transcript?.break_scoring_status === "complete" ? "BREAKS REVIEWED" : job.transcript?.scene_analysis_status === "complete" ? "SCENES READY" : job.transcript ? "TRANSCRIPT READY" : job.status === "failed" ? "NEEDS ATTENTION" : "INTAKE READY";
    badge.classList.toggle("analysis-active", job.status === "processing" || job.status === "queued");
    badge.classList.toggle("analysis-complete", Boolean(job.transcript));
    if (notify) showToast(job.transcript ? "Bengali transcript is ready." : "Video intake complete.");
    if (poll && (job.status === "queued" || job.status === "processing")) watchJob(job.id);
  }

  function renderTranscript(job) {
    const panel = document.querySelector("#transcript-panel");
    const title = document.querySelector("#transcript-title");
    const meta = document.querySelector("#transcript-meta");
    const empty = document.querySelector("#transcript-empty");
    const list = document.querySelector("#transcript-list");
    panel.classList.remove("hidden");
    list.replaceChildren();

    if (job.transcript) {
      title.textContent = "Bengali transcript";
      const repaired = job.transcript.timestamp_adjustments || 0;
      meta.textContent = `${job.transcript.language || "Language detected"} · ${job.transcript.model} · ${job.transcript.segments.length} timestamped segments${repaired ? ` · ${repaired} tail timestamps adjusted to clip bounds` : ""}`;
      empty.classList.add("hidden");
      list.classList.remove("hidden");
      for (const segment of job.transcript.segments) {
        const item = document.createElement("li");
        const seek = document.createElement("button");
        seek.className = "transcript-time";
        seek.type = "button";
        seek.textContent = formatDuration(segment.start);
        seek.setAttribute("aria-label", `Play from ${formatDuration(segment.start)}`);
        seek.addEventListener("click", () => {
          const video = document.querySelector("#video-preview");
          video.currentTime = segment.start;
          video.play().catch(() => {});
        });
        const text = document.createElement("span");
        text.className = "transcript-text";
        text.textContent = segment.text;
        item.append(seek, text);
        list.append(item);
      }
      return;
    }

    list.classList.add("hidden");
    empty.classList.remove("hidden");
    if (job.status === "queued" || job.status === "processing") {
      title.textContent = "Listening to the story…";
      meta.textContent = "AI evidence · Bengali speech recognition";
      empty.textContent = job.message;
    } else if (job.status === "failed") {
      title.textContent = "Transcription needs another try";
      meta.textContent = "Your uploaded video is safe; the analysis can be retried.";
      empty.textContent = job.message;
    } else {
      title.textContent = "Bengali transcript";
      meta.textContent = "AI evidence will appear here.";
      empty.textContent = job.message || "AI transcription is not configured for this service yet.";
    }
  }

  function renderSceneEvidence(job) {
    const panel = document.querySelector("#scene-evidence");
    const meta = document.querySelector("#scene-evidence-meta");
    const warning = document.querySelector("#scene-warning");
    const sceneList = document.querySelector("#scene-list");
    const pausePanel = document.querySelector("#pause-evidence");
    const pauseList = document.querySelector("#pause-list");
    const transitionPanel = document.querySelector("#transition-evidence");
    const transitionList = document.querySelector("#transition-list");
    const transcript = job.transcript;
    panel.classList.toggle("hidden", !transcript);
    sceneList.replaceChildren();
    pauseList.replaceChildren();
    transitionList.replaceChildren();
    warning.classList.add("hidden");
    pausePanel.classList.add("hidden");
    transitionPanel.classList.add("hidden");
    if (!transcript) return;

    const scenes = transcript.scenes || [];
    const pauses = transcript.silence_intervals || [];
    const transitions = transcript.transitions || [];
    const shotBoundaries = transcript.shot_boundaries || [];
    meta.textContent = `${scenes.length} scenes · ${transitions.length} cut contexts checked · ${shotBoundaries.length} shot cuts · ${pauses.length} low-audio pauses · ${transcript.scene_model || "scene model pending"} · catalogue ${transcript.brand_catalog_version || "legacy"} · ${transcript.scene_prompt_version || ""}${transcript.cache_hit ? " · cached" : ""}`;
    const issues = [transcript.pause_detection_error, transcript.shot_detection_error,
      transcript.transition_probe_error, transcript.scene_analysis_error].filter(Boolean);
    if (issues.length) {
      warning.textContent = issues.join(" ");
      warning.classList.remove("hidden");
    }

    if (pauses.length) {
      pausePanel.classList.remove("hidden");
      for (const pause of pauses.slice(0, 24)) {
        const button = document.createElement("button");
        button.type = "button";
        button.className = "pause-chip";
        button.textContent = `${formatDuration(pause.start)} · ${pause.duration.toFixed(1)}s`;
        button.setAttribute("aria-label", `Play low-audio interval at ${formatDuration(pause.start)}`);
        button.addEventListener("click", () => {
          const video = document.querySelector("#video-preview");
          video.currentTime = pause.start;
          video.play().catch(() => {});
        });
        pauseList.append(button);
      }
      if (pauses.length > 24) {
        const more = document.createElement("span");
        more.className = "pause-more";
        more.textContent = `+${pauses.length - 24} more`;
        pauseList.append(more);
      }
    }

    if (transitions.length) {
      transitionPanel.classList.remove("hidden");
      for (const transition of transitions) {
        const item = document.createElement("li");
        const time = document.createElement("button");
        time.type = "button";
        time.className = "scene-time";
        time.textContent = formatDuration(transition.time);
        time.setAttribute("aria-label", `Play visual transition at ${formatDuration(transition.time)}`);
        time.addEventListener("click", () => {
          const video = document.querySelector("#video-preview");
          video.currentTime = transition.time;
          video.play().catch(() => {});
        });
        const label = document.createElement("span");
        label.className = `transition-label transition-${transition.continuity}`;
        label.textContent = transition.continuity === "same_scene"
          ? "same scene · camera change"
          : transition.continuity === "new_scene"
            ? `new scene · ${transition.kind.replaceAll("_", " ")}`
            : "uncertain transition";
        const confidence = document.createElement("span");
        confidence.className = "transition-confidence";
        confidence.textContent = `${Math.round(transition.confidence * 100)}%`;
        const evidence = document.createElement("p");
        evidence.className = "transition-reason";
        evidence.textContent = transition.evidence;
        item.append(time, label, confidence, evidence);
        transitionList.append(item);
      }
    }

    for (const scene of scenes) {
      const item = document.createElement("li");
      item.className = "scene-card";
      const header = document.createElement("div");
      header.className = "scene-card-header";
      const seek = document.createElement("button");
      seek.type = "button";
      seek.className = "scene-time";
      seek.textContent = `${formatDuration(scene.start)}–${formatDuration(scene.end)}`;
      seek.setAttribute("aria-label", `Play scene from ${formatDuration(scene.start)}`);
      seek.addEventListener("click", () => {
        const video = document.querySelector("#video-preview");
        video.currentTime = scene.start;
        video.play().catch(() => {});
      });
      const confidence = document.createElement("span");
      confidence.className = "scene-confidence";
      const sceneCuts = scene.shot_boundaries || [];
      const feeling = scene.emotional_intensity === undefined ? ""
        : ` · intensity ${Math.round(scene.emotional_intensity * 100)}%${scene.valence === undefined ? "" : ` · ${scene.valence > 0.2 ? "positive" : scene.valence < -0.2 ? "negative" : "neutral"} feeling`}`;
      confidence.textContent = `${Math.round(scene.confidence * 100)}% confidence${sceneCuts.length ? ` · ${sceneCuts.length} cuts` : ""}${feeling}`;
      header.append(seek, confidence);
      const summary = document.createElement("p");
      summary.className = "scene-summary";
      summary.textContent = scene.summary;
      const tags = document.createElement("div");
      tags.className = "scene-tags";
      for (const value of [...(scene.activities || []), ...(scene.tone || [])].slice(0, 8)) {
        const tag = document.createElement("span");
        tag.className = "scene-tag";
        tag.textContent = value.replaceAll("_", " ");
        tags.append(tag);
      }
      for (const value of scene.sensitive_contexts || []) {
        const tag = document.createElement("span");
        tag.className = "scene-tag scene-tag-sensitive";
        tag.textContent = `caution · ${value.replaceAll("_", " ")}`;
        tags.append(tag);
      }
      for (const cut of sceneCuts) {
        const cutButton = document.createElement("button");
        cutButton.type = "button";
        cutButton.className = "scene-tag scene-cut-tag";
        cutButton.textContent = `shot cut · ${formatDuration(cut)}`;
        cutButton.setAttribute("aria-label", `Choose an ad at shot cut ${formatDuration(cut)}`);
        cutButton.addEventListener("click", () => {
          if (breakContext?.job.id === job.id) {
            openCutDialog(cut);
            return;
          }
          const video = document.querySelector("#video-preview");
          video.currentTime = cut;
          video.play().catch(() => {});
        });
        tags.append(cutButton);
      }
      const evidence = document.createElement("p");
      evidence.className = "scene-evidence-copy";
      evidence.textContent = (scene.evidence || []).slice(0, 3).join(" · ");
      item.append(header, summary, tags);
      if (evidence.textContent) item.append(evidence);
      sceneList.append(item);
    }
  }

  const TIER_CLASS = { High: "tier-high", Medium: "tier-medium", Low: "tier-low" };
  const SIGNAL_LABELS = [["visual_window", "Visual"], ["audio", "Audio"], ["pause", "Pause"], ["text", "Dialogue"], ["transition", "Fade/dissolve"]];

  function signalBars(scores = {}) {
    const bars = document.createElement("div");
    bars.className = "signal-bars";
    for (const [key, label] of SIGNAL_LABELS) {
      if (scores[key] === undefined) continue;
      const bar = document.createElement("span");
      bar.className = "signal-bar";
      bar.title = `${label} signal ${Math.round(scores[key] * 100)}%`;
      const name = document.createElement("span");
      name.textContent = label;
      const track = document.createElement("i");
      const fill = document.createElement("b");
      fill.style.width = `${Math.round(scores[key] * 100)}%`;
      track.append(fill);
      bar.append(name, track);
      bars.append(bar);
    }
    return bars;
  }

  function renderPolicyEvidence(job, state, policy, duration, maxCount) {
    const holder = document.querySelector("#policy-evidence");
    holder.replaceChildren();
    if (!state || !policy || !duration) {
      const note = document.createElement("p");
      note.textContent = job?.transcript ? "Finish scene analysis to see this video's break limits."
        : "Choose a video to see its break limits.";
      holder.append(note);
      return;
    }
    const selected = [...state.selected].sort((left, right) => left.time - right.time);
    const plannedSeconds = Number(policy.planned_ad_seconds) || 15;
    let estimated = 0;
    const adSeconds = selected.reduce((sum, item) => {
      const creative = catalogBrands.find((brand) => brand.brand_id === item.brandId)?.creatives
        .find((entry) => entry.id === item.creativeId);
      if (!creative) estimated++;
      return sum + (Number(creative?.duration_sec) || plannedSeconds);
    }, 0);
    const adLoad = adSeconds / (duration + adSeconds) * 100;
    const minGap = Number(policy.min_gap_seconds) || 0;
    const smallestGap = selected.slice(1).reduce((lowest, item, index) =>
      Math.min(lowest, item.time - selected[index].time), Infinity);
    const first = selected[0]?.time ?? Infinity;
    const tail = selected.length ? duration - selected.at(-1).time : Infinity;
    const rows = [
      ["Breaks", `${selected.length} / ${maxCount}`, `At most ${policy.max_breaks_per_hour || 8} per hour; this video's cap is ${maxCount}.`, selected.length > maxCount],
      ["Spacing", selected.length > 1 ? `${formatDuration(smallestGap)} closest` : `${formatDuration(minGap)} minimum`,
        selected.length > 1 ? `${formatDuration(minGap)} required between breaks.` : "Applies when two or more breaks are selected.", smallestGap < minGap],
      ["Programme edges", selected.length ? `${formatDuration(first)} in · ${formatDuration(tail)} left` : "15s in · 10s left",
        "Every break needs at least 15s before it and 10s after it.", first < 15 || tail < 10],
      ["Ad load", `${adLoad.toFixed(1)}% / ${Number(policy.max_ad_load_percent).toFixed(0)}%`,
        `${formatDuration(adSeconds)} ads ÷ (${formatDuration(duration)} programme + ${formatDuration(adSeconds)} ads)${estimated ? ` · ${estimated} duration${estimated === 1 ? "" : "s"} estimated until an ad is chosen` : " · selected ad durations"}.`,
        adLoad > Number(policy.max_ad_load_percent)],
    ];
    for (const [name, value, detail, overLimit] of rows) {
      const row = document.createElement("div");
      row.className = `policy-evidence-row${overLimit ? " policy-evidence-warning" : ""}`;
      const heading = document.createElement("div");
      const label = document.createElement("strong");
      label.textContent = name;
      const amount = document.createElement("span");
      amount.textContent = value;
      const explanation = document.createElement("p");
      explanation.textContent = detail;
      heading.append(label, amount);
      row.append(heading, explanation);
      holder.append(row);
    }
    const note = document.createElement("p");
    note.className = "policy-evidence-note";
    note.textContent = "Finalizing rechecks these limits and the scene's brand safety on the server.";
    holder.append(note);
  }

  function renderBreakDecisions(job) {
    const transcript = job.transcript;
    const panel = document.querySelector("#break-decisions");
    const cutsLayer = document.querySelector("#all-cut-markers");
    const potentialLayer = document.querySelector("#potential-break-markers");
    const selectedLayer = document.querySelector("#break-markers");
    const list = document.querySelector("#break-list");
    const empty = document.querySelector("#break-empty");
    const warning = document.querySelector("#break-warning");
    const countRange = document.querySelector("#break-count-range");
    const countValue = document.querySelector("#break-count-value");
    const status = document.querySelector("#break-selection-status");
    const video = document.querySelector("#video-preview");
    const candidates = transcript?.break_candidates || [];
    const policy = transcript?.break_policy;
    panel.classList.toggle("hidden", !transcript);
    selectedLayer.classList.toggle("hidden", !transcript);
    for (const layer of [cutsLayer, potentialLayer, selectedLayer, list]) layer.replaceChildren();
    empty.classList.add("hidden");
    warning.classList.add("hidden");
    breakContext = null;
    if (!transcript) {
      renderPolicyEvidence(job);
      return;
    }
    const busy = job.status === "queued" || job.status === "processing";
    const unfinished = busy || transcript.break_scoring_status !== "complete" || !policy?.version ||
      !candidates.every((candidate) => "scene_change" in candidate);
    if (unfinished) {
      document.querySelector("#break-funnel").replaceChildren();
      empty.textContent = busy ? "Scene analysis is running. Breaks appear here when it finishes."
        : transcript.scene_analysis_error || transcript.break_scoring_error
          ? `Scene analysis did not finish: ${transcript.scene_analysis_error || transcript.break_scoring_error} Use “Retry scene analysis” above; the transcript is kept.`
          : "This analysis predates scene-change detection. Use “Re-run” on Scene analysis above; the transcript is kept.";
      empty.classList.remove("hidden");
      document.querySelector("#break-meta").textContent = busy ? "Analysing…" : "Scene analysis needed.";
      renderPolicyEvidence(job);
      return;
    }

    const duration = Number(transcript.duration) || 0;
    const maxCount = policy.max_break_count || 0;
    const state = reviewState(job, candidates, maxCount);
    const persist = () => saveReview(job, state);
    if (state.migrated) {
      delete state.migrated;
      persist();
    }
    const rerender = () => renderBreakDecisions(job);
    const applyTarget = async (target) => {
      try {
        await rebalance(job, state, target);
        persist();
      } catch (error) {
        showToast(error.message);
      }
      rerender();
    };
    if (!state.plan) {
      // Load the optimiser's explanations for the current plan without changing it.
      state.plan = { outcomes: [] };
      rebalance(job, state, state.target).then(rerender).catch(() => {});
    }
    breakContext = { job, state, persist, policy, maxCount, duration, candidates, rerender };
    const seek = document.querySelector("#review-seek");
    seek.max = String(duration);
    seek.value = String(video.currentTime || 0);
    document.querySelector("#review-time").value = formatDuration(video.currentTime || 0);
    seek.oninput = () => { video.currentTime = Number(seek.value); };

    const place = (time, inset) => `${Math.max(inset, Math.min(100 - inset, time / duration * 100))}%`;
    const kinds = new Map((transcript.shot_transitions || []).map((item) => [item.time, item.kind]));
    for (const cut of transcript.shot_boundaries || []) {
      const marker = document.createElement("button");
      marker.type = "button";
      marker.className = `cut-marker cut-${kinds.get(cut) || "cut"}`;
      marker.style.left = place(cut, 0.4);
      marker.title = `Shot ${kinds.get(cut) || "cut"} ${formatPrecise(cut)} · choose an ad`;
      marker.setAttribute("aria-label", marker.title);
      marker.onclick = () => openCutDialog(cut);
      cutsLayer.append(marker);
    }

    const outcomes = new Map((state.plan.outcomes || []).map((item) => [item.candidate_id, item]));
    const isSelected = (candidate) => state.selected.some((item) =>
      item.candidateId === candidate.candidate_id || Math.abs(item.time - candidate.time) < 0.5);
    const sceneChanges = candidates.filter((candidate) => candidate.scene_change);
    for (const candidate of sceneChanges) {
      const marker = document.createElement("button");
      marker.type = "button";
      marker.className = `potential-marker ${candidate.potential ? TIER_CLASS[candidate.tier] || "tier-low" : "tier-blocked"}${isSelected(candidate) ? " potential-marker-selected" : ""}`;
      marker.style.left = place(candidate.time, 0.6);
      marker.title = `Scene change ${formatPrecise(candidate.time)} · ${candidate.potential ? `${candidate.tier} ad-friendliness` : "blocked"}`;
      marker.setAttribute("aria-label", marker.title);
      marker.onclick = () => openCutDialog(candidate.time);
      potentialLayer.append(marker);
    }
    for (const item of state.selected) {
      const marker = document.createElement("button");
      marker.type = "button";
      marker.className = `break-marker break-marker-accepted${item.source === "manual" ? " break-marker-manual" : ""}`;
      marker.style.left = place(item.time, 0.8);
      marker.title = `${item.source === "manual" ? "Reviewer" : "AI selected"} ad break at ${formatDuration(item.time)} · edit ad`;
      marker.setAttribute("aria-label", marker.title);
      marker.onclick = () => openCutDialog(item.time);
      selectedLayer.append(marker);
    }

    const manualCount = state.selected.filter((item) => item.source === "manual").length;
    countRange.min = String(manualCount);
    countRange.max = String(maxCount);
    countRange.value = String(state.target);
    countValue.value = String(state.target);
    countRange.oninput = () => { countValue.value = countRange.value; };
    countRange.onchange = () => applyTarget(Number(countRange.value));
    document.querySelector("#break-recommend").onclick = () => applyTarget(policy.auto_break_count);
    document.querySelector("#break-recommend").disabled = state.target === policy.auto_break_count && !manualCount && !state.excludedAI.length;

    const adSeconds = state.selected.length * policy.planned_ad_seconds;
    const load = duration ? adSeconds / (duration + adSeconds) * 100 : 0;
    document.querySelector("#break-funnel").replaceChildren(...[
      [transcript.shot_boundaries?.length || 0, "shot boundaries", "n"],
      [policy.scene_change_count, "scene changes", "m"],
      [state.selected.length, "ad breaks", "k"],
    ].map(([value, label, letter]) => {
      const cell = document.createElement("div");
      const number = document.createElement("strong");
      number.textContent = String(value);
      const caption = document.createElement("span");
      caption.textContent = `${letter} · ${label}`;
      cell.append(number, caption);
      return cell;
    }));
    document.querySelector("#break-meta").textContent = `System recommends ${policy.auto_break_count} of at most ${maxCount} · ` +
      `min gap ${formatDuration(policy.min_gap_seconds)} · planned ad load ${load.toFixed(1)}% of ${policy.max_ad_load_percent}%`;
    renderPolicyEvidence(job, state, policy, duration, maxCount);
    status.textContent = state.plan.message || policy.summary || "";
    if (transcript.break_scoring_error || transcript.text_signal_error) {
      warning.textContent = [transcript.break_scoring_error, transcript.text_signal_error].filter(Boolean).join(" ");
      warning.classList.remove("hidden");
    }
    if (!sceneChanges.length && !state.selected.length) {
      empty.textContent = "No real scene change was found. Review the video with the precise slider and place a break if you find a suitable moment.";
      empty.classList.remove("hidden");
    }
    document.querySelector("#add-break-marker").onclick = () => openCutDialog(Number(video.currentTime));

    const card = (candidate) => {
      const selected = isSelected(candidate);
      const outcome = outcomes.get(candidate.candidate_id);
      const excluded = state.excludedAI.includes(candidate.candidate_id);
      const item = document.createElement("li");
      const tierClass = candidate.potential ? TIER_CLASS[candidate.tier] || "tier-low" : "tier-blocked";
      item.className = `break-card ${selected ? "break-card-accepted" : candidate.potential ? "break-card-potential" : "break-card-rejected"}`;
      const header = document.createElement("div");
      header.className = "break-card-header";
      const time = document.createElement("button");
      time.type = "button";
      time.className = "break-time";
      time.textContent = formatDuration(candidate.time);
      time.onclick = () => { video.currentTime = candidate.time; };
      const tier = document.createElement("span");
      tier.className = `tier-badge ${tierClass}`;
      tier.textContent = candidate.potential ? `${candidate.tier} · ${Math.round(candidate.ad_score * 100)}` : "Blocked";
      const verdict = document.createElement("span");
      verdict.className = `break-verdict ${selected ? "break-verdict-accepted" : candidate.potential ? "break-verdict-potential" : "break-verdict-rejected"}`;
      verdict.textContent = selected ? "SELECTED" : excluded ? "REMOVED" : candidate.potential ? "AVAILABLE" : "WITHHELD";
      header.append(time, tier, verdict);
      const rationale = document.createElement("p");
      rationale.className = "break-rationale";
      rationale.textContent = candidate.rationale || candidate.ai_reason;
      const context = document.createElement("p");
      context.className = "break-signals";
      context.textContent = `${candidate.shot_transition || "cut"} · mood before: ${candidate.preceding_scene_mood || "unclear"}${candidate.preceding_sensitive_contexts?.length ? ` · caution: ${candidate.preceding_sensitive_contexts.join(", ")}` : ""}`;
      const note = document.createElement("p");
      note.className = "break-policy-reason";
      note.textContent = candidate.potential ? (outcome?.note || candidate.selection_note || "") : (candidate.reasons || []).map((reason) => reason.message).join(" ");
      const brandFit = document.createElement("p");
      brandFit.className = "break-brand-fit";
      const bestBrand = candidate.brand_recommendations?.[0];
      brandFit.textContent = bestBrand ? `Best brand fit: ${bestBrand.display_name} · ${Math.round(bestBrand.fit_score * 100)}% · ${bestBrand.reason}` : "No safe brand fit identified for the preceding scene.";
      const actions = document.createElement("div");
      actions.className = "break-card-actions";
      const choose = document.createElement("button");
      choose.type = "button";
      choose.className = "remove-break-marker";
      choose.textContent = selected ? "Edit ad…" : "Inspect & choose ad…";
      choose.onclick = () => openCutDialog(candidate.time);
      actions.append(choose);
      if (candidate.potential) {
        const toggle = document.createElement("button");
        toggle.type = "button";
        toggle.className = "remove-break-marker";
        toggle.textContent = selected ? "Remove" : excluded ? "Allow again" : "Force include";
        toggle.onclick = async () => {
          if (selected) {
            state.excludedAI = [...new Set([...state.excludedAI, candidate.candidate_id])];
            state.selected = state.selected.filter((entry) => entry.candidateId !== candidate.candidate_id);
            await applyTarget(Math.max(manualCount, state.target - 1));
          } else if (excluded) {
            state.excludedAI = state.excludedAI.filter((id) => id !== candidate.candidate_id);
            await applyTarget(state.target);
          } else {
            const issue = placementLimitMessage({ selected: state.selected.filter((entry) => entry.source === "manual") },
              candidate.time, duration, policy, maxCount);
            if (issue) { showToast(issue); return; }
            state.selected.push({ time: candidate.time, source: "manual", candidateId: candidate.candidate_id });
            await applyTarget(Math.min(maxCount, state.target + 1));
          }
        };
        actions.append(toggle);
      }
      const beat = peakBefore(transcript.pacing, candidate.time);
      const pacingNote = document.createElement("p");
      pacingNote.className = "break-pacing";
      if (beat) {
        pacingNote.textContent = `${beat.kind === "cliffhanger" ? "Cliffhanger break" : "After a dramatic beat"}: follows the ${formatDuration(beat.time)} peak` +
          `${beat.label ? ` (${beat.label})` : ""}, ${Math.round(beat.value * 100)}% tension.`;
      }
      item.append(header, rationale, context, signalBars(candidate.signal_scores), ...(beat ? [pacingNote] : []), note, brandFit, actions);
      return item;
    };
    sceneChanges.forEach((candidate) => list.append(card(candidate)));
    for (const item of state.selected.filter((entry) => !candidates.some((candidate) =>
      candidate.candidate_id === entry.candidateId || Math.abs(candidate.time - entry.time) < 0.5))) {
      const manual = document.createElement("li");
      manual.className = "break-card break-card-accepted";
      const header = document.createElement("div");
      header.className = "break-card-header";
      const time = document.createElement("button");
      time.type = "button";
      time.className = "break-time";
      time.textContent = formatDuration(item.time);
      time.onclick = () => { video.currentTime = item.time; };
      const verdict = document.createElement("span");
      verdict.className = "break-verdict break-verdict-accepted";
      verdict.textContent = "REVIEWER BREAK";
      header.append(time, verdict);
      const noteText = document.createElement("p");
      noteText.className = "break-policy-reason";
      noteText.textContent = "Placed outside the detected scene changes. Check the scene mood, dialogue, and sensitive context before finalizing.";
      const edit = document.createElement("button");
      edit.type = "button";
      edit.className = "remove-break-marker";
      edit.textContent = "Edit or remove…";
      edit.onclick = () => openCutDialog(item.time);
      manual.append(header, noteText, edit);
      list.append(manual);
    }
    const sameScene = candidates.filter((candidate) => !candidate.scene_change);
    if (sameScene.length) {
      const holder = document.createElement("li");
      holder.className = "break-card same-scene-group";
      const details = document.createElement("details");
      const summary = document.createElement("summary");
      summary.textContent = `${sameScene.length} likely boundar${sameScene.length === 1 ? "y was" : "ies were"} judged camera changes inside a scene`;
      const rows = document.createElement("ul");
      for (const candidate of sameScene) {
        const row = document.createElement("li");
        const time = document.createElement("button");
        time.type = "button";
        time.className = "break-time";
        time.textContent = formatDuration(candidate.time);
        time.onclick = () => { video.currentTime = candidate.time; };
        const text = document.createElement("span");
        text.textContent = candidate.ai_reason || candidate.rationale;
        row.append(time, text);
        rows.append(row);
      }
      details.append(summary, rows);
      holder.append(details);
      list.append(holder);
    }
    renderPlaybackPlanner(job, state, persist);
  }

  async function watchJob(jobId) {
    if (pollingJobs.has(jobId)) return;
    pollingJobs.add(jobId);
    const started = Date.now();
    try {
      while (Date.now() - started < 10 * 60 * 1000) {
        await new Promise((resolve) => window.setTimeout(resolve, 1800));
        const response = await fetch(`/api/jobs/${encodeURIComponent(jobId)}`);
        if (!response.ok) continue;
        const job = await response.json();
        showJob(job, { poll: false, notify: job.status === "completed" || job.status === "failed" });
        if (job.status !== "queued" && job.status !== "processing") break;
      }
    } catch {
      showToast("Could not refresh analysis status. The job may still be running.");
    } finally {
      pollingJobs.delete(jobId);
    }
  }

  const libraryStatus = (job) => job.status === "processing" || job.status === "queued" ? "Analysing…"
    : job.transcript?.break_scoring_status === "complete" ? "Analysis ready"
      : job.transcript ? "Scene analysis needs retry" : job.status === "failed" ? "Needs retry" : "Intake ready";

  function addToLibrary(job) {
    const list = document.querySelector("#library-list");
    const existing = list.querySelector(`[data-job-id="${CSS.escape(job.id)}"]`);
    if (existing) {
      existing.querySelector(".library-job-copy span").textContent = libraryStatus(job);
      return;
    }
    list.querySelector(".library-empty")?.remove();
    const item = document.createElement("div");
    item.className = "library-item";
    item.dataset.jobId = job.id;
    const button = document.createElement("button");
    button.className = "library-job";
    button.type = "button";
    button.setAttribute("aria-label", `Open ${job.fileName}`);
    const thumb = document.createElement("span");
    thumb.className = "library-thumb";
    thumb.textContent = "▶";
    const copy = document.createElement("span");
    copy.className = "library-job-copy";
    const name = document.createElement("strong");
    name.textContent = job.fileName;
    const status = document.createElement("span");
    status.textContent = libraryStatus(job);
    copy.append(name, status);
    button.append(thumb, copy);
    button.addEventListener("click", async () => {
      try {
        const response = await fetch(`/api/jobs/${encodeURIComponent(job.id)}`);
        if (response.ok) showJob(await response.json());
      } catch {
        showJob(job);
      }
    });
    const remove = document.createElement("button");
    remove.type = "button";
    remove.className = "library-delete";
    remove.textContent = "×";
    remove.title = `Delete ${job.fileName}`;
    remove.setAttribute("aria-label", remove.title);
    remove.addEventListener("click", () => deleteVideo(job.id, job.fileName));
    item.append(button, remove);
    list.prepend(item);
  }

  function resetWorkspace() {
    activeJob = null;
    playbackPlan = null;
    breakContext = null;
    renderPolicyEvidence(null);
    const video = document.querySelector("#video-preview");
    video.removeAttribute("src");
    video.load();
    assetPanel.classList.add("hidden");
    dropzone.classList.remove("hidden");
    for (const selector of ["#transcript-panel", "#scene-evidence", "#break-decisions", "#phase-status"]) {
      document.querySelector(selector).classList.add("hidden");
    }
    for (const selector of ["#all-cut-markers", "#potential-break-markers", "#break-markers"]) {
      document.querySelector(selector).replaceChildren();
    }
    Object.assign(timeline, { jobId: null, zoom: 1, duration: 0 });
    timelineTrack.style.width = "100%";
    for (const selector of ["#timeline-scenes", "#timeline-ruler", "#timeline-emotion"]) document.querySelector(selector).replaceChildren();
    document.querySelector("#pacing-summary").classList.add("hidden");
    updateZoomControls();
    document.querySelector("#timeline-empty-title").textContent = "The story comes first.";
    document.querySelector("#timeline-empty-copy").textContent = "Upload a video to get its first look.";
    input.value = "";
  }

  async function deleteVideo(id, fileName) {
    if (!window.confirm(`Delete “${fileName}” with its transcript, analysis, break review and manifests? This cannot be undone.`)) return;
    try {
      const response = await fetch(`/api/jobs/${encodeURIComponent(id)}`, { method: "DELETE" });
      if (!response.ok) throw new Error((await response.json()).error || "The video could not be deleted.");
    } catch (error) {
      showToast(error.message || "The video could not be deleted.");
      return;
    }
    reviewStates.delete(id);
    const list = document.querySelector("#library-list");
    list.querySelector(`[data-job-id="${CSS.escape(id)}"]`)?.remove();
    if (!list.children.length) {
      list.innerHTML = '<div class="library-empty"><span class="folder-icon">▱</span><span>Your stories will<br />show up here</span></div>';
    }
    if (activeJob?.id === id) resetWorkspace();
    showToast(`${fileName} was deleted.`);
  }

  // Phase status: what has run for this video, what failed, and where a retry would start.
  function renderPhaseStatus(job) {
    const holder = document.querySelector("#phase-status");
    holder.replaceChildren();
    holder.classList.remove("hidden");
    const busy = job.status === "queued" || job.status === "processing";
    const transcript = job.transcript;
    const analysed = transcript?.scene_analysis_status === "complete" && transcript?.break_scoring_status === "complete";
    const inTranscription = busy && /transcri/.test(job.stage || "") && !transcript;
    const phases = [
      { name: "Upload", state: "done", detail: `${formatDuration(job.media.durationSeconds)} · ${job.media.width}×${job.media.height}` },
      {
        name: "Transcription", phase: "transcription",
        state: transcript ? "done" : inTranscription || busy ? "running" : job.status === "failed" ? "failed" : "waiting",
        detail: transcript ? `${transcript.segments.length} segments · ${transcript.model}` : job.status === "failed" ? job.message : busy ? job.message : "Not started",
      },
      {
        name: "Scene analysis", phase: "scene_analysis",
        state: analysed ? "done" : busy && !inTranscription ? "running" : transcript ? "failed" : "waiting",
        detail: analysed ? `${transcript.shot_boundaries?.length || 0} shots · ${transcript.break_policy?.scene_change_count ?? 0} scene changes`
          : busy && !inTranscription ? job.message
            : transcript ? (transcript.scene_analysis_error || transcript.break_scoring_error || "Not run with the current pipeline.") : "Waits for the transcript",
      },
    ];
    for (const phase of phases) {
      const row = document.createElement("div");
      row.className = `phase-row phase-${phase.state}`;
      const icon = document.createElement("span");
      icon.className = "phase-icon";
      icon.textContent = { done: "✓", running: "…", failed: "!", waiting: "·" }[phase.state];
      const copy = document.createElement("div");
      copy.className = "phase-copy";
      const name = document.createElement("strong");
      name.textContent = phase.name;
      const detail = document.createElement("span");
      detail.textContent = phase.detail;
      copy.append(name, detail);
      row.append(icon, copy);
      const canRun = phase.phase && !busy && (phase.phase === "transcription" || transcript);
      if (canRun && (phase.state === "failed" || phase.state === "done")) {
        const button = document.createElement("button");
        button.type = "button";
        button.className = phase.state === "failed" ? "primary-button" : "outline-button";
        button.textContent = phase.state === "failed" ? `Retry ${phase.name.toLowerCase()}`
          : phase.phase === "transcription" ? "Re-transcribe" : "Re-run";
        button.title = phase.phase === "transcription" ? "Transcribe again, then redo scene analysis."
          : "Keep the transcript and redo shots, signals, scene judgements, and break scoring.";
        button.onclick = () => retryPhase(job, phase.phase, phase.state === "done");
        row.append(button);
      }
      holder.append(row);
    }
  }

  async function retryPhase(job, phase, replacing) {
    if (replacing && !window.confirm(phase === "transcription"
      ? "Transcribe this video again? Scene analysis and the break plan will be recomputed."
      : "Re-run scene analysis with the saved transcript? The break plan will be recomputed; finalized manifests stay available.")) return;
    try {
      const response = await fetch(`/api/jobs/${encodeURIComponent(job.id)}/retry`, {
        method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ from: phase }),
      });
      const updated = await response.json();
      if (!response.ok) throw new Error(updated.error || "The retry could not be started.");
      reviewStates.delete(job.id);
      showJob(updated);
      watchJob(updated.id);
    } catch (error) {
      showToast(error.message || "The retry could not be started.");
    }
  }

  function uploadFile(file) {
    if (!file) return;
    if (!file.name.toLowerCase().endsWith(".mp4")) {
      showError("Choose an MP4 file to continue.");
      return;
    }
    if (file.size > 500 * 1024 * 1024) {
      showError("This file is larger than the 500 MB upload limit.");
      return;
    }

    setBusy(file);
    const body = new FormData();
    body.append("video", file);
    const request = new XMLHttpRequest();
    activeRequest = request;
    request.open("POST", "/api/jobs");
    request.responseType = "json";
    request.upload.addEventListener("progress", (event) => {
      if (!event.lengthComputable) return;
      const fraction = event.loaded / event.total;
      setProgress(fraction * 88, fraction < 1 ? "Uploading and checking your video…" : "Upload complete. Reading media details…");
    });
    request.addEventListener("load", () => {
      activeRequest = null;
      const payload = request.response;
      if (request.status >= 200 && request.status < 300 && payload?.id) {
        setProgress(100, "Media intake complete.");
        showJob({ ...payload, fileSize: file.size });
        return;
      }
      showError(payload?.error || "The upload could not be processed. Please try another MP4.");
    });
    request.addEventListener("error", () => {
      activeRequest = null;
      showError("Could not reach the analysis service. Check your connection and try again.");
    });
    request.addEventListener("abort", () => {
      activeRequest = null;
      progressPanel.classList.add("hidden");
      dropzone.classList.remove("hidden");
      showToast("Upload cancelled.");
    });
    request.send(body);
  }

  document.querySelector("#browse-button").addEventListener("click", (event) => {
    event.stopPropagation();
    input.click();
  });
  dropzone.addEventListener("click", (event) => {
    if (event.target.closest("button")) return;
    input.click();
  });
  dropzone.addEventListener("keydown", (event) => {
    if (event.key === "Enter" || event.key === " ") {
      event.preventDefault();
      input.click();
    }
  });
  input.addEventListener("change", () => uploadFile(input.files?.[0]));
  document.querySelector("#cancel-upload").addEventListener("click", () => activeRequest?.abort());
  document.querySelector("#delete-video").addEventListener("click", () => {
    if (activeJob) deleteVideo(activeJob.id, activeJob.fileName);
  });
  document.querySelector("#new-upload").addEventListener("click", () => {
    document.querySelector("#video-preview").removeAttribute("src");
    document.querySelector("#video-preview").load();
    assetPanel.classList.add("hidden");
    dropzone.classList.remove("hidden");
    input.value = "";
    dropzone.focus();
  });

  for (const eventName of ["dragenter", "dragover"]) {
    dropzone.addEventListener(eventName, (event) => {
      event.preventDefault();
      dropzone.classList.add("dragging");
    });
  }
  for (const eventName of ["dragleave", "drop"]) {
    dropzone.addEventListener(eventName, (event) => {
      event.preventDefault();
      dropzone.classList.remove("dragging");
    });
  }
  dropzone.addEventListener("drop", (event) => uploadFile(event.dataTransfer?.files?.[0]));

  const previewVideo = document.querySelector("#video-preview");
  const reviewSeek = document.querySelector("#review-seek");
  const reviewTime = document.querySelector("#review-time");
  previewVideo.addEventListener("timeupdate", () => {
    reviewSeek.value = String(previewVideo.currentTime || 0);
    reviewTime.value = formatDuration(previewVideo.currentTime || 0);
  });
  previewVideo.addEventListener("timeupdate", () => updatePlayhead(false));
  previewVideo.addEventListener("seeked", () => updatePlayhead(true));
  previewVideo.addEventListener("play", () => {
    if (timeline.following) return;
    timeline.following = true;
    window.requestAnimationFrame(followPlayback);
  });
  document.querySelector("#zoom-in").addEventListener("click", () => setZoom(timeline.zoom * 2));
  document.querySelector("#zoom-out").addEventListener("click", () => setZoom(timeline.zoom / 2));
  document.querySelector("#zoom-fit").addEventListener("click", () => setZoom(1));
  timelineViewport.addEventListener("scroll", () => {
    // Scrolls caused by a layout change arrive before the resize callback; only user or zoom scrolls count.
    if (timelineTrack.clientWidth === timeline.trackWidth) {
      timeline.startFraction = timelineViewport.scrollLeft / (timeline.trackWidth || 1);
    }
    updateZoomControls();
  }, { passive: true });
  timelineViewport.addEventListener("wheel", (event) => {
    // Ctrl/⌘ + scroll and trackpad pinch zoom around the playhead; plain scrolling pans.
    if (!event.ctrlKey && !event.metaKey) return;
    event.preventDefault();
    setZoom(timeline.zoom * Math.exp(-event.deltaY * 0.0025));
  }, { passive: false });
  timelineViewport.addEventListener("keydown", (event) => {
    if (event.target !== timelineViewport) return;
    const actions = { "+": 2, "=": 2, "-": 0.5, "_": 0.5 };
    if (event.key in actions) setZoom(timeline.zoom * actions[event.key]);
    else if (event.key === "0") setZoom(1);
    else return;
    event.preventDefault();
  });
  timelineTrack.addEventListener("click", (event) => {
    // Clicking the timeline (not a marker) moves the playhead there.
    if (event.target.closest("button") || !timeline.duration) return;
    const box = timelineTrack.getBoundingClientRect();
    previewVideo.currentTime = Math.min(timeline.duration, Math.max(0, (event.clientX - box.left) / box.width * timeline.duration));
    updatePlayhead(false);
  });
  const emotionTooltip = document.querySelector("#emotion-tooltip");
  timelineTrack.addEventListener("mousemove", (event) => {
    const pacing = activeJob?.transcript?.pacing;
    const lane = document.querySelector("#timeline-emotion").getBoundingClientRect();
    if (!pacing?.tension?.length || !timeline.duration || event.clientY < lane.top || event.clientY > lane.bottom) {
      emotionTooltip.classList.add("hidden");
      return;
    }
    const box = timelineTrack.getBoundingClientRect();
    const time = (event.clientX - box.left) / box.width * timeline.duration;
    const value = pacingAt(pacing, time);
    const percent = (number) => number === null || number === undefined ? "—" : `${Math.round(number * 100)}%`;
    emotionTooltip.textContent = `${formatDuration(time)} · tension ${percent(value.tension)} — scene ${percent(value.scene)}, audio ${percent(value.audio)}, editing ${percent(value.cuts)}`;
    const viewport = timelineViewport.getBoundingClientRect();
    emotionTooltip.style.left = `${Math.min(viewport.width - 240, Math.max(4, event.clientX - viewport.left + 10))}px`;
    emotionTooltip.classList.remove("hidden");
  });
  timelineTrack.addEventListener("mouseleave", () => emotionTooltip.classList.add("hidden"));
  // The track width is a percentage, so after a resize restore the same slice of the programme.
  new ResizeObserver(() => {
    const start = timeline.startFraction;
    timeline.trackWidth = timelineTrack.clientWidth;
    timelineViewport.scrollLeft = start * timeline.trackWidth;
    timeline.startFraction = start;
    updateZoomControls();
  }).observe(timelineViewport);
  previewVideo.addEventListener("loadedmetadata", () => {
    reviewSeek.max = String(previewVideo.duration || 0);
  });
  document.querySelector("#cut-dialog-close").addEventListener("click", () => cutDialog.close());
  cutDialog.addEventListener("click", (event) => { if (event.target === cutDialog) cutDialog.close(); });

  const adForm = document.querySelector("#ad-upload-form");
  const adStatus = document.querySelector("#ad-upload-status");
  adForm.elements.brand_name.addEventListener("input", () => {
    const value = adForm.elements.brand_name.value.trim().toLowerCase();
    const brand = catalogBrands.find((entry) => entry.source === "custom" && entry.display_name.toLowerCase() === value);
    if (!brand) return;
    adForm.elements.category.value = brand.category === "uncategorised" ? "" : brand.category;
    adForm.elements.target_contexts.value = (brand.target_contexts || []).join(", ");
    adForm.elements.negative_contexts.value = (brand.negative_contexts || []).join(", ");
    adForm.elements.click_through_url.value = brand.click_through_url || "";
    adForm.elements.cta_label.value = brand.cta_label || "";
    adStatus.textContent = `Adding another creative to ${brand.display_name}. Its contexts will be updated to what you submit.`;
  });
  adForm.addEventListener("submit", (event) => {
    event.preventDefault();
    const submit = document.querySelector("#ad-upload-submit");
    const file = adForm.elements.video.files?.[0];
    const name = adForm.elements.brand_name.value.trim();
    let problem = "";
    if (name.length < 2) problem = "Enter a brand name.";
    else if (!adForm.elements.target_contexts.value.trim()) problem = "Add at least one target context.";
    else if (!file) problem = "Choose the ad video.";
    else if (!file.name.toLowerCase().endsWith(".mp4")) problem = "The ad must be an MP4 file.";
    else if (file.size > 200 * 1024 * 1024) problem = "The ad must be smaller than 200 MB.";
    else if (adForm.elements.click_through_url.value.trim() && !/^https?:\/\/\S+$/i.test(adForm.elements.click_through_url.value.trim())) {
      problem = "The website link must start with http:// or https://.";
    }
    if (problem) {
      adStatus.textContent = problem;
      return;
    }
    const request = new XMLHttpRequest();
    request.open("POST", "/api/ads");
    request.responseType = "json";
    submit.disabled = true;
    adStatus.textContent = "Uploading…";
    request.upload.addEventListener("progress", (progress) => {
      if (progress.lengthComputable) adStatus.textContent = progress.loaded < progress.total
        ? `Uploading… ${Math.round(progress.loaded / progress.total * 100)}%` : "Checking the video…";
    });
    request.addEventListener("load", async () => {
      submit.disabled = false;
      const payload = request.response;
      if (request.status < 200 || request.status >= 300) {
        adStatus.textContent = payload?.error || "The ad could not be uploaded.";
        return;
      }
      adForm.reset();
      const creative = payload.brand.creatives.find((entry) => entry.id === payload.creative_id);
      adStatus.textContent = `${payload.brand.display_name} · ${creative?.duration_sec ?? "?"}s ad saved to the library.`;
      showToast("Ad saved. It is now suggested wherever its contexts fit.");
      try { await loadCatalog(); } catch (error) { adStatus.textContent = error.message; }
    });
    request.addEventListener("error", () => {
      submit.disabled = false;
      adStatus.textContent = "Could not reach the service. Check your connection and try again.";
    });
    request.send(new FormData(adForm));
  });

  loadCatalog().catch(() => {
    document.querySelector("#ad-library-count").textContent = "Unavailable";
    document.querySelector("#playback-status").textContent = "The ad library could not be loaded; VMAP creation is unavailable.";
  });

  fetch("/api/jobs").then((response) => response.json()).then(({ jobs = [] }) => {
    jobs.sort((a, b) => new Date(b.createdAt) - new Date(a.createdAt)).forEach((job) => {
      addToLibrary(job);
      if (job.status === "queued" || job.status === "processing") watchJob(job.id);
    });
    const requestedJob = new URLSearchParams(window.location.search).get("job");
    const selected = jobs.find((job) => job.id === requestedJob);
    if (selected) showJob(selected);
  }).catch(() => {});
})();
