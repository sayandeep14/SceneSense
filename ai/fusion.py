"""Fuse visual, audio, pause, transcript, and transition signals into a scene-change score per shot
boundary, then shortlist distinct candidates for the vision-language model."""

from __future__ import annotations

from typing import Any

import numpy as np

WEIGHTS = {"visual_window": 0.30, "visual_local": 0.10, "audio": 0.20, "pause": 0.15, "text": 0.20, "transition": 0.05}
TRANSITION_SCORE = {"fade": 1.0, "dissolve": 0.8, "cut": 0.0}
WINDOW_SHOTS, WINDOW_SECONDS = 3, 30.0
SHORTLIST_MIN_SCORE = 0.30
MIN_SCENE_SECONDS = 15.0
MAX_SHORTLIST = 60
PAUSE_RADIUS = 2.0
MAX_LULL_SECONDS = 20.0


def _ramp(value: float, low: float, high: float) -> float:
    return float(min(1.0, max(0.0, (value - low) / (high - low))))


def _window_mean(embeddings: np.ndarray, shots: list[dict], indices: range, time: float) -> np.ndarray | None:
    picks = [i for i in indices if 0 <= i < len(shots) and abs((shots[i]["start"] + shots[i]["end"]) / 2 - time) <= WINDOW_SECONDS]
    if not picks:
        return None
    weights = np.array([max(0.3, shots[i]["end"] - shots[i]["start"]) for i in picks])[:, None]
    mean = (embeddings[picks] * weights).sum(axis=0)
    return mean / max(float(np.linalg.norm(mean)), 1e-8)


def pause_at(time: float, silences: list[dict], speech_free: list[dict]) -> tuple[float, str]:
    """Quiet time within PAUSE_RADIUS of `time`: silence counts fully, a lull in dialogue at 70%.
    A speech-free stretch longer than MAX_LULL_SECONDS is a non-dialogue passage, not a lull."""
    best, source = 0.0, ""
    lulls = [item for item in speech_free if float(item["end"]) - float(item["start"]) <= MAX_LULL_SECONDS]
    for interval, weight, name in [*((item, 1.0, "silence") for item in silences),
                                   *((item, 0.7, "lull in dialogue") for item in lulls)]:
        start, end = float(interval["start"]), float(interval["end"])
        if start - 0.3 <= time <= end + 0.3:
            overlap = min(end, time + PAUSE_RADIUS) - max(start, time - PAUSE_RADIUS)
            if overlap * weight > best:
                best, source = overlap * weight, name
    return round(max(0.0, best), 2), source


def score_boundaries(boundaries: list[dict], shots: list[dict], embeddings: np.ndarray, audio_shift: dict[float, dict],
                     speech: dict[float, float], silences: list[dict], speech_free: list[dict],
                     text_shift: dict[float, float]) -> list[dict[str, Any]]:
    scored = []
    for index, boundary in enumerate(boundaries):
        time = boundary["time"]
        signals: dict[str, float] = {}
        raw: dict[str, Any] = {"shot_transition": boundary["kind"]}
        if index + 1 < len(embeddings):
            local = 1 - float(embeddings[index] @ embeddings[index + 1])
            raw["visual_local_distance"] = round(local, 4)
            signals["visual_local"] = _ramp(local, 0.10, 0.30)
            before = _window_mean(embeddings, shots, range(index - WINDOW_SHOTS + 1, index + 1), time)
            after = _window_mean(embeddings, shots, range(index + 1, index + 1 + WINDOW_SHOTS), time)
            if before is not None and after is not None:
                window = 1 - float(before @ after)
                raw["visual_window_distance"] = round(window, 4)
                signals["visual_window"] = _ramp(window, 0.08, 0.25)
        shift = audio_shift.get(time)
        if shift:
            raw["audio"] = shift
            signals["audio"] = _ramp(float(shift["shift"]), 0.05, 0.40)
        pause, pause_source = pause_at(time, silences, speech_free)
        raw["pause_seconds"], raw["pause_source"] = pause, pause_source
        raw["speech_at_cut"] = round(float(speech.get(time, 0.0)), 3)
        signals["pause"] = _ramp(pause, 0.0, 2.0)
        if time in text_shift:
            raw["text_distance"] = text_shift[time]
            signals["text"] = _ramp(float(text_shift[time]), 0.15, 0.50)
        signals["transition"] = TRANSITION_SCORE.get(boundary["kind"], 0.0)
        total = sum(WEIGHTS[name] for name in signals)
        score = sum(WEIGHTS[name] * value for name, value in signals.items()) / total if total else 0.0
        scored.append({"time": time, "boundary_index": index, "scene_score": round(score, 4),
                       "signal_scores": {name: round(value, 3) for name, value in signals.items()}, **raw})
    return scored


def shortlist(scored: list[dict], duration: float) -> list[dict]:
    """Keep the strongest boundary in every MIN_SCENE_SECONDS neighbourhood, then cap the count while
    keeping coverage across the programme."""
    kept: list[dict] = []
    for item in sorted(scored, key=lambda entry: (-entry["scene_score"], entry["time"])):
        if item["scene_score"] < SHORTLIST_MIN_SCORE:
            break
        if all(abs(item["time"] - other["time"]) >= MIN_SCENE_SECONDS for other in kept):
            kept.append(item)
    if len(kept) > MAX_SHORTLIST:
        buckets: dict[int, dict] = {}
        for item in kept:
            bucket = min(MAX_SHORTLIST - 1, int(item["time"] / max(duration, 1) * MAX_SHORTLIST))
            if bucket not in buckets or item["scene_score"] > buckets[bucket]["scene_score"]:
                buckets[bucket] = item
        chosen = {id(item) for item in buckets.values()}
        for item in kept:
            if len(chosen) >= MAX_SHORTLIST:
                break
            chosen.add(id(item))
        kept = [item for item in kept if id(item) in chosen]
    return sorted(kept, key=lambda entry: entry["time"])
