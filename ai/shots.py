"""Shot segmentation and keyframe sampling in one FFmpeg decoding pass.

FFmpeg decodes the programme once and produces two streams: small frames at DETECT_FPS for
shot detection, and a keyframe pool at POOL_FPS for the vision models. PySceneDetect finds hard
cuts (AdaptiveDetector) and fades (ThresholdDetector); a twin-comparison detector finds
dissolves, which PySceneDetect does not model.
"""

from __future__ import annotations

import shutil
import subprocess
import tempfile
from pathlib import Path
from typing import Any

import cv2
import numpy as np
from scenedetect import FrameTimecode
from scenedetect.detectors import AdaptiveDetector, ThresholdDetector

from versions import SHOT_DETECTOR_VERSION

DETECT_FPS = 12
DETECT_WIDTH, DETECT_HEIGHT = 160, 90
POOL_FPS = 2
POOL_HEIGHT = 224
MIN_SHOT_SECONDS = 0.5
MERGE_SECONDS = 0.6
DISSOLVE_MIN_SECONDS, DISSOLVE_MAX_SECONDS = 0.4, 3.0
LONG_SHOT_SECONDS = 8.0
EXTRA_KEYFRAME_EVERY = 4.0
MAX_KEYFRAMES_PER_SHOT = 5


class ShotDetectionError(Exception):
    """A safe error to surface when the video cannot be segmented."""


def _mean_abs(a: np.ndarray, b: np.ndarray) -> float:
    return float(np.abs(a.astype(np.int16) - b.astype(np.int16)).mean())


def _detect_dissolves(gray: np.ndarray, diffs: np.ndarray, hard: list[float]) -> list[float]:
    """Twin-comparison: a run of moderate frame differences that adds up to a large change,
    where the middle frame is close to a blend of the endpoints (rules out pans and zooms).
    `gray` holds small uint8 frames; diffs[i] is the mean change from frame i to i + 1."""
    if len(gray) < 4:
        return []
    median = float(np.median(diffs))
    mad = float(np.median(np.abs(diffs - median))) * 1.4826
    low = max(1.5, median + 3 * mad)
    found: list[float] = []
    index = 0
    while index < len(diffs):
        if diffs[index] <= low:
            index += 1
            continue
        start = index
        end = index
        while end + 1 < len(diffs) and diffs[end + 1] > low * 0.6 and end - start < DISSOLVE_MAX_SECONDS * DETECT_FPS:
            end += 1
        first, last = start, end + 1  # frame indices bracketing the gradual change
        seconds = (last - first) / DETECT_FPS
        index = end + 1
        if not DISSOLVE_MIN_SECONDS <= seconds <= DISSOLVE_MAX_SECONDS:
            continue
        mid_time = (first + last) / 2 / DETECT_FPS
        if any(abs(mid_time - cut) < seconds / 2 + MERGE_SECONDS for cut in hard):
            continue
        change = _mean_abs(gray[last], gray[first])
        if change < 18:
            continue
        middle = gray[(first + last) // 2].astype(np.float32)
        blend = (gray[first].astype(np.float32) + gray[last].astype(np.float32)) / 2
        blend_error = float(np.abs(middle - blend).mean()) / change
        if blend_error < 0.3:
            found.append(round(mid_time, 3))
    return found


def _keyframe_times(start: float, end: float) -> list[float]:
    times = [(start + end) / 2]
    if end - start > LONG_SHOT_SECONDS:
        extra = np.arange(start + EXTRA_KEYFRAME_EVERY / 2, end - 1.0, EXTRA_KEYFRAME_EVERY)
        times = sorted({round(float(value), 3) for value in [*extra, times[0]]})
        if len(times) > MAX_KEYFRAMES_PER_SHOT:
            picks = np.linspace(0, len(times) - 1, MAX_KEYFRAMES_PER_SHOT).round().astype(int)
            times = [times[pick] for pick in picks]
    return times


def segment_shots(video_path: Path, work_dir: Path, duration: float) -> dict[str, Any]:
    """Return shot boundaries, shots with keyframe paths, and the keyframe pool directory."""
    ffmpeg = shutil.which("ffmpeg")
    if not ffmpeg:
        raise ShotDetectionError("FFmpeg is required for shot detection.")
    pool = Path(tempfile.mkdtemp(prefix="keyframes-", dir=work_dir))
    graph = (f"[0:v]split=2[a][b];[a]fps={DETECT_FPS},scale={DETECT_WIDTH}:{DETECT_HEIGHT}:flags=area,"
             f"format=bgr24[d];[b]fps={POOL_FPS},scale=-2:{POOL_HEIGHT}[k]")
    process = subprocess.Popen(
        [ffmpeg, "-hide_banner", "-loglevel", "error", "-nostdin", "-i", str(video_path), "-filter_complex", graph,
         "-map", "[d]", "-f", "rawvideo", "pipe:1", "-map", "[k]", "-q:v", "4", str(pool / "f_%06d.jpg")],
        stdout=subprocess.PIPE, stderr=subprocess.PIPE,
    )
    frame_bytes = DETECT_WIDTH * DETECT_HEIGHT * 3
    adaptive = AdaptiveDetector(min_scene_len=int(MIN_SHOT_SECONDS * DETECT_FPS))
    fades = ThresholdDetector(threshold=12, min_scene_len=int(MIN_SHOT_SECONDS * DETECT_FPS))
    cut_frames: list[int] = []
    fade_frames: list[int] = []
    gray: list[np.ndarray] = []
    diffs: list[float] = []
    index = -1
    assert process.stdout is not None
    while True:
        data = process.stdout.read(frame_bytes)
        if len(data) < frame_bytes:
            break
        index += 1
        frame = np.frombuffer(data, dtype=np.uint8).reshape(DETECT_HEIGHT, DETECT_WIDTH, 3)
        timecode = FrameTimecode(index, fps=float(DETECT_FPS))
        cut_frames += [cut.frame_num for cut in adaptive.process_frame(timecode, frame)]
        fade_frames += [cut.frame_num for cut in fades.process_frame(timecode, frame)]
        small = cv2.resize(cv2.cvtColor(frame, cv2.COLOR_BGR2GRAY), (48, 27), interpolation=cv2.INTER_AREA)
        if gray:
            diffs.append(_mean_abs(small, gray[-1]))
        gray.append(small)
    stderr = process.communicate()[1].decode("utf-8", "replace")
    if process.returncode != 0 or index < 1:
        shutil.rmtree(pool, ignore_errors=True)
        raise ShotDetectionError("FFmpeg could not decode the video for shot detection. " + stderr[-200:].strip())
    last = FrameTimecode(index, fps=float(DETECT_FPS))
    cut_frames += [cut.frame_num for cut in adaptive.post_process(last)]
    fade_frames += [cut.frame_num for cut in fades.post_process(last)]

    stack = np.stack(gray)
    hard = sorted({round(frame / DETECT_FPS, 3) for frame in cut_frames})
    faded = sorted({round(frame / DETECT_FPS, 3) for frame in fade_frames})
    dissolves = _detect_dissolves(stack, np.array(diffs), hard + faded)
    boundaries: list[dict[str, Any]] = []
    # A fade or dissolve explains any hard cut the content detector reports inside it.
    for kind, times in (("fade", faded), ("dissolve", dissolves), ("cut", hard)):
        for time in times:
            if not MIN_SHOT_SECONDS <= time <= duration - MIN_SHOT_SECONDS:
                continue
            if any(abs(time - existing["time"]) < MERGE_SECONDS for existing in boundaries):
                continue
            before = max(0, int(round(time * DETECT_FPS)) - 1)
            after = min(len(stack) - 1, int(round(time * DETECT_FPS)) + 1)
            strength = _mean_abs(stack[after], stack[before]) / 255
            boundaries.append({"time": time, "kind": kind, "strength": round(strength, 4)})
    boundaries.sort(key=lambda item: item["time"])

    pool_frames = sorted(pool.glob("f_*.jpg"))
    if not pool_frames:
        shutil.rmtree(pool, ignore_errors=True)
        raise ShotDetectionError("No keyframes could be sampled from the video.")

    def pool_frame(time: float) -> str:
        position = min(len(pool_frames) - 1, max(0, int(round(time * POOL_FPS))))
        return str(pool_frames[position])

    edges = [0.0, *[item["time"] for item in boundaries], float(duration)]
    shots = []
    for number in range(len(edges) - 1):
        start, end = edges[number], edges[number + 1]
        times = _keyframe_times(start, end)
        shots.append({
            "shot_id": f"shot-{number + 1:04d}", "start": round(start, 3), "end": round(end, 3),
            "keyframes": [{"time": round(time, 3), "path": pool_frame(time)} for time in times],
        })
    return {"boundaries": boundaries, "shots": shots, "pool_dir": str(pool), "pool_fps": POOL_FPS,
            "pool_frame": pool_frame, "detector": SHOT_DETECTOR_VERSION}
