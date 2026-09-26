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
  let activePlaybackBreak = null;
  let playbackAdStarted = false;
  let adFailureHandled = false;
  const playedBreaks = new Set();
  const pollingJobs = new Set();

  const formatDuration = (seconds) => {
    const total = Math.max(0, Math.floor(seconds));
    const hours = Math.floor(total / 3600);
    const minutes = Math.floor((total % 3600) / 60);
    const remainder = total % 60;
    return hours ? `${hours}:${String(minutes).padStart(2, "0")}:${String(remainder).padStart(2, "0")}` : `${minutes}:${String(remainder).padStart(2, "0")}`;
  };

  function loadBreakSelection(jobId, candidates, maxCount, duration, policy) {
    const key = `scenesense-break-selection:${jobId}`;
    let state;
    try { state = JSON.parse(localStorage.getItem(key) || "null"); } catch { state = null; }
    if (!state || !Array.isArray(state.selected)) {
      state = {
        target: Math.min(maxCount, candidates.filter((candidate) => candidate.decision === "accepted").length),
        selected: candidates.filter((candidate) => candidate.decision === "accepted")
          .map((candidate) => ({ time: candidate.time, source: "ai", candidateId: candidate.candidate_id })),
        excludedAI: [],
      };
    }
    if (!Array.isArray(state.excludedAI)) state.excludedAI = [];
    const cleaned = [];
    for (const item of [...state.selected].sort((left, right) => left.time - right.time)) {
      if (!Number.isFinite(item.time) || item.time < 15 || item.time > duration - 10 ||
          (item.source !== "manual" && !candidates.some((candidate) => candidate.candidate_id === item.candidateId &&
            (candidate.potential || candidate.decision === "accepted")))) continue;
      if (cleaned.length >= maxCount || cleaned.some((prior) => Math.abs(prior.time - item.time) < policy.min_gap_seconds)) continue;
      cleaned.push({ ...item, source: item.source === "manual" ? "manual" : "ai" });
    }
    state.selected = cleaned;
    state.target = Math.min(maxCount, Math.max(state.selected.length, Number(state.target) || 0));
    return { key, state };
  }

  function saveBreakSelection(key, state) {
    try { localStorage.setItem(key, JSON.stringify(state)); } catch { /* This session remains editable without storage. */ }
  }

  function selectBestAIBreaks(state, candidates, target, minimumGap) {
    const selected = state.selected.filter((item) => item.source === "manual");
    const eligible = candidates.filter((candidate) => (candidate.potential ||
      (candidate.potential === undefined && candidate.decision === "accepted")) &&
      !state.excludedAI.includes(candidate.candidate_id));
    eligible.sort((left, right) => (right.naturalness - right.disruption_risk) -
      (left.naturalness - left.disruption_risk) || right.confidence - left.confidence || left.time - right.time);
    for (const candidate of eligible) {
      if (selected.length >= target) break;
      if (selected.some((item) => Math.abs(item.time - candidate.time) < minimumGap)) continue;
      selected.push({ time: candidate.time, source: "ai", candidateId: candidate.candidate_id });
    }
    state.selected = selected.sort((left, right) => left.time - right.time);
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
    if (playbackPlan?.breaks?.length) {
      document.querySelector("#vmap-download").href = playbackPlan.vmap_url;
      document.querySelector("#debug-download").href = playbackPlan.debug_url;
      document.querySelector("#playback-downloads").classList.remove("hidden");
    } else {
      document.querySelector("#playback-downloads").classList.add("hidden");
    }
    if (!catalogBrands.length) {
      status.textContent = "Loading the synthetic brand catalogue…";
      return;
    }
    if (!state.selected.length) {
      status.textContent = "Add or select a break marker first. Local placeholder MP4s show a sample filename, time, and mood; the overlay shows this marker's actual scene context.";
      return;
    }
    let allRowsSafe = true;
    for (const [index, item] of state.selected.entries()) {
      const row = document.createElement("div");
      row.className = "playback-break-row";
      const label = document.createElement("strong");
      label.className = "playback-break-label";
      label.textContent = `${formatDuration(item.time)} · ${item.source === "manual" ? "manual" : "AI"}`;
      const brandLabel = document.createElement("label");
      brandLabel.textContent = "Brand";
      const brandSelect = document.createElement("select");
      brandSelect.setAttribute("aria-label", `Brand for break at ${formatDuration(item.time)}`);
      const matches = precedingSceneBrandMatches(job.transcript, item.time);
      const matchByID = new Map(matches.map((match) => [match.brand_id, match]));
      const safeBrands = catalogBrands.filter((brand) => matchByID.has(brand.brand_id) && !matchByID.get(brand.brand_id).blocked);
      const candidate = (job.transcript.break_candidates || []).find((entry) => Math.abs(entry.time - item.time) < 0.5);
      const aiBrand = candidate?.brand_recommendations?.find((match) => !match.blocked)?.brand_id;
      if (!item.brandId) item.brandId = safeBrands.some((brand) => brand.brand_id === aiBrand)
        ? aiBrand : safeBrands[0]?.brand_id || "";
      const placeholder = document.createElement("option");
      placeholder.value = "";
      placeholder.textContent = "Choose a safe brand";
      brandSelect.append(placeholder);
      for (const brand of catalogBrands) {
        const option = document.createElement("option");
        option.value = brand.brand_id;
        const fit = matchByID.get(brand.brand_id);
        option.disabled = !fit || fit.blocked;
        option.textContent = `${brand.display_name}${fit?.recommended ? ` · AI fit ${Math.round(fit.fit_score * 100)}%` : fit?.blocked ? " · blocked by scene" : fit ? " · reviewer choice" : " · no scene evidence"}`;
        brandSelect.append(option);
      }
      brandSelect.value = item.brandId;
      if (!item.brandId || !safeBrands.some((brand) => brand.brand_id === item.brandId)) allRowsSafe = false;
      brandSelect.onchange = () => {
        item.brandId = brandSelect.value;
        const brand = catalogBrands.find((entry) => entry.brand_id === item.brandId);
        item.creativeId = brand?.creatives.find((creative) => creative.duration_sec === 15)?.id || brand?.creatives[0]?.id || "";
        persist();
        renderPlaybackPlanner(job, state, persist);
      };
      brandLabel.append(brandSelect);
      const creativeLabel = document.createElement("label");
      creativeLabel.textContent = "Catalogue creative · demo slate";
      const creativeSelect = document.createElement("select");
      creativeSelect.setAttribute("aria-label", `Creative for break at ${formatDuration(item.time)}`);
      const brand = catalogBrands.find((entry) => entry.brand_id === item.brandId);
      for (const creative of brand?.creatives || []) {
        const option = document.createElement("option");
        option.value = creative.id;
        option.textContent = `${creative.url.split("/").at(-1)} · ${creative.duration_sec}s · ${creative.language}`;
        creativeSelect.append(option);
      }
      if (!item.creativeId || !(brand?.creatives || []).some((creative) => creative.id === item.creativeId)) {
        item.creativeId = brand?.creatives.find((creative) => creative.duration_sec === 15)?.id || brand?.creatives[0]?.id || "";
      }
      creativeSelect.value = item.creativeId;
      creativeSelect.disabled = !brand;
      creativeSelect.onchange = () => {
        item.creativeId = creativeSelect.value;
        persist();
        document.querySelector("#playback-status").textContent = "Creative changed. Build the VMAP again to update the playback plan.";
        document.querySelector("#play-programme").disabled = true;
      };
      creativeLabel.append(creativeSelect);
      row.append(label, brandLabel, creativeLabel);
      rows.append(row);
    }
    build.disabled = !allRowsSafe;
    if (!allRowsSafe) {
      status.textContent = "Choose a brand with clear, non-blocked scene evidence for every marker. If all are blocked or uncertain, playback stays fail-closed.";
    } else if (playbackPlan?.breaks?.length === state.selected.length) {
      status.textContent = "VMAP ready. Play to pause at each marker, show its local placeholder creative, then resume the programme.";
    } else {
      status.textContent = "Brand fit and creative duration are checked again by the server when you build the VMAP.";
    }
    build.onclick = async () => {
      build.disabled = true;
      status.textContent = "Validating creative safety, break spacing, and actual ad-load…";
      try {
        const response = await fetch(`/api/jobs/${encodeURIComponent(job.id)}/playback-plan`, {
          method: "POST", headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ breaks: state.selected.map((selection) => ({
            time: selection.time, source: selection.source, brand_id: selection.brandId, creative_id: selection.creativeId,
          })) }),
        });
        const payload = await response.json();
        if (!response.ok) throw new Error(payload.error || "The playback plan could not be validated.");
        playbackPlan = payload.plan;
        activeJob.playback_plan = playbackPlan;
        playedBreaks.clear();
        document.querySelector("#vmap-download").href = playbackPlan.vmap_url;
        document.querySelector("#debug-download").href = playbackPlan.debug_url;
        document.querySelector("#playback-downloads").classList.remove("hidden");
        document.querySelector("#play-programme").disabled = playbackPlan.breaks.length === 0;
        renderPlaybackEventHistory([]);
        status.textContent = `${payload.break_count} VMAP break${payload.break_count === 1 ? "" : "s"} validated. The XML uses catalogue creative IDs and locally generated placeholder media.`;
      } catch (error) {
        showToast(error.message || "Could not create the playback plan.");
        status.textContent = error.message || "Could not create the playback plan.";
        build.disabled = false;
      }
    };
    play.onclick = () => startProgrammeWithAds(job);
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

  function startProgrammeWithAds(job) {
    if (!playbackPlan?.breaks?.length || !job?.id) return;
    const video = document.querySelector("#video-preview");
    playedBreaks.clear();
    activePlaybackBreak = null;
    playbackAdStarted = false;
    renderPlaybackEventHistory([]);
    video.currentTime = 0;
    video.play().catch(() => showToast("Press play on the programme player to start the demo."));
  }

  async function playScheduledBreak(item) {
    if (activePlaybackBreak || playedBreaks.has(item.break_id)) return;
    const video = document.querySelector("#video-preview");
    const ad = document.querySelector("#ad-preview");
    activePlaybackBreak = item;
    playbackAdStarted = false;
    adFailureHandled = false;
    playedBreaks.add(item.break_id);
    video.pause();
    video.currentTime = item.time;
    document.querySelector("#ad-preview-title").textContent = item.brand_name;
    document.querySelector("#ad-preview-file").textContent = `Creative file: ${item.source_filename}`;
    document.querySelector("#ad-preview-time").textContent = `Insertion point: ${formatDuration(item.time)} · ${item.duration_sec}s`;
    document.querySelector("#ad-preview-context").textContent = `Annotated preceding-scene mood: ${item.preceding_scene_mood || "unclear"} · ${item.preceding_scene_context || "No scene summary"}`;
    document.querySelector("#ad-countdown").textContent = `${item.duration_sec}s`;
    document.querySelector("#ad-preview-overlay").classList.remove("hidden");
    await recordPlaybackEvent("break_start", item);
    ad.src = item.creative_url;
    ad.load();
    ad.play().catch(async (error) => {
      if (!activePlaybackBreak || adFailureHandled) return;
      adFailureHandled = true;
      await recordPlaybackEvent("error", item, "synthetic creative failed to start");
      await recordPlaybackEvent("skip", item, error?.message || "ad playback failed; fail open to programme");
      resumeProgramme(item);
    });
  }

  function resumeProgramme(item) {
    const video = document.querySelector("#video-preview");
    const ad = document.querySelector("#ad-preview");
    document.querySelector("#ad-preview-overlay").classList.add("hidden");
    ad.pause();
    activePlaybackBreak = null;
    playbackAdStarted = false;
    video.currentTime = item.time;
    video.play().then(() => recordPlaybackEvent("resume", item)).catch(() => {
      recordPlaybackEvent("error", item, "programme could not resume automatically");
      showToast("Ad finished. Press play to continue the programme.");
    });
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
    if (activeJob?.id !== job.id) {
      playbackPlan = null;
      activePlaybackBreak = null;
      playedBreaks.clear();
    }
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
    document.querySelector(".wave-end").textContent = formatDuration(job.media.durationSeconds);
    renderTranscript(job);
    renderSceneEvidence(job);
    renderBreakDecisions(job);
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
    const retry = document.querySelector("#retry-transcription");
    panel.classList.remove("hidden");
    retry.classList.toggle("hidden", job.status === "queued" || job.status === "processing");
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
      confidence.textContent = `${Math.round(scene.confidence * 100)}% confidence${sceneCuts.length ? ` · ${sceneCuts.length} cuts` : ""}`;
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
        cutButton.setAttribute("aria-label", `Play shot cut at ${formatDuration(cut)}`);
        cutButton.addEventListener("click", () => {
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
    cutsLayer.replaceChildren();
    potentialLayer.replaceChildren();
    selectedLayer.replaceChildren();
    list.replaceChildren();
    empty.classList.add("hidden");
    warning.classList.add("hidden");
    if (!transcript) return;
    if (!policy?.version) {
      empty.textContent = "This saved analysis predates break selection. Re-run it with current AI to edit markers.";
      empty.classList.remove("hidden");
      return;
    }

    const duration = Number(transcript.duration) || 0;
    const maxByLoad = Math.floor((policy.max_ad_load_percent / 100) * duration /
      ((1 - policy.max_ad_load_percent / 100) * policy.planned_ad_seconds));
    const maxByGap = Math.max(0, Math.floor(Math.max(0, duration - 25) / policy.min_gap_seconds) + 1);
    const maxCount = Math.max(0, Math.min(policy.max_break_count || Math.ceil(duration / 1800 * 4), maxByLoad, maxByGap));
    const potentials = candidates.filter((item) => item.potential ||
      (item.potential === undefined && item.decision === "accepted"));
    const { key, state } = loadBreakSelection(job.id, candidates, maxCount, duration, policy);
    const persist = () => {
      saveBreakSelection(key, state);
      playbackPlan = null;
      if (activeJob?.id === job.id) activeJob.playback_plan = null;
      document.querySelector("#playback-downloads").classList.add("hidden");
      document.querySelector("#play-programme").disabled = true;
    };
    const seek = document.querySelector("#review-seek");
    seek.max = String(duration);
    seek.value = String(video.currentTime || 0);
    document.querySelector("#review-time").value = formatDuration(video.currentTime || 0);
    seek.oninput = () => { video.currentTime = Number(seek.value); };

    for (const cut of transcript.shot_boundaries || []) {
      const marker = document.createElement("button");
      marker.type = "button";
      marker.className = "cut-marker";
      marker.style.left = `${Math.max(0.4, Math.min(99.6, cut / duration * 100))}%`;
      marker.title = `Shot cut ${formatDuration(cut)} · click to review`;
      marker.setAttribute("aria-label", marker.title);
      marker.onclick = () => { video.currentTime = cut; };
      cutsLayer.append(marker);
    }

    const isSelected = (candidate) => state.selected.some((item) =>
      item.candidateId === candidate.candidate_id || Math.abs(item.time - candidate.time) < 0.05);
    for (const candidate of potentials) {
      const marker = document.createElement("button");
      marker.type = "button";
      marker.className = `potential-marker${isSelected(candidate) ? " potential-marker-selected" : ""}`;
      marker.style.left = `${Math.max(0.6, Math.min(99.4, candidate.time / duration * 100))}%`;
      marker.title = `${isSelected(candidate) ? "Selected" : "AI potential"} ${formatDuration(candidate.time)} · ${Math.round(candidate.confidence * 100)}% confidence`;
      marker.setAttribute("aria-label", marker.title);
      marker.onclick = () => {
        const index = state.selected.findIndex((item) => item.candidateId === candidate.candidate_id || Math.abs(item.time - candidate.time) < 0.05);
        if (index >= 0) {
          if (!state.excludedAI.includes(candidate.candidate_id)) state.excludedAI.push(candidate.candidate_id);
          state.selected.splice(index, 1);
          selectBestAIBreaks(state, candidates, state.target, policy.min_gap_seconds);
        } else {
          state.excludedAI = state.excludedAI.filter((id) => id !== candidate.candidate_id);
          const issue = placementLimitMessage(state, candidate.time, duration, policy, maxCount);
          if (issue) { showToast(issue); return; }
          state.selected.push({ time: candidate.time, source: "manual", candidateId: candidate.candidate_id });
          state.target = Math.max(state.target, state.selected.length);
          selectBestAIBreaks(state, candidates, state.target, policy.min_gap_seconds);
        }
        persist();
        renderBreakDecisions(job);
      };
      potentialLayer.append(marker);
    }

    const manualCount = state.selected.filter((item) => item.source === "manual").length;
    countRange.min = String(manualCount);
    countRange.max = String(maxCount);
    countRange.value = String(state.target);
    countValue.value = String(state.target);
    countRange.oninput = () => {
      state.target = Number(countRange.value);
      selectBestAIBreaks(state, candidates, state.target, policy.min_gap_seconds);
      persist();
      renderBreakDecisions(job);
    };
    const selectedAI = state.selected.filter((item) => item.source === "ai").length;
    status.textContent = state.selected.length < state.target
      ? `Only ${selectedAI} AI-safe point${selectedAI === 1 ? "" : "s"} found. Add ${state.target - state.selected.length} manual marker${state.target - state.selected.length === 1 ? "" : "s"} to reach ${state.target}.`
      : `${state.selected.length} of ${maxCount} allowed · ${manualCount} manual · ${policy.min_gap_seconds}s minimum spacing. Saved in this browser.`;
    document.querySelector("#break-meta").textContent = `${(transcript.shot_boundaries || []).length} cuts · ${potentials.length} AI potential · ${state.selected.length} selected · maximum ${maxCount}`;
    if (transcript.break_scoring_error) {
      warning.textContent = transcript.break_scoring_error;
      warning.classList.remove("hidden");
    }
    if (!candidates.length && !state.selected.length) {
      empty.textContent = "AI found no safe ad point. Review the video with the precise slider and place a marker if you find a suitable moment.";
      empty.classList.remove("hidden");
    }

    const addMarker = document.querySelector("#add-break-marker");
    addMarker.onclick = () => {
      const time = Number(video.currentTime);
      const issue = placementLimitMessage(state, time, duration, policy, maxCount);
      if (issue) { showToast(issue); return; }
      const nearby = potentials.find((candidate) => Math.abs(candidate.time - time) < 0.5);
      state.selected.push({ time, source: "manual", ...(nearby ? { candidateId: nearby.candidate_id } : {}) });
      state.selected.sort((left, right) => left.time - right.time);
      state.target = Math.max(state.target, state.selected.length);
      selectBestAIBreaks(state, candidates, state.target, policy.min_gap_seconds);
      persist();
      renderBreakDecisions(job);
    };

    for (const item of state.selected) {
      const marker = document.createElement("button");
      marker.type = "button";
      marker.className = `break-marker break-marker-accepted${item.source === "manual" ? " break-marker-manual" : ""}`;
      marker.style.left = `${Math.max(0.8, Math.min(99.2, item.time / duration * 100))}%`;
      marker.title = `${item.source === "manual" ? "Manual" : "AI selected"} ad marker at ${formatDuration(item.time)}`;
      marker.setAttribute("aria-label", marker.title);
      marker.onclick = () => { video.currentTime = item.time; };
      selectedLayer.append(marker);
    }

    for (const candidate of candidates) {
      const selected = isSelected(candidate);
      const card = document.createElement("li");
      card.className = `break-card ${selected ? "break-card-accepted" : candidate.potential ? "break-card-potential" : "break-card-rejected"}`;
      const header = document.createElement("div");
      header.className = "break-card-header";
      const time = document.createElement("button");
      time.type = "button";
      time.className = "break-time";
      time.textContent = formatDuration(candidate.time);
      time.onclick = () => { video.currentTime = candidate.time; };
      const verdict = document.createElement("span");
      verdict.className = `break-verdict ${selected ? "break-verdict-accepted" : candidate.potential ? "break-verdict-potential" : "break-verdict-rejected"}`;
      verdict.textContent = selected ? "SELECTED" : candidate.potential ? "AI POTENTIAL" : "WITHHELD";
      header.append(time, verdict);
      const score = document.createElement("p");
      score.className = "break-score";
      score.textContent = `Break naturalness ${Math.round(candidate.naturalness * 100)}% · disruption ${Math.round(candidate.disruption_risk * 100)}% · confidence ${Math.round(candidate.confidence * 100)}%`;
      const mood = document.createElement("p");
      mood.className = "break-signals";
      mood.textContent = `Preceding mood: ${candidate.preceding_scene_mood || "unclear"}${candidate.preceding_sensitive_contexts?.length ? ` · caution: ${candidate.preceding_sensitive_contexts.join(", ")}` : ""}`;
      const brandFit = document.createElement("p");
      brandFit.className = "break-brand-fit";
      const bestBrand = candidate.brand_recommendations?.[0];
      brandFit.textContent = bestBrand
        ? `Best contextual brand fit: ${bestBrand.display_name} · ${Math.round(bestBrand.fit_score * 100)}% · ${bestBrand.reason}`
        : "No safe brand fit was identified for the preceding scene.";
      if (candidate.blocked_brand_matches?.length) {
        const blocked = document.createElement("span");
        blocked.className = "break-brand-blocks";
        blocked.textContent = `Blocked by context: ${candidate.blocked_brand_matches.map((item) => `${item.display_name} (${item.blocked_contexts.join(", ")})`).join(" · ")}`;
        brandFit.append(document.createElement("br"), blocked);
      }
      const rationale = document.createElement("p");
      rationale.className = "break-rationale";
      rationale.textContent = candidate.ai_reason || candidate.scene_context || "Candidate from media evidence.";
      const signals = document.createElement("p");
      signals.className = "break-signals";
      signals.textContent = `Evidence: ${(candidate.signals || []).map((value) => value.replaceAll("_", " ")).join(" · ")}`;
      const reasons = document.createElement("p");
      reasons.className = "break-policy-reason";
      reasons.textContent = selected && state.selected.some((item) => item.source === "manual" && item.candidateId === candidate.candidate_id)
        ? "Human selected this time. Review the scene mood and story before playback."
        : candidate.potential ? (candidate.transition_evidence || "Passed AI scene, speech, pause, and confidence checks; spacing limits still apply.")
          : (candidate.reasons || []).map((reason) => reason.message).join(" ");
      card.append(header, score, mood, brandFit, rationale, signals, reasons);
      if (selected) {
        const remove = document.createElement("button");
        remove.type = "button";
        remove.className = "remove-break-marker";
        remove.textContent = "Remove marker";
        remove.onclick = () => {
          if (!state.excludedAI.includes(candidate.candidate_id)) state.excludedAI.push(candidate.candidate_id);
          state.selected = state.selected.filter((item) => item.candidateId !== candidate.candidate_id && Math.abs(item.time - candidate.time) >= 0.05);
          selectBestAIBreaks(state, candidates, state.target, policy.min_gap_seconds);
          persist();
          renderBreakDecisions(job);
        };
        card.append(remove);
      }
      list.append(card);
    }
    for (const item of state.selected.filter((selection) => !candidates.some((candidate) =>
      candidate.candidate_id === selection.candidateId || Math.abs(candidate.time - selection.time) < 0.5))) {
      const card = document.createElement("li");
      card.className = "break-card break-card-accepted";
      const header = document.createElement("div");
      header.className = "break-card-header";
      const time = document.createElement("button");
      time.type = "button";
      time.className = "break-time";
      time.textContent = formatDuration(item.time);
      time.onclick = () => { video.currentTime = item.time; };
      const verdict = document.createElement("span");
      verdict.className = "break-verdict break-verdict-accepted";
      verdict.textContent = "MANUAL MARKER";
      header.append(time, verdict);
      const note = document.createElement("p");
      note.className = "break-policy-reason";
      note.textContent = "Placed outside the AI potential list. Check preceding-scene mood, dialogue, and sensitive context.";
      const sceneMatches = precedingSceneBrandMatches(transcript, item.time);
      const safeMatch = sceneMatches.find((match) => match.recommended && !match.blocked);
      const brandFit = document.createElement("p");
      brandFit.className = "break-brand-fit";
      brandFit.textContent = safeMatch
        ? `Preceding-scene brand fit: ${safeMatch.display_name} · ${Math.round(safeMatch.fit_score * 100)}% · ${safeMatch.reason}`
        : "No safe brand fit for the preceding scene; choose a brand manually or leave the break unfilled.";
      const remove = document.createElement("button");
      remove.type = "button";
      remove.className = "remove-break-marker";
      remove.textContent = "Remove marker";
      remove.onclick = () => {
        state.selected = state.selected.filter((selection) => selection !== item);
        selectBestAIBreaks(state, candidates, state.target, policy.min_gap_seconds);
        persist();
        renderBreakDecisions(job);
      };
      card.append(header, note, brandFit, remove);
      list.append(card);
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

  function addToLibrary(job) {
    const list = document.querySelector("#library-list");
    const existing = list.querySelector(`[data-job-id="${CSS.escape(job.id)}"]`);
    if (existing) {
      existing.querySelector(".library-job-copy span").textContent = job.transcript ? "Transcript ready" : job.status === "processing" || job.status === "queued" ? "Transcribing…" : job.status === "failed" ? "Needs retry" : "Intake ready";
      return;
    }
    list.querySelector(".library-empty")?.remove();
    const button = document.createElement("button");
    button.className = "library-job";
    button.dataset.jobId = job.id;
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
    status.textContent = job.transcript ? "Transcript ready" : job.status === "processing" || job.status === "queued" ? "Transcribing…" : job.status === "failed" ? "Needs retry" : "Intake ready";
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
    list.prepend(button);
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
  document.querySelector("#retry-transcription").addEventListener("click", async () => {
    const videoURL = document.querySelector("#video-preview").src;
    const jobId = videoURL.split("/").pop();
    if (!jobId) return;
    const button = document.querySelector("#retry-transcription");
    button.disabled = true;
    try {
      const response = await fetch(`/api/jobs/${encodeURIComponent(jobId)}/transcribe`, { method: "POST" });
      const job = await response.json();
      if (!response.ok) throw new Error(job.error || "Retry could not be started.");
      showJob(job);
      watchJob(job.id);
    } catch (error) {
      showToast(error.message || "Retry could not be started.");
    } finally {
      button.disabled = false;
    }
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
  const adPreview = document.querySelector("#ad-preview");
  const reviewSeek = document.querySelector("#review-seek");
  const reviewTime = document.querySelector("#review-time");
  previewVideo.addEventListener("timeupdate", () => {
    reviewSeek.value = String(previewVideo.currentTime || 0);
    reviewTime.value = formatDuration(previewVideo.currentTime || 0);
    if (!playbackPlan || previewVideo.paused || activePlaybackBreak) return;
    const due = playbackPlan.breaks.find((item) => !playedBreaks.has(item.break_id) && previewVideo.currentTime >= item.time);
    if (due) playScheduledBreak(due);
  });
  previewVideo.addEventListener("loadedmetadata", () => {
    reviewSeek.max = String(previewVideo.duration || 0);
  });
  adPreview.addEventListener("playing", () => {
    if (activePlaybackBreak && !playbackAdStarted) {
      playbackAdStarted = true;
      recordPlaybackEvent("ad_start", activePlaybackBreak);
    }
  });
  adPreview.addEventListener("timeupdate", () => {
    if (!activePlaybackBreak || !Number.isFinite(adPreview.duration)) return;
    document.querySelector("#ad-countdown").textContent = `${Math.max(0, Math.ceil(adPreview.duration - adPreview.currentTime))}s`;
  });
  adPreview.addEventListener("ended", async () => {
    const item = activePlaybackBreak;
    if (!item) return;
    await recordPlaybackEvent("ad_complete", item);
    resumeProgramme(item);
  });
  adPreview.addEventListener("error", async () => {
    const item = activePlaybackBreak;
    if (!item || adFailureHandled) return;
    adFailureHandled = true;
    await recordPlaybackEvent("error", item, "demo creative failed to load");
    await recordPlaybackEvent("skip", item, "fail-open: resume programme");
    resumeProgramme(item);
  });
  document.querySelector("#skip-demo-ad").addEventListener("click", async () => {
    const item = activePlaybackBreak;
    if (!item) return;
    adFailureHandled = true;
    await recordPlaybackEvent("skip", item, "reviewer skipped synthetic demo slate");
    resumeProgramme(item);
  });

  fetch("/api/brands").then((response) => response.json()).then(({ brands = [] }) => {
    catalogBrands = brands;
    if (activeJob?.transcript) renderBreakDecisions(activeJob);
  }).catch(() => {
    document.querySelector("#playback-status").textContent = "The synthetic brand catalogue could not be loaded; VMAP creation is unavailable.";
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
