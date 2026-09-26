// SceneSense player screen: plays the programme, interrupts it with linear ads at break times,
// and resumes. Used for single-break previews and for the full VMAP plan.
(() => {
  const $ = (selector) => document.querySelector(selector);
  const dialog = $("#player-screen");
  const shell = dialog.querySelector(".player-shell");
  const stage = $("#player-stage");
  const programme = $("#player-programme");
  const ad = $("#player-ad");
  const bumper = $("#player-mood-bumper");
  const adUI = $("#player-ad-ui");
  const upcoming = $("#player-upcoming");
  const skip = $("#player-skip");
  const cta = $("#player-cta");
  const ctaButton = $("#player-cta-button");
  const paused = $("#player-ad-paused");
  const endCard = $("#player-end");
  const bigPlay = $("#player-big-play");
  const toggle = $("#player-toggle");
  const mute = $("#player-mute");
  const fullscreen = $("#player-fullscreen");
  const seek = $("#player-seek");
  const trackFill = $("#player-track-fill");
  const caption = $("#player-caption");
  const railList = $("#player-rail-list");

  const svg = (path) => `<svg viewBox="0 0 24 24" width="18" height="18" aria-hidden="true" fill="currentColor">${path}</svg>`;
  const icons = {
    play: svg('<path d="M8 5.5v13l10.5-6.5z"/>'),
    pause: svg('<path d="M7 5h3.5v14H7zM13.5 5H17v14h-3.5z"/>'),
    volume: svg('<path d="M4 9.5h3.5L12 5.5v13l-4.5-4H4z"/><path d="M15 8.5a4.5 4.5 0 0 1 0 7M17.5 6a8 8 0 0 1 0 12" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"/>'),
    muted: svg('<path d="M4 9.5h3.5L12 5.5v13l-4.5-4H4z"/><path d="m15.5 9.5 5 5m0-5-5 5" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"/>'),
    expand: svg('<path d="M4 9V4h5M15 4h5v5M20 15v5h-5M9 20H4v-5" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"/>'),
    collapse: svg('<path d="M9 4v5H4M20 9h-5V4M15 20v-5h5M4 15h5v5" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"/>'),
  };
  const clock = (value) => {
    const total = Math.max(0, Math.floor(Number(value) || 0));
    const hours = Math.floor(total / 3600);
    const minutes = Math.floor((total % 3600) / 60);
    const seconds = String(total % 60).padStart(2, "0");
    return hours ? `${hours}:${String(minutes).padStart(2, "0")}:${seconds}` : `${minutes}:${seconds}`;
  };
  const hostname = (url) => { try { return new URL(url).hostname.replace(/^www\./, ""); } catch { return ""; } };

  let session = null;
  let isMuted = false;

  const setIcon = (button, name, label) => {
    button.innerHTML = icons[name];
    button.setAttribute("aria-label", label);
    button.title = label;
  };
  const activeVideo = () => (session?.active ? ad : programme);
  const emit = (type, item, detail = "") => {
    try { session?.onEvent?.(type, item, detail); } catch { /* Event reporting never interrupts playback. */ }
  };
  const skipRule = (item) => item.allowSkip ? `skippable after ${item.skipAfterSec} s` : "not skippable";

  function syncToggle() {
    const video = activeVideo();
    if (video.paused) setIcon(toggle, "play", "Play");
    else setIcon(toggle, "pause", "Pause");
  }

  function showBigPlay() {
    bigPlay.classList.remove("hidden");
    syncToggle();
  }

  function renderRail() {
    railList.replaceChildren();
    $("#player-rail-count").textContent = `${session.breaks.length} scheduled`;
    for (const item of session.breaks) {
      const row = document.createElement("li");
      const outcome = session.outcomes.get(item.id);
      const state = session.active?.id === item.id ? "playing" : outcome ? "done" : session.played.has(item.id) ? "passed" : "upcoming";
      row.className = `player-rail-item player-rail-${state}`;
      const button = document.createElement("button");
      button.type = "button";
      button.setAttribute("aria-label", `Jump to five seconds before the ad at ${clock(item.time)}`);
      button.onclick = () => {
        if (session.active) return;
        // Jumping to a break passes earlier ones; play the chosen break, not the latest one passed.
        for (const other of session.breaks) {
          if (other.time < item.time) session.played.add(other.id);
          else if (other.time >= item.time) {
            session.played.delete(other.id);
            session.outcomes.delete(other.id);
          }
        }
        hideEnd();
        programme.currentTime = Math.max(0, item.time - 5);
        programme.play().catch(showBigPlay);
        renderRail();
        renderDots();
      };
      const time = document.createElement("span");
      time.className = "player-rail-time";
      time.textContent = clock(item.time);
      const copy = document.createElement("span");
      copy.className = "player-rail-copy";
      const name = document.createElement("strong");
      name.textContent = item.brandName;
      const rule = document.createElement("span");
      rule.textContent = `${item.durationSec} s · ${skipRule(item)}${item.clickUrl ? ` · ${item.ctaLabel || "Visit website"} ↗` : ""}`;
      copy.append(name, rule);
      const status = document.createElement("span");
      status.className = "player-rail-status";
      status.textContent = state === "playing" ? "Playing" : outcome ? outcome.label : state === "passed" ? "Passed" : "Up next";
      button.append(time, copy, status);
      row.append(button);
      railList.append(row);
    }
  }

  function renderDots() {
    const dots = $("#player-break-dots");
    dots.replaceChildren();
    for (const item of session.breaks) {
      const dot = document.createElement("span");
      dot.className = `player-break-dot${session.played.has(item.id) ? " player-break-dot-played" : ""}`;
      dot.style.left = `${Math.min(100, Math.max(0, item.time / session.duration * 100))}%`;
      dots.append(dot);
    }
  }

  function hideEnd() {
    endCard.classList.add("hidden");
    shell.classList.remove("is-ended");
  }

  function updateProgramme() {
    if (!session) return;
    const time = programme.currentTime || 0;
    seek.value = String(time);
    trackFill.style.width = `${Math.min(100, time / session.duration * 100)}%`;
    if (!session.active) $("#player-time").textContent = `${clock(time)} / ${clock(session.duration)}`;
    if (session.active || session.finished || programme.seeking) return;

    const next = session.breaks.find((item) => !session.played.has(item.id) && item.time > time);
    const lead = next ? next.time - time : Infinity;
    upcoming.classList.toggle("hidden", !(lead <= 5 && !programme.paused));
    if (lead <= 5) upcoming.textContent = `Ad in ${Math.max(1, Math.ceil(lead))}`;

    const due = session.breaks.filter((item) => !session.played.has(item.id) && item.time <= time).at(-1);
    if (due) {
      // A viewer who seeks past several breaks sees only the latest one, then continues from where they sought to.
      for (const item of session.breaks) if (item.time <= due.time) session.played.add(item.id);
      startAd(due, time - due.time > 1.5 ? time : due.time);
      return;
    }
    if (session.mode === "preview" && session.played.size && time >= session.endAt) finish();
  }

  function beginCreative(item) {
    if (!session || session.active !== item) return;
    session.bumper = false;
    session.bumperTimer = null;
    bumper.classList.add("hidden");
    shell.classList.remove("is-bumper");
    shell.classList.add("is-ad");
    adUI.classList.remove("hidden");
    paused.classList.add("hidden");
    bigPlay.classList.add("hidden");
    const index = session.breaks.indexOf(item) + 1;
    $("#player-ad-meta").textContent = `${item.brandName} · ${clock(item.durationSec)}`;
    $("#player-time").textContent = `Ad ${index} of ${session.breaks.length}`;
    $("#player-cta-mark").textContent = (item.brandName || "?").trim().charAt(0).toUpperCase();
    $("#player-cta-brand").textContent = item.brandName;
    const domain = hostname(item.clickUrl);
    $("#player-cta-domain").textContent = domain || "Sponsored";
    ctaButton.textContent = `${item.ctaLabel || "Visit website"} ↗`;
    ctaButton.classList.toggle("hidden", !item.clickUrl);
    cta.classList.toggle("player-cta-plain", !item.clickUrl);
    skip.classList.toggle("hidden", !item.allowSkip);
    skip.disabled = true;
    skip.textContent = item.allowSkip && item.skipAfterSec > 0 ? `Skip in ${item.skipAfterSec}` : "Skip ad ⏭";
    skip.disabled = item.allowSkip && item.skipAfterSec > 0;
    $("#player-ad-progress-fill").style.width = "0%";
    caption.textContent = `Ad break at ${clock(item.time)} · ${item.brandName} · ${item.durationSec} s, ${skipRule(item)}.`;
    session.adStarted = false;
    ad.src = item.creativeUrl;
    ad.muted = isMuted;
    ad.currentTime = 0;
    emit("break_start", item);
    ad.play().catch((error) => {
      if (session?.active !== item) return;
      if (error?.name === "NotAllowedError") showBigPlay();
      else endAd("error", "the creative could not start");
    });
    renderRail();
    renderDots();
    syncToggle();
  }

  function startAd(item, resumeAt) {
    session.active = item;
    session.resumeAt = resumeAt;
    programme.pause();
    upcoming.classList.add("hidden");
    hideEnd();
    if (item.mood && ["warm", "reflective", "calm"].includes(item.mood)) {
      session.bumper = true;
      bumper.dataset.mood = item.mood;
      $("#bumper-title").textContent = item.mood === "warm" ? "A warm moment" : item.mood === "reflective" ? "A moment to reflect" : "A quiet pause";
      $("#bumper-subtitle").textContent = `Next: ${item.brandName} · an ad matched to the moment`;
      bumper.classList.remove("hidden");
      shell.classList.add("is-bumper");
      caption.textContent = "Synthetic mood transition for the demo; the ad starts next.";
      session.bumperTimer = window.setTimeout(() => beginCreative(item), 1700);
      renderRail();
      renderDots();
      return;
    }
    beginCreative(item);
  }

  function updateAd() {
    const item = session?.active;
    if (!item) return;
    const total = Number.isFinite(ad.duration) && ad.duration > 0 ? ad.duration : item.durationSec;
    const elapsed = ad.currentTime || 0;
    $("#player-ad-meta").textContent = `${item.brandName} · ${clock(Math.ceil(total - elapsed))}`;
    $("#player-ad-progress-fill").style.width = `${Math.min(100, elapsed / total * 100)}%`;
    if (item.allowSkip) {
      const left = item.skipAfterSec - elapsed;
      skip.disabled = left > 0;
      skip.textContent = left > 0 ? `Skip in ${Math.ceil(left)}` : "Skip ad ⏭";
    }
  }

  function endAd(type, detail = "") {
    const item = session?.active;
    if (!item) return;
    const watched = ad.currentTime || 0;
    session.active = null;
    const label = type === "skip" ? `Skipped at ${watched.toFixed(1)} s` : type === "ad_complete" ? "Watched" : "Failed · resumed";
    session.outcomes.set(item.id, { type, label, watched, clicked: session.clicked.has(item.id) });
    emit(type, item, detail);
    ad.pause();
    ad.removeAttribute("src");
    ad.load();
    shell.classList.remove("is-ad");
    adUI.classList.add("hidden");
    paused.classList.add("hidden");
    bigPlay.classList.add("hidden");
    caption.textContent = type === "error"
      ? "The ad could not play, so the programme resumed without it."
      : `Back to the programme at ${clock(session.resumeAt)}.`;
    programme.currentTime = session.resumeAt;
    programme.play().then(() => emit("resume", item)).catch(showBigPlay);
    renderRail();
    renderDots();
    syncToggle();
  }

  function finish() {
    if (!session || session.finished) return;
    session.finished = true;
    programme.pause();
    upcoming.classList.add("hidden");
    shell.classList.add("is-ended");
    $("#player-end-title").textContent = session.mode === "preview" ? "Preview finished" : "Programme finished";
    const log = $("#player-end-log");
    log.replaceChildren();
    for (const item of session.breaks) {
      const outcome = session.outcomes.get(item.id);
      const row = document.createElement("li");
      const when = document.createElement("span");
      when.textContent = clock(item.time);
      const text = document.createElement("span");
      text.textContent = !outcome ? `${item.brandName} · not reached`
        : outcome.type === "skip" ? `${item.brandName} · skipped after ${outcome.watched.toFixed(1)} s of ${item.durationSec} s`
          : outcome.type === "ad_complete" ? `${item.brandName} · watched in full (${item.durationSec} s)`
            : `${item.brandName} · creative failed, programme resumed`;
      if (outcome?.clicked) text.textContent += " · website opened";
      row.append(when, text);
      log.append(row);
    }
    endCard.classList.remove("hidden");
    caption.textContent = session.mode === "preview" ? "Replay to watch the break again, or close to keep editing." : "All scheduled breaks are done.";
    syncToggle();
  }

  function begin() {
    if (!session) return;
    programme.currentTime = session.startAt;
    programme.play().catch(showBigPlay);
  }

  function open(options) {
    teardown();
    const duration = Number(options.duration) || 0;
    const breaks = [...options.breaks].sort((left, right) => left.time - right.time);
    session = {
      ...options, duration, breaks, options,
      startAt: Math.max(0, Number(options.startAt) || 0),
      endAt: Number(options.endAt) || duration,
      played: new Set(), outcomes: new Map(), clicked: new Set(),
      active: null, resumeAt: 0, finished: false, adStarted: false, bumper: false, bumperTimer: null,
    };
    hideEnd();
    shell.classList.remove("is-ad", "is-bumper");
    bumper.classList.add("hidden");
    adUI.classList.add("hidden");
    upcoming.classList.add("hidden");
    bigPlay.classList.add("hidden");
    $("#player-mode").textContent = options.mode === "preview"
      ? `BREAK PREVIEW · ${clock(breaks[0]?.time)}`
      : `PROGRAMME · ${breaks.length} AD BREAK${breaks.length === 1 ? "" : "S"}`;
    $("#player-title").textContent = options.title || "Programme";
    caption.textContent = options.mode === "preview"
      ? `Starts ${Math.round((breaks[0]?.time || 0) - session.startAt)} s before the break and continues for a few seconds after it.`
      : "Plays the programme from the start with every break in the VMAP plan.";
    seek.max = String(duration);
    programme.muted = isMuted;
    setIcon(mute, isMuted ? "muted" : "volume", isMuted ? "Unmute" : "Mute");
    setIcon(fullscreen, "expand", "Full screen");
    renderRail();
    renderDots();
    if (!dialog.open) dialog.showModal();
    if (programme.getAttribute("src") !== options.programmeUrl) {
      programme.src = options.programmeUrl;
      programme.addEventListener("loadedmetadata", begin, { once: true });
      programme.load();
    } else if (programme.readyState >= 1) {
      begin();
    } else {
      programme.addEventListener("loadedmetadata", begin, { once: true });
    }
    syncToggle();
  }

  function teardown() {
    if (session?.bumperTimer) window.clearTimeout(session.bumperTimer);
    bumper.classList.add("hidden");
    shell.classList.remove("is-bumper");
    programme.pause();
    ad.pause();
    if (ad.getAttribute("src")) {
      ad.removeAttribute("src");
      ad.load();
    }
    session = null;
  }

  function togglePlayback() {
    if (!session || session.finished || session.bumper) return;
    const video = activeVideo();
    bigPlay.classList.add("hidden");
    if (video.paused) video.play().catch(showBigPlay);
    else video.pause();
  }

  programme.addEventListener("timeupdate", updateProgramme);
  programme.addEventListener("seeked", updateProgramme);
  programme.addEventListener("ended", () => { if (!session?.active) finish(); });
  for (const video of [programme, ad]) {
    video.addEventListener("play", syncToggle);
    video.addEventListener("pause", syncToggle);
  }
  ad.addEventListener("playing", () => {
    const item = session?.active;
    if (!item) return;
    paused.classList.add("hidden");
    bigPlay.classList.add("hidden");
    if (!session.adStarted) {
      session.adStarted = true;
      emit("ad_start", item);
    }
  });
  ad.addEventListener("timeupdate", updateAd);
  ad.addEventListener("ended", () => endAd("ad_complete"));
  ad.addEventListener("error", () => { if (session?.active && ad.getAttribute("src")) endAd("error", "the creative failed to load"); });
  skip.addEventListener("click", () => {
    if (!session?.active || skip.disabled) return;
    endAd("skip", `skipped at ${(ad.currentTime || 0).toFixed(1)} s`);
  });
  ctaButton.addEventListener("click", () => {
    const item = session?.active;
    if (!item?.clickUrl) return;
    window.open(item.clickUrl, "_blank", "noopener,noreferrer");
    session.clicked.add(item.id);
    ad.pause();
    paused.classList.remove("hidden");
    emit("click_through", item, item.clickUrl);
  });
  $("#player-ad-resume").addEventListener("click", () => ad.play().catch(showBigPlay));
  bigPlay.addEventListener("click", togglePlayback);
  toggle.addEventListener("click", togglePlayback);
  stage.addEventListener("click", (event) => {
    if (event.target === programme && !session?.active) togglePlayback();
  });
  seek.addEventListener("input", () => {
    if (!session || session.active) return;
    hideEnd();
    session.finished = false;
    programme.currentTime = Number(seek.value);
  });
  mute.addEventListener("click", () => {
    isMuted = !isMuted;
    programme.muted = ad.muted = isMuted;
    setIcon(mute, isMuted ? "muted" : "volume", isMuted ? "Unmute" : "Mute");
  });
  fullscreen.addEventListener("click", () => {
    if (document.fullscreenElement) document.exitFullscreen().catch(() => {});
    else shell.requestFullscreen?.().catch(() => {});
  });
  document.addEventListener("fullscreenchange", () => {
    const active = document.fullscreenElement === shell;
    setIcon(fullscreen, active ? "collapse" : "expand", active ? "Exit full screen" : "Full screen");
  });
  $("#player-replay").addEventListener("click", () => { if (session) open(session.options); });
  $("#player-end-close").addEventListener("click", () => dialog.close());
  $("#player-close").addEventListener("click", () => dialog.close());
  dialog.addEventListener("close", () => {
    if (document.fullscreenElement) document.exitFullscreen().catch(() => {});
    const onClose = session?.onClose;
    teardown();
    onClose?.();
  });
  dialog.addEventListener("keydown", (event) => {
    if (event.target.closest("input, textarea, select") || event.metaKey || event.ctrlKey || event.altKey) return;
    const key = event.key.toLowerCase();
    if (key === " " || key === "k") {
      if (event.target.closest("button") && key === " ") return;
      event.preventDefault();
      togglePlayback();
    } else if (key === "m") {
      mute.click();
    } else if (key === "f") {
      fullscreen.click();
    }
  });

  window.SceneSensePlayer = { open, close: () => dialog.close() };
})();
