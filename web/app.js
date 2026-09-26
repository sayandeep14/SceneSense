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
  const pollingJobs = new Set();

  const formatDuration = (seconds) => {
    const total = Math.max(0, Math.floor(seconds));
    const hours = Math.floor(total / 3600);
    const minutes = Math.floor((total % 3600) / 60);
    const remainder = total % 60;
    return hours ? `${hours}:${String(minutes).padStart(2, "0")}:${String(remainder).padStart(2, "0")}` : `${minutes}:${String(remainder).padStart(2, "0")}`;
  };

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
    const transcript = job.transcript;
    panel.classList.toggle("hidden", !transcript);
    sceneList.replaceChildren();
    pauseList.replaceChildren();
    warning.classList.add("hidden");
    pausePanel.classList.add("hidden");
    if (!transcript) return;

    const scenes = transcript.scenes || [];
    const pauses = transcript.silence_intervals || [];
    const shotBoundaries = transcript.shot_boundaries || [];
    meta.textContent = `${scenes.length} scenes · ${shotBoundaries.length} shot cuts · ${pauses.length} low-audio pauses · ${transcript.scene_model || "scene model pending"} · ${transcript.scene_prompt_version || ""}${transcript.cache_hit ? " · cached" : ""}`;
    const issues = [transcript.pause_detection_error, transcript.shot_detection_error, transcript.scene_analysis_error].filter(Boolean);
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
    const panel = document.querySelector("#break-decisions");
    const markers = document.querySelector("#break-markers");
    const list = document.querySelector("#break-list");
    const meta = document.querySelector("#break-meta");
    const warning = document.querySelector("#break-warning");
    const empty = document.querySelector("#break-empty");
    const transcript = job.transcript;
    panel.classList.toggle("hidden", !transcript);
    markers.classList.toggle("hidden", !transcript?.break_candidates?.length);
    markers.replaceChildren();
    list.replaceChildren();
    warning.classList.add("hidden");
    empty.classList.add("hidden");
    if (!transcript) return;

    const candidates = transcript.break_candidates || [];
    const policy = transcript.break_policy;
    if (!policy?.version) {
      meta.textContent = "This saved analysis predates break scoring.";
      empty.textContent = "Use “Re-run with current AI” above to review safe break opportunities.";
      empty.classList.remove("hidden");
      return;
    }
    meta.textContent = `${policy.accepted_count} selected · ${candidates.length - policy.accepted_count} withheld · ${policy.version} · ${policy.min_gap_seconds}s minimum gap · ${policy.max_ad_load_percent}% ad-load cap for ${policy.planned_ad_seconds}s test spots`;
    if (transcript.break_scoring_error) {
      warning.textContent = transcript.break_scoring_error;
      warning.classList.remove("hidden");
    }
    if (!candidates.length) {
      empty.textContent = "No reliable pause or scene boundary was found for a break. The safe decision is to keep playing the programme.";
      empty.classList.remove("hidden");
      return;
    }
    for (const candidate of candidates) {
      const accepted = candidate.decision === "accepted";
      const seekTo = () => {
        const video = document.querySelector("#video-preview");
        video.currentTime = candidate.time;
        video.play().catch(() => {});
      };
      const marker = document.createElement("button");
      marker.type = "button";
      marker.className = `break-marker ${accepted ? "break-marker-accepted" : "break-marker-rejected"}`;
      marker.style.left = `${Math.max(1, Math.min(99, candidate.time / transcript.duration * 100))}%`;
      marker.title = `${accepted ? "Selected" : "Withheld"} break at ${formatDuration(candidate.time)}`;
      marker.setAttribute("aria-label", marker.title);
      marker.addEventListener("click", seekTo);
      markers.append(marker);

      const card = document.createElement("li");
      card.className = `break-card ${accepted ? "break-card-accepted" : "break-card-rejected"}`;
      const header = document.createElement("div");
      header.className = "break-card-header";
      const time = document.createElement("button");
      time.type = "button";
      time.className = "break-time";
      time.textContent = formatDuration(candidate.time);
      time.setAttribute("aria-label", `Play candidate at ${formatDuration(candidate.time)}`);
      time.addEventListener("click", seekTo);
      const verdict = document.createElement("span");
      verdict.className = `break-verdict ${accepted ? "break-verdict-accepted" : "break-verdict-rejected"}`;
      verdict.textContent = accepted ? "SELECTED" : "WITHHELD";
      header.append(time, verdict);
      const score = document.createElement("p");
      score.className = "break-score";
      score.textContent = transcript.break_scoring_status === "complete" ? `AI naturalness ${Math.round(candidate.naturalness * 100)}% · disruption ${Math.round(candidate.disruption_risk * 100)}% · confidence ${Math.round(candidate.confidence * 100)}%` : "AI score unavailable";
      const rationale = document.createElement("p");
      rationale.className = "break-rationale";
      rationale.textContent = candidate.ai_reason || candidate.scene_context || "Candidate from media evidence.";
      const signals = document.createElement("p");
      signals.className = "break-signals";
      signals.textContent = `Evidence: ${(candidate.signals || []).map((signal) => signal.replaceAll("_", " ")).join(" · ")}`;
      const policyText = document.createElement("p");
      policyText.className = "break-policy-reason";
      policyText.textContent = accepted ? "Passed scene, speech, pause and pacing checks." : (candidate.reasons || []).map((reason) => reason.message).join(" ");
      card.append(header, score, rationale, signals, policyText);
      list.append(card);
    }
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
