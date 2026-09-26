"""Emotional pacing map: a tension curve across the programme with its peaks, cliffhangers, and valleys.

Tension blends three signals on a fixed time grid: the vision model's per-scene emotional intensity,
audio arousal (loudness plus YAMNet's emotional classes such as crying, shouting, or sad/scary music),
and editing pace (cuts per minute). The map is evidence for reviewers; the break policy does not use it.
"""

from __future__ import annotations

from typing import Any

import numpy as np

from versions import PACING_VERSION

STEP_SECONDS = 2.0
WEIGHTS = {"scene": 0.5, "audio": 0.3, "cuts": 0.2}
DRAMATIC_SOUNDS = {
    "Shout", "Yell", "Children shouting", "Screaming", "Crying, sobbing", "Wail, moan", "Groan", "Gasp",
    "Heart sounds, heartbeat", "Sad music", "Scary music", "Angry music", "Exciting music", "Siren", "Slam",
    "Thunder", "Explosion", "Gunshot, gunfire",
}
PEAK_MIN, PEAK_PROMINENCE, PEAK_SEPARATION = 0.55, 0.12, 30.0
VALLEY_MAX, VALLEY_SEPARATION = 0.35, 60.0
CLIFFHANGER_WINDOW = 15.0  # seconds from the end of a high stretch to the next scene change
PLATEAU_TOLERANCE = 0.03


def _smooth(values: np.ndarray, width: int) -> np.ndarray:
    if len(values) < 3 or width < 2:
        return values
    kernel = np.ones(width) / width
    padded = np.pad(values, (width // 2, width - 1 - width // 2), mode="edge")
    return np.convolve(padded, kernel, mode="valid")


def _rank(values: np.ndarray) -> np.ndarray:
    """Map values to 0-1 by percentile rank, so loud and quiet programmes compare fairly."""
    if len(values) == 0:
        return values
    order = values.argsort().argsort()
    return order / max(1, len(values) - 1)


def audio_arousal(sound: dict[str, Any], grid: np.ndarray) -> tuple[np.ndarray, list[str]]:
    """Per grid step: arousal 0-1 and the most prominent dramatic sound (or "")."""
    if not len(sound["times"]):
        return np.zeros(len(grid)), [""] * len(grid)
    labels = sound["labels"]
    dramatic = [index for index, label in enumerate(labels) if label in DRAMATIC_SOUNDS]
    cues = sound["scores"][:, dramatic]
    loudness = _rank(np.asarray(sound["loudness_db"], dtype=np.float64))
    emotion = cues.max(axis=1)
    per_patch = 0.5 * loudness + 0.5 * np.clip(emotion * 2, 0, 1)
    index = np.clip(np.searchsorted(sound["times"], grid), 0, len(sound["times"]) - 1)
    strongest = cues.argmax(axis=1)
    names = [labels[dramatic[strongest[i]]] if emotion[i] >= 0.25 else "" for i in index]
    return _smooth(per_patch[index], 3), names


def cut_rate(shot_times: list[float], grid: np.ndarray, window: float = 20.0) -> np.ndarray:
    """Cuts per minute around each step, saturating at 30 cuts per minute."""
    times = np.asarray(sorted(shot_times), dtype=np.float64)
    if not len(times):
        return np.zeros(len(grid))
    counts = np.searchsorted(times, grid + window / 2) - np.searchsorted(times, grid - window / 2)
    return np.clip(counts * (60 / window) / 30, 0, 1)


def scene_intensity(scenes: list[dict[str, Any]], grid: np.ndarray) -> np.ndarray:
    values = np.full(len(grid), np.nan)
    for scene in scenes:
        if "emotional_intensity" not in scene:
            continue
        mask = (grid >= scene["start"]) & (grid < scene["end"])
        values[mask] = float(scene["emotional_intensity"])
    return values


def _scene_at(scenes: list[dict[str, Any]], time: float) -> dict[str, Any] | None:
    return next((scene for scene in scenes if scene["start"] <= time < scene["end"]), None)


def _extremes(values: np.ndarray, grid: np.ndarray, peaks: bool) -> list[int]:
    """Local maxima (or minima) with a minimum height/depth and spacing, strongest first."""
    sign = 1 if peaks else -1
    candidates = []
    for i in range(len(values)):
        left, right = values[max(0, i - 1)], values[min(len(values) - 1, i + 1)]
        if sign * values[i] < sign * left or sign * values[i] < sign * right:
            continue
        if peaks:
            window = values[max(0, i - 15):i + 16]  # about ±30 s
            if values[i] < PEAK_MIN or values[i] - window.min() < PEAK_PROMINENCE:
                continue
        elif values[i] > VALLEY_MAX:
            continue
        candidates.append(i)
    separation = PEAK_SEPARATION if peaks else VALLEY_SEPARATION
    chosen: list[int] = []
    for i in sorted(candidates, key=lambda index: -sign * values[index]):
        if all(abs(grid[i] - grid[j]) >= separation for j in chosen):
            chosen.append(i)
    return sorted(chosen)


def build_pacing(duration: float, scenes: list[dict[str, Any]], sound: dict[str, Any] | None,
                 shot_times: list[float], scene_change_times: list[float]) -> dict[str, Any]:
    grid = np.arange(0.0, max(duration, STEP_SECONDS), STEP_SECONDS)
    scene = scene_intensity(scenes, grid)
    audio, cues = audio_arousal(sound, grid) if sound is not None else (np.zeros(len(grid)), [""] * len(grid))
    cuts = cut_rate(shot_times, grid)
    has_scene = ~np.isnan(scene)
    weighted = WEIGHTS["audio"] * audio + WEIGHTS["cuts"] * cuts + WEIGHTS["scene"] * np.nan_to_num(scene)
    total = WEIGHTS["audio"] + WEIGHTS["cuts"] + WEIGHTS["scene"] * has_scene
    tension = np.clip(_smooth(weighted / total, 3), 0, 1)

    def describe(index: int) -> dict[str, Any]:
        time = float(grid[index])
        current = _scene_at(scenes, time) or {}
        drivers = {"scene": float(np.nan_to_num(scene[index])), "audio": float(audio[index]), "cuts": float(cuts[index])}
        return {
            "time": round(time, 1), "value": round(float(tension[index]), 3),
            "label": ", ".join(current.get("tone", [])[:2]) or current.get("summary", "")[:40],
            "scene_id": current.get("scene_id", ""), "cue": cues[index],
            "driver": max(drivers, key=drivers.get),
        }

    peaks = []
    for index in _extremes(tension, grid, peaks=True):
        # A peak is the whole stretch that stays near its height: label it in the middle, and call it a
        # cliffhanger when the story cuts to a new scene while (or right after) that stretch plays.
        start = end = index
        while start > 0 and tension[start - 1] >= tension[index] - PLATEAU_TOLERANCE:
            start -= 1
        while end + 1 < len(tension) and tension[end + 1] >= tension[index] - PLATEAU_TOLERANCE:
            end += 1
        peak = describe((start + end) // 2)
        peak["start"], peak["end"] = round(float(grid[start]), 1), round(float(grid[end] + STEP_SECONDS), 1)
        following = [change for change in scene_change_times
                     if grid[start] <= change <= grid[end] + STEP_SECONDS + CLIFFHANGER_WINDOW]
        peak["kind"] = "cliffhanger" if following else "peak"
        if following:
            peak["scene_change"] = round(following[0], 3)
        peaks.append(peak)
    valleys = [describe(index) for index in _extremes(tension, grid, peaks=False)]
    top = max(peaks, key=lambda peak: peak["value"], default=None)
    cliffhangers = sum(peak["kind"] == "cliffhanger" for peak in peaks)
    summary = f"{len(peaks)} dramatic peak(s), {cliffhangers} just before a scene change"
    if top:
        summary += f"; the most intense moment is at {int(top['time'] // 60)}:{int(top['time'] % 60):02d}"
        summary += f" ({top['label']})" if top["label"] else ""
    summary += f"; {len(valleys)} calm valley(s)."
    return {
        "version": PACING_VERSION, "step_sec": STEP_SECONDS,
        "tension": [round(float(value), 3) for value in tension],
        "components": {
            "scene": [None if np.isnan(value) else round(float(value), 3) for value in scene],
            "audio": [round(float(value), 3) for value in audio],
            "cuts": [round(float(value), 3) for value in cuts],
        },
        "peaks": peaks, "valleys": valleys, "summary": summary,
    }
