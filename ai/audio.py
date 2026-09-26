"""Audio scene signals from YAMNet (AudioSet, 521 classes) on 0.96 s log-mel patches.

The log-mel front end reproduces YAMNet's: 16 kHz mono, 25 ms Hann windows every 10 ms,
512-point FFT magnitudes, 64 HTK mel bands from 125 to 7500 Hz, log(mel + 0.001).
"""

from __future__ import annotations

import shutil
import subprocess
from functools import lru_cache
from pathlib import Path
from typing import Any

import numpy as np

from visual import models_dir

SAMPLE_RATE = 16000
WINDOW, HOP, FFT = 400, 160, 512
MEL_BANDS, PATCH_FRAMES = 64, 96
PATCH_HOP_FRAMES = 48  # 0.48 s between patch starts
PATCH_SECONDS = PATCH_FRAMES * HOP / SAMPLE_RATE
PATCH_STEP_SECONDS = PATCH_HOP_FRAMES * HOP / SAMPLE_RATE

SPEECH_LABELS = {"Speech", "Child speech, kid speaking", "Conversation", "Narration, monologue", "Babbling",
                 "Speech synthesizer", "Shout", "Yell", "Whispering", "Female speech, woman speaking",
                 "Male speech, man speaking", "Chatter"}
MUSIC_LABELS = {"Music", "Musical instrument", "Singing", "Song", "Theme music", "Background music",
                "Soundtrack music", "Jingle (music)", "Orchestra"}


class AudioModelError(Exception):
    """A safe error to surface when audio classification is unavailable."""


@lru_cache(maxsize=1)
def _model():
    import onnxruntime as ort

    root = models_dir() / "yamnet"
    if not (root / "yamnet.onnx").is_file():
        raise AudioModelError("The YAMNet model is missing; run `python ai/fetch_models.py`.")
    options = ort.SessionOptions()
    options.log_severity_level = 3
    session = ort.InferenceSession(str(root / "yamnet.onnx"), options, providers=["CPUExecutionProvider"])
    labels = [line.strip() for line in (root / "labels.txt").read_text().splitlines() if line.strip()]
    return session, labels


@lru_cache(maxsize=1)
def _mel_matrix() -> np.ndarray:
    def hz_to_mel(hz):
        return 1127.0 * np.log1p(np.asarray(hz, dtype=np.float64) / 700.0)

    bins = FFT // 2 + 1
    spectrogram_mel = hz_to_mel(np.linspace(0.0, SAMPLE_RATE / 2, bins))[1:]
    edges = np.linspace(hz_to_mel(125.0), hz_to_mel(7500.0), MEL_BANDS + 2)
    lower, center, upper = edges[:-2], edges[1:-1], edges[2:]
    lower_slope = (spectrogram_mel[:, None] - lower) / (center - lower)
    upper_slope = (upper - spectrogram_mel[:, None]) / (upper - center)
    weights = np.maximum(0.0, np.minimum(lower_slope, upper_slope))
    return np.pad(weights, ((1, 0), (0, 0))).astype(np.float32)  # the DC bin carries no weight


def load_audio(video_path: Path) -> np.ndarray:
    ffmpeg = shutil.which("ffmpeg")
    if not ffmpeg:
        raise AudioModelError("FFmpeg is required for audio analysis.")
    result = subprocess.run(
        [ffmpeg, "-hide_banner", "-loglevel", "error", "-nostdin", "-i", str(video_path), "-vn", "-ac", "1",
         "-ar", str(SAMPLE_RATE), "-f", "f32le", "pipe:1"], capture_output=True, check=False,
    )
    if result.returncode != 0:
        raise AudioModelError("FFmpeg could not decode the audio track.")
    return np.frombuffer(result.stdout, dtype=np.float32)


def log_mel(samples: np.ndarray) -> np.ndarray:
    if len(samples) < WINDOW:
        return np.zeros((0, MEL_BANDS), dtype=np.float32)
    window = (0.5 - 0.5 * np.cos(2 * np.pi * np.arange(WINDOW) / WINDOW)).astype(np.float32)  # periodic Hann
    count = 1 + (len(samples) - WINDOW) // HOP
    mel = _mel_matrix()
    rows = []
    for offset in range(0, count, 4096):
        idx = np.arange(offset, min(count, offset + 4096))[:, None] * HOP + np.arange(WINDOW)
        magnitude = np.abs(np.fft.rfft(samples[idx] * window, n=FFT)).astype(np.float32)
        rows.append(np.log(magnitude @ mel + 0.001))
    return np.concatenate(rows)


def classify(samples: np.ndarray) -> dict[str, Any]:
    """Class scores, loudness, speech and music probability for each 0.96 s patch."""
    session, labels = _model()
    features = log_mel(samples)
    starts = range(0, max(0, len(features) - PATCH_FRAMES) + 1, PATCH_HOP_FRAMES)
    speech_idx = [i for i, label in enumerate(labels) if label in SPEECH_LABELS]
    music_idx = [i for i, label in enumerate(labels) if label in MUSIC_LABELS]
    silence_idx = labels.index("Silence") if "Silence" in labels else None
    scores, times, loudness = [], [], []
    for start in starts:
        patch = features[start:start + PATCH_FRAMES][None, None]
        logits = session.run(["class_scores"], {"audio": patch.astype(np.float32)})[0][0]
        scores.append(1.0 / (1.0 + np.exp(-np.clip(logits, -50, 50))))  # the export returns logits
        begin = start * HOP
        chunk = samples[begin:begin + PATCH_FRAMES * HOP]
        loudness.append(20 * np.log10(max(float(np.sqrt(np.mean(chunk ** 2))), 1e-5)))
        times.append(start * HOP / SAMPLE_RATE + PATCH_SECONDS / 2)
    matrix = np.array(scores, dtype=np.float32) if scores else np.zeros((0, len(labels)), dtype=np.float32)
    return {
        "labels": labels, "times": np.array(times), "scores": matrix, "loudness_db": np.array(loudness),
        "speech": matrix[:, speech_idx].max(axis=1) if len(matrix) else np.zeros(0),
        "music": matrix[:, music_idx].max(axis=1) if len(matrix) else np.zeros(0),
        "silence": matrix[:, silence_idx] if len(matrix) and silence_idx is not None else np.zeros(len(matrix)),
    }


def _window(audio: dict[str, Any], start: float, end: float) -> np.ndarray:
    return (audio["times"] >= start) & (audio["times"] <= end)


def dominant_label(audio: dict[str, Any], start: float, end: float) -> str:
    mask = _window(audio, start, end)
    if not mask.any():
        return ""
    mean = audio["scores"][mask].mean(axis=0)
    return audio["labels"][int(np.argmax(mean))]


def speech_at(audio: dict[str, Any], time: float, radius: float = 0.5) -> float:
    """Highest speech probability in patches centred within `radius` seconds of `time`."""
    mask = _window(audio, time - radius, time + radius)
    return float(audio["speech"][mask].max()) if mask.any() else 0.0


def speech_between(audio: dict[str, Any], start: float, end: float) -> float:
    """Highest speech probability among patches overlapping [start, end]."""
    mask = (audio["times"] + PATCH_SECONDS / 2 >= start) & (audio["times"] - PATCH_SECONDS / 2 <= end)
    return float(audio["speech"][mask].max()) if mask.any() else 0.0


def boundary_shift(audio: dict[str, Any], time: float, context: float = 6.0) -> dict[str, Any]:
    """Compare the audio scene just before and just after `time`."""
    before = _window(audio, time - context, time - 0.5)
    after = _window(audio, time + 0.5, time + context)
    if not before.any() or not after.any():
        return {"shift": 0.0, "loudness_change_db": 0.0, "before": "", "after": ""}
    # Hellinger-style comparison of the mean class distributions.
    a = np.sqrt(np.clip(audio["scores"][before].mean(axis=0), 0, None))
    b = np.sqrt(np.clip(audio["scores"][after].mean(axis=0), 0, None))
    cosine = float(a @ b / max(float(np.linalg.norm(a) * np.linalg.norm(b)), 1e-8))
    return {
        "shift": round(1 - cosine, 4),
        "loudness_change_db": round(float(audio["loudness_db"][after].mean() - audio["loudness_db"][before].mean()), 2),
        "before": dominant_label(audio, time - context, time - 0.5),
        "after": dominant_label(audio, time + 0.5, time + context),
        "music_change": round(float(audio["music"][after].mean() - audio["music"][before].mean()), 3),
    }


def speech_free_intervals(audio: dict[str, Any], threshold: float = 0.2, minimum: float = 0.9) -> list[dict]:
    """Stretches where no patch detects speech: lulls in dialogue even when music or ambience plays."""
    intervals, start = [], None
    step = PATCH_STEP_SECONDS
    for time, speech in zip(audio["times"], audio["speech"]):
        quiet = speech < threshold
        if quiet and start is None:
            start = time - step / 2
        elif not quiet and start is not None:
            if time - step / 2 - start >= minimum:
                intervals.append({"start": round(float(start), 3), "end": round(float(time - step / 2), 3)})
            start = None
    if start is not None and len(audio["times"]) and audio["times"][-1] + step / 2 - start >= minimum:
        intervals.append({"start": round(float(start), 3), "end": round(float(audio["times"][-1] + step / 2), 3)})
    return intervals
