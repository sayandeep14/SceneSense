#!/usr/bin/env python3
"""AI worker: Bengali ASR, local shot/audio/visual signals, scene fusion, and model judgements.
Input and output are one JSON object each on stdin/stdout; progress lines go to stderr."""

from __future__ import annotations

import hashlib
import json
import math
import os
import re
import shutil
import subprocess
import sys
import tempfile
import urllib.error
import urllib.request
import wave
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
from typing import Any

from scene_ai import (BOUNDARY_PROMPT_VERSION, KEYFRAMES_PER_SCENE, SCENE_DESCRIBE_PROMPT_VERSION, SENSITIVE_CONTEXTS,
                      SceneAIError, ad_friendliness, describe_scenes, judge_boundaries)
from versions import (AUDIO_MODEL_VERSION, CLIP_MODEL_VERSION, EMBEDDING_MODEL, FUSION_VERSION, PIPELINE_CACHE_VERSION,
                      SHOT_DETECTOR_VERSION)

API_URL = "https://api.groq.com/openai/v1/audio/transcriptions"
SARVAM_API_URL = "https://api.sarvam.ai/speech-to-text"
MODEL = "whisper-large-v3-turbo"
SARVAM_MODEL = "saaras:v4"
SARVAM_CHUNK_SECONDS = 25
SARVAM_PARALLEL_REQUESTS = 4
ASR_PROVIDER = os.environ.get("ASR_PROVIDER", "groq").strip().lower() or "groq"
SCENE_MODEL = os.environ.get("OPENAI_VISION_MODEL", "gpt-4o-mini").strip() or "gpt-4o-mini"
BREAK_MODEL = os.environ.get("OPENAI_BREAK_MODEL", SCENE_MODEL).strip() or SCENE_MODEL
SILENCE_DETECTOR = "silencedetect:-32dB:0.45s"
MAX_AUDIO_BYTES = 25 * 1024 * 1024
ACCEPT_UNCERTAIN_SCORE = 0.60
SPEECH_EVIDENCE = 0.3


class WorkerError(Exception):
    """Safe-to-display worker error; must never contain credentials or raw media."""


def progress(stage: str, percent: int, message: str) -> None:
    """Report pipeline progress to the Go server on stderr."""
    print("@@progress " + json.dumps({"stage": stage, "progress": percent, "message": message}),
          file=sys.stderr, flush=True)


def load_brand_catalog(path: Path) -> list[dict[str, Any]]:
    try:
        raw = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise WorkerError("The brand catalogue is missing or invalid JSON.") from exc
    if not isinstance(raw, list) or not raw:
        raise WorkerError("The brand catalogue must be a non-empty list.")

    brands: list[dict[str, Any]] = []
    seen_ids: set[str] = set()
    for item in raw:
        if not isinstance(item, dict):
            raise WorkerError("The brand catalogue contains an invalid entry.")
        brand_id = item.get("brand_id")
        if not isinstance(brand_id, str) or not brand_id.strip() or brand_id in seen_ids:
            raise WorkerError("Brand IDs must be non-empty and unique.")
        seen_ids.add(brand_id)
        for field in ("display_name", "category"):
            if not isinstance(item.get(field), str) or not item[field].strip():
                raise WorkerError(f"Brand {brand_id} is missing {field}.")
        for field in ("target_contexts", "negative_contexts"):
            contexts = item.get(field)
            if not isinstance(contexts, list) or not contexts or any(not isinstance(value, str) or not value.strip() for value in contexts):
                raise WorkerError(f"Brand {brand_id} has invalid {field}.")
        creatives = item.get("creatives")
        if not isinstance(creatives, list) or not creatives:
            raise WorkerError(f"Brand {brand_id} has no creative options.")
        for creative in creatives:
            if (
                not isinstance(creative, dict)
                or not isinstance(creative.get("id"), str)
                or not isinstance(creative.get("url"), str)
                or not isinstance(creative.get("language"), str)
                or not isinstance(creative.get("duration_sec"), int)
                or creative["duration_sec"] <= 0
            ):
                raise WorkerError(f"Brand {brand_id} has an invalid creative option.")
        brands.append(item)
    return brands


def extract_audio(video_path: Path, work_dir: Path) -> Path:
    work_dir.mkdir(parents=True, exist_ok=True)
    provider = _asr_provider()
    suffix = ".wav" if provider == "sarvam" else ".mp3"
    handle = tempfile.NamedTemporaryFile(prefix="transcription-", suffix=suffix, dir=work_dir, delete=False)
    audio_path = Path(handle.name)
    handle.close()
    audio_options = (
        ["-c:a", "pcm_s16le", "-f", "wav"]
        if provider == "sarvam"
        else ["-c:a", "libmp3lame", "-b:a", "48k", "-f", "mp3"]
    )
    try:
        subprocess.run(
            [
                "ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-i", str(video_path),
                "-map", "0:a:0", "-vn", "-ac", "1", "-ar", "16000", *audio_options, str(audio_path),
            ],
            check=True,
            capture_output=True,
            timeout=7 * 60,
        )
        size = audio_path.stat().st_size
        if size <= 0:
            raise WorkerError("The extracted audio is empty.")
        if provider == "groq" and size > MAX_AUDIO_BYTES:
            raise WorkerError(f"Compressed audio is {size / (1024 * 1024):.1f} MB; the current upload limit is 25 MB.")
        return audio_path
    except subprocess.TimeoutExpired as exc:
        audio_path.unlink(missing_ok=True)
        raise WorkerError("Audio extraction timed out.") from exc
    except subprocess.CalledProcessError as exc:
        audio_path.unlink(missing_ok=True)
        raise WorkerError("Could not extract audio from this video.") from exc
    except OSError as exc:
        audio_path.unlink(missing_ok=True)
        raise WorkerError("FFmpeg is unavailable or temporary audio storage could not be used.") from exc
    except Exception:
        audio_path.unlink(missing_ok=True)
        raise


def _validate_transcript(payload: dict[str, Any], model: str = MODEL) -> dict[str, Any]:
    duration = float(payload.get("duration") or 0)
    if duration < 0:
        raise WorkerError("The transcription provider returned an invalid duration.")
    source_segments = payload.get("segments") or []
    segments: list[dict[str, Any]] = []
    previous_end = 0.0
    timestamp_adjustments = 0
    for source in source_segments:
        start, end = float(source.get("start", -1)), float(source.get("end", -1))
        if not math.isfinite(start) or not math.isfinite(end) or start < 0 or end < start:
            raise WorkerError("The transcription provider returned invalid timestamps.")
        if duration and start >= duration:
            timestamp_adjustments += 1
            continue
        if start + 0.05 < previous_end:
            raise WorkerError("The transcription provider returned invalid timestamps.")
        if duration and end > duration:
            end = duration
            timestamp_adjustments += 1
        words = []
        previous_word_end = start
        for word in source.get("words") or []:
            word_start, word_end = float(word.get("start", -1)), float(word.get("end", -1))
            if not math.isfinite(word_start) or not math.isfinite(word_end) or word_start < 0 or word_end < word_start:
                raise WorkerError("The transcription provider returned invalid word timestamps.")
            if duration and word_start >= duration:
                timestamp_adjustments += 1
                continue
            normalized_word_end = min(word_end, end, duration) if duration else min(word_end, end)
            if (
                word_start < start - 0.1
                or word_start + 0.05 < previous_word_end
                or normalized_word_end < word_start
            ):
                raise WorkerError("The transcription provider returned invalid word timestamps.")
            if normalized_word_end < word_end:
                timestamp_adjustments += 1
            previous_word_end = normalized_word_end
            words.append({"word": str(word.get("word", "")), "start": word_start, "end": normalized_word_end})
        segments.append({
            "text": str(source.get("text", "")).strip(), "start": start, "end": end,
            "words": words,
        })
        previous_end = end

    if not segments:
        words = payload.get("words") or []
        for word in words:
            start, end = float(word.get("start", -1)), float(word.get("end", -1))
            if not math.isfinite(start) or not math.isfinite(end) or start < 0 or end < start:
                raise WorkerError("The transcription provider returned invalid word timestamps.")
            if duration and start >= duration:
                timestamp_adjustments += 1
                continue
            if start + 0.05 < previous_end:
                raise WorkerError("The transcription provider returned invalid word timestamps.")
            if duration and end > duration:
                end = duration
                timestamp_adjustments += 1
            previous_end = end
            text = str(word.get("word", "")).strip()
            segments.append({"text": text, "start": start, "end": end,
                             "words": [{"word": text, "start": start, "end": end}]})

    return {
        "language": str(payload.get("language", "")),
        "duration": duration,
        "text": (
            " ".join(segment["text"] for segment in segments if segment["text"])
            if timestamp_adjustments else str(payload.get("text", "")).strip()
        ),
        "segments": segments,
        "model": model,
        "timestamp_adjustments": timestamp_adjustments,
    }


def _asr_provider() -> str:
    provider = os.environ.get("ASR_PROVIDER", ASR_PROVIDER).strip().lower() or "groq"
    if provider not in ("groq", "sarvam"):
        raise WorkerError("ASR_PROVIDER must be either 'groq' or 'sarvam'.")
    return provider


def _provider_model(provider: str) -> str:
    if provider == "sarvam":
        return os.environ.get("SARVAM_ASR_MODEL", SARVAM_MODEL).strip() or SARVAM_MODEL
    return os.environ.get("GROQ_ASR_MODEL", MODEL).strip() or MODEL


def _multipart(audio_path: Path, filename: str) -> tuple[bytes, str]:
    boundary = "----SceneSenseBoundary7MA4YWxkTrZu0gW"
    chunks: list[bytes] = []
    for name, value in (
        ("model", _provider_model("groq")),
        ("language", "bn"),
        ("response_format", "verbose_json"),
        ("timestamp_granularities[]", "segment"),
        ("timestamp_granularities[]", "word"),
    ):
        chunks.append(
            f"--{boundary}\r\nContent-Disposition: form-data; name=\"{name}\"\r\n\r\n{value}\r\n".encode()
        )
    safe_name = Path(filename).name.replace('"', "_")
    chunks.append(
        f"--{boundary}\r\nContent-Disposition: form-data; name=\"file\"; filename=\"{safe_name}.mp3\"\r\nContent-Type: audio/mpeg\r\n\r\n".encode()
    )
    chunks.append(audio_path.read_bytes())
    chunks.append(f"\r\n--{boundary}--\r\n".encode())
    return b"".join(chunks), f"multipart/form-data; boundary={boundary}"


def _transcribe_groq(audio_path: Path, filename: str) -> dict[str, Any]:
    api_key = os.environ.get("GROQ_API_KEY", "").strip()
    if not api_key:
        raise WorkerError("GROQ_API_KEY is not configured for this service.")
    body, content_type = _multipart(audio_path, filename)
    request = urllib.request.Request(
        API_URL,
        data=body,
        headers={
            "Authorization": f"Bearer {api_key}",
            "Content-Type": content_type,
            # Identify the API client instead of urllib's generic default UA.
            "User-Agent": "hoichoi-contextual-ad-lab/0.1",
        },
        method="POST",
    )
    try:
        with urllib.request.urlopen(request, timeout=8 * 60) as response:
            payload = json.loads(response.read(12 * 1024 * 1024))
    except urllib.error.HTTPError as exc:
        content_type = exc.headers.get_content_type() if exc.headers else "unknown"
        try:
            detail = json.loads(exc.read(64 * 1024))
            error = detail.get("error", {}) if isinstance(detail, dict) else {}
            code = error.get("code") or error.get("type") if isinstance(error, dict) else None
            message = error.get("message", "") if isinstance(error, dict) else str(error)
        except (json.JSONDecodeError, OSError):
            code, message = None, ""
        safe_code = code if isinstance(code, str) and re.fullmatch(r"[A-Za-z0-9_-]{1,80}", code) else None
        safe_hints = (
            ("credential", "credential"), ("api key", "credential"), ("permission", "account access"),
            ("forbidden", "account access"), ("not authorized", "account access"),
            ("billing", "billing or quota"), ("quota", "billing or quota"), ("credit", "billing or quota"),
            ("region", "regional availability"),
        )
        hint = next((label for needle, label in safe_hints if needle in str(message).lower()), None)
        details = [value for value in (safe_code, hint) if value]
        if exc.code == 403 and not details:
            details.append("check Groq organization/project model permissions")
        if content_type in ("text/html", "text/plain"):
            details.append(f"provider response is {content_type}")
        suffix = f" ({', '.join(details)})" if details else ""
        raise WorkerError(f"Transcription provider returned HTTP {exc.code}{suffix}.") from exc
    except (urllib.error.URLError, TimeoutError) as exc:
        raise WorkerError("Could not reach the transcription provider, or the request timed out.") from exc
    except (json.JSONDecodeError, OSError) as exc:
        raise WorkerError("The transcription provider returned an unreadable response.") from exc
    return _validate_transcript(payload, _provider_model("groq"))


def _sarvam_multipart(audio_path: Path, model: str) -> tuple[bytes, str]:
    boundary = "----SceneSenseSarvamBoundary" + hashlib.sha256(os.urandom(32)).hexdigest()[:24]
    chunks: list[bytes] = []
    for name, value in (
        ("model", model),
        ("language_code", "bn-IN"),
        ("mode", "transcribe"),
        ("with_timestamps", "true"),
    ):
        chunks.append(
            f"--{boundary}\r\nContent-Disposition: form-data; name=\"{name}\"\r\n\r\n{value}\r\n".encode()
        )
    chunks.append(
        f"--{boundary}\r\nContent-Disposition: form-data; name=\"file\"; filename=\"audio.wav\"\r\nContent-Type: audio/wav\r\n\r\n".encode()
    )
    chunks.append(audio_path.read_bytes())
    chunks.append(f"\r\n--{boundary}--\r\n".encode())
    return b"".join(chunks), f"multipart/form-data; boundary={boundary}"


def _split_sarvam_audio(audio_path: Path, work_dir: Path) -> tuple[Path, list[tuple[Path, float, float]]]:
    chunk_dir = Path(tempfile.mkdtemp(prefix="sarvam-audio-", dir=work_dir))
    pattern = chunk_dir / "chunk-%04d.wav"
    try:
        subprocess.run(
            [
                "ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-i", str(audio_path),
                "-f", "segment", "-segment_time", str(SARVAM_CHUNK_SECONDS),
                "-reset_timestamps", "1", "-c:a", "pcm_s16le", str(pattern),
            ],
            check=True, capture_output=True, timeout=7 * 60,
        )
        paths = sorted(chunk_dir.glob("chunk-*.wav"))
        if not paths:
            raise WorkerError("Could not split audio into Sarvam-sized chunks.")
        chunks: list[tuple[Path, float, float]] = []
        offset = 0.0
        for path in paths:
            with wave.open(str(path), "rb") as audio:
                duration = audio.getnframes() / audio.getframerate()
            if duration <= 0 or duration > SARVAM_CHUNK_SECONDS + 0.1:
                raise WorkerError("Audio chunk duration is outside Sarvam REST limits.")
            chunks.append((path, offset, duration))
            offset += duration
        return chunk_dir, chunks
    except (OSError, subprocess.CalledProcessError, wave.Error) as exc:
        shutil.rmtree(chunk_dir, ignore_errors=True)
        raise WorkerError("Could not prepare audio chunks for Sarvam transcription.") from exc
    except Exception:
        shutil.rmtree(chunk_dir, ignore_errors=True)
        raise


def _sarvam_segments(payload: dict[str, Any], offset: float, duration: float) -> list[dict[str, Any]]:
    transcript_text = payload.get("transcript")
    if not isinstance(transcript_text, str):
        raise WorkerError("Sarvam returned a response without transcript text.")
    timestamps = payload.get("timestamps")
    if not isinstance(timestamps, dict):
        if transcript_text.strip():
            raise WorkerError("Sarvam did not return phrase timestamps for the transcript.")
        return []
    texts = timestamps.get("words") or []
    starts = timestamps.get("start_time_seconds") or []
    ends = timestamps.get("end_time_seconds") or []
    if not all(isinstance(values, list) for values in (texts, starts, ends)):
        raise WorkerError("Sarvam returned invalid phrase timestamp arrays.")
    if not (len(texts) == len(starts) == len(ends)):
        raise WorkerError("Sarvam returned mismatched transcript timestamp arrays.")
    if transcript_text.strip() and not texts:
        return [{
            "text": transcript_text.strip(), "start": offset, "end": offset + duration,
            "words": [],
        }]
    segments: list[dict[str, Any]] = []
    for text, start, end in zip(texts, starts, ends):
        try:
            start_sec, end_sec = float(start), float(end)
        except (TypeError, ValueError) as exc:
            raise WorkerError("Sarvam returned invalid phrase timestamps.") from exc
        if (
            not isinstance(text, str) or not text.strip()
            or not math.isfinite(start_sec) or not math.isfinite(end_sec)
            or start_sec < 0 or end_sec < start_sec or end_sec > duration + 0.1
        ):
            raise WorkerError("Sarvam returned invalid phrase timestamps.")
        segments.append({
            "text": text.strip(), "start": offset + start_sec, "end": offset + end_sec,
            "words": [],
        })
    return segments


def _transcribe_sarvam_chunk(audio_path: Path, model: str) -> dict[str, Any]:
    api_key = os.environ.get("SARVAM_API_KEY", "").strip()
    if not api_key:
        raise WorkerError("SARVAM_API_KEY is not configured for this service.")
    body, content_type = _sarvam_multipart(audio_path, model)
    request = urllib.request.Request(
        SARVAM_API_URL,
        data=body,
        headers={
            "api-subscription-key": api_key,
            "Content-Type": content_type,
            "User-Agent": "hoichoi-contextual-ad-lab/0.1",
        },
        method="POST",
    )
    try:
        with urllib.request.urlopen(request, timeout=8 * 60) as response:
            return json.loads(response.read(12 * 1024 * 1024))
    except urllib.error.HTTPError as exc:
        code = None
        try:
            detail = json.loads(exc.read(64 * 1024))
            error = detail.get("error", {}) if isinstance(detail, dict) else {}
            raw_code = error.get("code") if isinstance(error, dict) else None
            if isinstance(raw_code, str) and re.fullmatch(r"[A-Za-z0-9_-]{1,80}", raw_code):
                code = raw_code
        except (json.JSONDecodeError, OSError):
            pass
        suffix = f" ({code})" if code else ""
        raise WorkerError(f"Sarvam transcription returned HTTP {exc.code}{suffix}.") from exc
    except (urllib.error.URLError, TimeoutError) as exc:
        raise WorkerError("Could not reach Sarvam transcription, or the request timed out.") from exc
    except (json.JSONDecodeError, OSError) as exc:
        raise WorkerError("Sarvam returned an unreadable transcription response.") from exc


def _transcribe_sarvam(audio_path: Path) -> dict[str, Any]:
    model = _provider_model("sarvam")
    chunk_dir, chunks = _split_sarvam_audio(audio_path, audio_path.parent)
    try:
        with ThreadPoolExecutor(max_workers=SARVAM_PARALLEL_REQUESTS) as pool:
            payloads = list(pool.map(lambda chunk: _transcribe_sarvam_chunk(chunk[0], model), chunks))
        segments: list[dict[str, Any]] = []
        for payload, (_chunk_path, offset, duration) in zip(payloads, chunks):
            segments.extend(_sarvam_segments(payload, offset, duration))
        normalized = _validate_transcript({
            "language": "bn-IN",
            "duration": sum(duration for _, _, duration in chunks),
            "text": " ".join(segment["text"] for segment in segments),
            "segments": segments,
        }, f"sarvam/{model}")
        return normalized
    finally:
        shutil.rmtree(chunk_dir, ignore_errors=True)


def transcribe(audio_path: Path, filename: str) -> dict[str, Any]:
    provider = _asr_provider()
    if provider == "sarvam":
        return _transcribe_sarvam(audio_path)
    return _transcribe_groq(audio_path, filename)


def detect_silences(video_path: Path, duration: float) -> list[dict[str, float]]:
    """Find sustained low-audio intervals; these are evidence, not ad decisions."""
    try:
        result = subprocess.run(
            [
                "ffmpeg", "-hide_banner", "-i", str(video_path), "-vn",
                "-af", "silencedetect=noise=-32dB:d=0.45", "-f", "null", "-",
            ],
            check=False,
            capture_output=True,
            text=True,
            timeout=7 * 60,
        )
    except subprocess.TimeoutExpired as exc:
        raise WorkerError("Audio pause detection timed out.") from exc
    except OSError as exc:
        raise WorkerError("FFmpeg is unavailable for audio pause detection.") from exc
    if result.returncode != 0:
        raise WorkerError("Could not detect low-audio pauses in this video.")

    combined = f"{result.stdout}\n{result.stderr}"
    intervals: list[dict[str, float]] = []
    open_start: float | None = None
    for line in combined.splitlines():
        start_match = re.search(r"silence_start:\s*([0-9.]+)", line)
        end_match = re.search(r"silence_end:\s*([0-9.]+)", line)
        if start_match:
            open_start = float(start_match.group(1))
        if end_match and open_start is not None:
            end = min(float(end_match.group(1)), duration)
            if end > open_start:
                intervals.append({"start": open_start, "end": end, "duration": end - open_start})
            open_start = None
    if open_start is not None and duration > open_start:
        intervals.append({"start": open_start, "end": duration, "duration": duration - open_start})
    return intervals[:500]


def _cache_key(content_hash: str, brand_catalog_hash: str | None = None) -> str:
    provider = _asr_provider()
    if brand_catalog_hash is None:
        catalog_path = Path(os.environ.get("AI_BRANDS_PATH", "assets/brands.json"))
        brand_catalog_hash = _file_sha256(catalog_path) if catalog_path.is_file() else "missing-brand-catalogue"
    parts = [
        content_hash, provider, _provider_model(provider), SCENE_MODEL, SCENE_DESCRIBE_PROMPT_VERSION,
        BREAK_MODEL, BOUNDARY_PROMPT_VERSION, PIPELINE_CACHE_VERSION, SHOT_DETECTOR_VERSION, CLIP_MODEL_VERSION,
        AUDIO_MODEL_VERSION, FUSION_VERSION, EMBEDDING_MODEL, SILENCE_DETECTOR, brand_catalog_hash,
    ]
    return hashlib.sha256("|".join(parts).encode("utf-8")).hexdigest()


def _write_cache(path: Path, payload: dict[str, Any]) -> None:
    temporary = path.with_suffix(path.suffix + ".tmp")
    try:
        temporary.write_text(json.dumps(payload, ensure_ascii=False, separators=(",", ":")), encoding="utf-8")
        os.chmod(temporary, 0o600)
        temporary.replace(path)
    finally:
        temporary.unlink(missing_ok=True)


def _read_cache(path: Path, content_hash: str, cache_key: str) -> dict[str, Any] | None:
    try:
        cached = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return None
    if not isinstance(cached, dict) or cached.get("content_hash") != content_hash or cached.get("cache_key") != cache_key:
        return None
    result = cached.get("result")
    if not isinstance(result, dict):
        return None
    return {**result, "cache_hit": True}


def _file_sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


# Shared with the Go server so both sides apply the same negative-context blocks.
_CONTEXT_GROUPS: dict[str, list[str]] = json.loads(
    Path(__file__).with_name("context_taxonomy.json").read_text(encoding="utf-8"))


def _canonical_contexts(values: list[str]) -> set[str]:
    normalized = [str(value).lower().replace("_", " ") for value in values]
    tags: set[str] = set()
    for canonical, aliases in _CONTEXT_GROUPS.items():
        for value in normalized:
            if any(re.search(r"(?<![a-z0-9])" + re.escape(alias) + r"(?![a-z0-9])", value) for alias in aliases):
                tags.add(canonical)
                break
    return tags


def _validate_scenes(
    payload: dict[str, Any], duration: float, brands: list[dict[str, Any]] | None = None,
) -> list[dict[str, Any]]:
    scenes = payload.get("scenes")
    if not isinstance(scenes, list) or not scenes:
        raise WorkerError("Scene analysis did not return any scenes.")
    normalized: list[dict[str, Any]] = []
    previous_start = -1.0
    for scene in scenes:
        if not isinstance(scene, dict):
            raise WorkerError("Scene analysis returned an invalid scene entry.")
        try:
            start, end = float(scene["start"]), float(scene["end"])
            confidence = float(scene["confidence"])
        except (KeyError, TypeError, ValueError) as exc:
            raise WorkerError("Scene analysis returned invalid time or confidence values.") from exc
        if start < 0 or end <= start or end > duration + 1 or start < previous_start:
            raise WorkerError("Scene analysis returned out-of-range or unordered scene timestamps.")
        if not 0 <= confidence <= 1:
            raise WorkerError("Scene analysis returned confidence outside the 0–1 range.")
        contexts = scene.get("sensitive_contexts")
        if not isinstance(contexts, list) or any(context not in SENSITIVE_CONTEXTS for context in contexts):
            raise WorkerError("Scene analysis returned an unknown sensitive-context label.")
        for field in ("scene_id", "summary", "dialogue_state"):
            if not isinstance(scene.get(field), str) or not scene[field].strip():
                raise WorkerError(f"Scene analysis returned an empty {field}.")
        for field in ("activities", "tone", "evidence"):
            if not isinstance(scene.get(field), list) or any(not isinstance(value, str) for value in scene[field]):
                raise WorkerError(f"Scene analysis returned invalid {field}.")
        raw_matches = scene.get("brand_matches")
        expected_brands = {brand["brand_id"] for brand in (brands or [])}
        if not isinstance(raw_matches, list) or len(raw_matches) != len(expected_brands):
            raise WorkerError("Scene analysis omitted or duplicated a brand match.")
        matches_by_id: dict[str, dict[str, Any]] = {}
        for match in raw_matches:
            if not isinstance(match, dict):
                raise WorkerError("Scene analysis returned an invalid brand match.")
            brand_id = match.get("brand_id")
            score = match.get("fit_score")
            if (brand_id not in expected_brands or brand_id in matches_by_id
                    or isinstance(score, bool) or not isinstance(score, (int, float)) or not 0 <= score <= 1
                    or not isinstance(match.get("matched_contexts"), list)
                    or any(not isinstance(value, str) for value in match["matched_contexts"])
                    or not isinstance(match.get("reason"), str) or not match["reason"].strip()):
                raise WorkerError("Scene analysis returned an invalid brand fit score or explanation.")
            matches_by_id[brand_id] = {
                "brand_id": brand_id, "fit_score": float(score),
                "matched_contexts": match["matched_contexts"][:8], "reason": match["reason"].strip()[:300],
            }
        scene_terms = [scene["summary"], *scene["activities"], *scene["tone"], *scene["evidence"], *contexts]
        scene_tags = _canonical_contexts(scene_terms)
        for brand in brands or []:
            match = matches_by_id[brand["brand_id"]]
            match["display_name"] = brand["display_name"]
            match["category"] = brand["category"]
            negative_tags = _canonical_contexts(brand["negative_contexts"])
            blocked_by = sorted(scene_tags.intersection(negative_tags))
            if confidence < 0.65 or scene["dialogue_state"] == "unclear":
                blocked_by.append("uncertain_scene")
            match["blocked_contexts"] = blocked_by
            match["blocked"] = bool(blocked_by)
            match["recommended"] = not blocked_by and match["fit_score"] >= 0.55
        ordered_matches = sorted(matches_by_id.values(), key=lambda item: (-item["fit_score"], item["brand_id"]))
        normalized.append({
            "scene_id": scene["scene_id"], "start": start, "end": end,
            "summary": scene["summary"].strip(), "activities": scene["activities"],
            "tone": scene["tone"], "sensitive_contexts": contexts,
            "dialogue_state": scene["dialogue_state"], "confidence": confidence,
            "evidence": scene["evidence"],
            "brand_matches": ordered_matches,
        })
        previous_start = start
    return normalized


def _asr_label() -> str:
    provider = _asr_provider()
    return f"sarvam/{_provider_model(provider)}" if provider == "sarvam" else _provider_model(provider)


TRANSCRIPT_FIELDS = ("language", "duration", "text", "segments", "model", "timestamp_adjustments")


def _transcript_cache_path(work_dir: Path, content_hash: str) -> Path:
    label = hashlib.sha256(_asr_label().encode("utf-8")).hexdigest()[:12]
    return work_dir / f"transcript-{content_hash[:16]}-{label}.json"


def _save_transcript(work_dir: Path, content_hash: str, transcript: dict[str, Any]) -> None:
    """Save the ASR result as soon as it exists, so a later failure never costs another transcription."""
    _write_cache(_transcript_cache_path(work_dir, content_hash), {
        "content_hash": content_hash, "asr": _asr_label(),
        "transcript": {field: transcript[field] for field in TRANSCRIPT_FIELDS if field in transcript},
    })


def _reusable_transcript(work_dir: Path, content_hash: str, supplied: dict[str, Any] | None = None) -> dict[str, Any] | None:
    """Reuse a Bengali transcript for this video from the same ASR model: the one the server supplied,
    the saved transcript file, or any earlier full analysis."""
    fields = TRANSCRIPT_FIELDS
    if (isinstance(supplied, dict) and supplied.get("model") == _asr_label()
            and isinstance(supplied.get("segments"), list)):
        return _validate_transcript({**supplied}, supplied["model"])
    try:
        saved = json.loads(_transcript_cache_path(work_dir, content_hash).read_text(encoding="utf-8"))
        if saved.get("content_hash") == content_hash and saved.get("asr") == _asr_label():
            return saved["transcript"]
    except (OSError, json.JSONDecodeError, KeyError, AttributeError):
        pass
    paths = sorted(work_dir.glob("analysis-*.json"), key=lambda path: path.stat().st_mtime, reverse=True)
    for path in paths:
        try:
            cached = json.loads(path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError):
            continue
        result = cached.get("result") if isinstance(cached, dict) else None
        if (cached.get("content_hash") == content_hash and isinstance(result, dict)
                and result.get("model") == _asr_label() and isinstance(result.get("segments"), list)):
            return {field: result[field] for field in fields if field in result}
    return None


def _speech_before_after(segments: list[dict[str, Any]], time: float) -> tuple[str, str, str]:
    from text_signals import window_text

    enclosing = next((segment for segment in segments if float(segment["start"]) < time < float(segment["end"])
                      and float(segment["end"]) - float(segment["start"]) > 6), None)
    return (window_text(segments, time - 25, time)[-400:], window_text(segments, time, time + 25)[:400],
            "coarse" if enclosing else "phrase")


def _scene_inputs(edges: list[float], shots: list[dict], segments: list[dict]) -> list[dict[str, Any]]:
    from text_signals import window_text

    scenes = []
    for number in range(len(edges) - 1):
        start, end = edges[number], edges[number + 1]
        inside = [shot for shot in shots if shot["end"] > start + 0.05 and shot["start"] < end - 0.05]
        frames = [frame for shot in inside for frame in shot["keyframes"] if start <= frame["time"] <= end]
        if len(frames) > KEYFRAMES_PER_SCENE:
            step = (len(frames) - 1) / (KEYFRAMES_PER_SCENE - 1)
            frames = [frames[round(i * step)] for i in range(KEYFRAMES_PER_SCENE)]
        scenes.append({
            "scene_id": f"scene-{number + 1:03d}", "start": round(start, 3), "end": round(end, 3),
            "shot_count": len(inside), "keyframes": [frame["path"] for frame in frames],
            "text": window_text(segments, start, end, limit=700),
            "shot_boundaries": [round(shot["start"], 3) for shot in inside if start < shot["start"] < end],
        })
    return scenes


def analyze_programme(output: dict[str, Any], video_path: Path, work_dir: Path, brands: list[dict[str, Any]]) -> None:
    """Shots -> keyframes -> visual/audio/text signals -> fused shortlist -> model judgements -> scenes and
    ad-friendliness. Fills `output` in place and records any stage that could not run."""
    import audio
    import fusion
    import shots as shot_module
    import text_signals
    import visual

    duration = float(output["duration"])
    segments = output.get("segments", [])
    progress("detecting_shots", 36, "Detecting shots, fades, and dissolves.")
    try:
        segmented = shot_module.segment_shots(video_path, work_dir, duration)
    except shot_module.ShotDetectionError as exc:
        output["shot_detection_error"] = str(exc)
        output["scene_analysis_error"] = "Scene analysis needs shot detection."
        return
    try:
        boundaries = segmented["boundaries"]
        output["shot_boundaries"] = [item["time"] for item in boundaries]
        output["shot_transitions"] = boundaries
        output["shot_count"] = len(segmented["shots"])
        output["shot_detection_status"] = "complete"
        times = output["shot_boundaries"]

        progress("embedding_shots", 44, f"Embedding {len(segmented['shots'])} shots with CLIP.")
        progress("listening", 50, "Classifying the audio track with YAMNet.")
        try:
            embeddings = visual.shot_embeddings(segmented["shots"])
            sound = audio.classify(audio.load_audio(video_path))
        except (visual.VisualModelError, audio.AudioModelError) as exc:
            output["scene_analysis_error"] = str(exc)
            return
        speech_free = audio.speech_free_intervals(sound)
        output["speech_free_intervals"] = speech_free[:500]
        # ASR can invent text over music or noise; only dialogue that YAMNet actually hears informs the analysis.
        heard = [segment for segment in segments
                 if audio.speech_between(sound, float(segment["start"]), float(segment["end"])) >= SPEECH_EVIDENCE]
        output["unheard_segment_count"] = len(segments) - len(heard)
        segments = heard
        text_shift, text_error = text_signals.text_shifts(segments, times)
        output["text_signal_error"] = text_error
        scored = fusion.score_boundaries(
            boundaries, segmented["shots"], embeddings,
            {time: audio.boundary_shift(sound, time) for time in times},
            {time: audio.speech_at(sound, time) for time in times},
            output.get("silence_intervals", []), speech_free, text_shift,
        )
        shortlist = fusion.shortlist(scored, duration)
        for number, item in enumerate(shortlist, 1):
            item["candidate_id"] = f"candidate-{number:03d}"
            item["before_text"], item["after_text"], item["timing_quality"] = _speech_before_after(segments, item["time"])
        output["fusion_shortlist_count"] = len(shortlist)

        progress("judging_boundaries", 60, f"Asking the vision model about {len(shortlist)} likely scene changes.")
        try:
            judgements = judge_boundaries(
                shortlist, segmented["pool_frame"], BREAK_MODEL, duration,
                lambda done, total: progress("judging_boundaries", 60 + int(12 * done / max(total, 1)),
                                             f"Judged {done} of {total} likely scene changes."))
        except SceneAIError as exc:
            output["scene_analysis_error"] = str(exc)
            return
        accepted = [item for item in shortlist if judgements[item["candidate_id"]]["continuity"] == "new_scene"
                    or (judgements[item["candidate_id"]]["continuity"] == "uncertain"
                        and item["scene_score"] >= ACCEPT_UNCERTAIN_SCORE)]
        edges = [0.0, *[item["time"] for item in accepted], duration]
        scene_inputs = _scene_inputs(edges, segmented["shots"], segments)

        progress("describing_scenes", 74, f"Describing {len(scene_inputs)} scenes and ranking brands.")
        brand_text = json.dumps([
            {key: brand[key] for key in ("brand_id", "display_name", "category", "target_contexts", "negative_contexts")}
            for brand in brands], ensure_ascii=False)
        try:
            described = describe_scenes(
                scene_inputs, SCENE_MODEL, brand_text,
                lambda done, total: progress("describing_scenes", 74 + int(12 * done / max(total, 1)),
                                             f"Described {done} of {total} scenes."))
        except SceneAIError as exc:
            output["scene_analysis_error"] = str(exc)
            return
        try:
            scenes = _validate_scenes({"scenes": [
                {**described[scene["scene_id"]], "start": scene["start"], "end": scene["end"]} for scene in scene_inputs
            ]}, duration, brands)
        except WorkerError as exc:
            output["scene_analysis_error"] = str(exc)
            return
        for scene, source in zip(scenes, scene_inputs):
            scene["shot_boundaries"] = source["shot_boundaries"]
            scene["shot_count"] = source["shot_count"]
        output["scenes"] = scenes
        output["scene_analysis_status"] = "complete"

        transitions, candidates = [], []
        for item in shortlist:
            judgement = judgements[item["candidate_id"]]
            is_scene_change = item in accepted
            before = max((scene for scene in scenes if scene["start"] < item["time"]), key=lambda scene: scene["start"])
            after = next((scene for scene in scenes if scene["start"] >= item["time"] - 0.01), None)
            ad_score, tier, rationale = ad_friendliness(item, judgement)
            transitions.append({
                "probe_id": item["candidate_id"], "time": item["time"], "kind": judgement["change_type"],
                "continuity": judgement["continuity"], "confidence": judgement["confidence"],
                "evidence": judgement["reason"],
            })
            matches = before.get("brand_matches", [])
            signals = [item["shot_transition"] if item["shot_transition"] != "cut" else "shot_cut"]
            signals += [name for name, value in (
                ("visual_shift", item["signal_scores"].get("visual_window", 0)),
                ("audio_shift", item["signal_scores"].get("audio", 0)),
                ("low_audio_pause", item["signal_scores"].get("pause", 0)),
                ("topic_shift", item["signal_scores"].get("text", 0))) if value >= 0.5]
            candidates.append({
                "candidate_id": item["candidate_id"], "time": item["time"], "signals": signals,
                "evidence": [f"{name}: {value:.2f}" for name, value in item["signal_scores"].items()],
                "before_text": item["before_text"], "after_text": item["after_text"],
                "timing_quality": item["timing_quality"],
                "scene_context": " | ".join(filter(None, [before["summary"], (after or {}).get("summary", "")]))[:400],
                "preceding_scene_context": before["summary"][:200],
                "preceding_scene_mood": ", ".join(before.get("tone", [])[:5]),
                "preceding_sensitive_contexts": sorted(set(before.get("sensitive_contexts", []))
                                                       | set(judgement["sensitive_contexts"])),
                "brand_recommendations": [match for match in matches if match.get("recommended")][:3],
                "blocked_brand_matches": [match for match in matches if match.get("blocked")][:3],
                "transition_kind": judgement["change_type"], "transition_evidence": judgement["reason"],
                "continuity": judgement["continuity"], "scene_change": is_scene_change,
                "from_context": judgement["from_context"], "to_context": judgement["to_context"],
                "topic_shift": judgement["topic_shift"], "tension": judgement["tension"],
                "dialogue_complete": judgement["dialogue_complete"],
                "naturalness": judgement["naturalness"], "disruption_risk": judgement["disruption_risk"],
                "confidence": judgement["confidence"], "ai_reason": judgement["reason"],
                "ai_model": BREAK_MODEL, "ai_prompt_version": BOUNDARY_PROMPT_VERSION,
                "scene_score": item["scene_score"], "signal_scores": item["signal_scores"],
                "shot_transition": item["shot_transition"], "pause_seconds": item["pause_seconds"],
                "pause_source": item["pause_source"], "speech_at_cut": item["speech_at_cut"],
                "audio_change": item.get("audio") or {}, "ad_score": ad_score, "tier": tier, "rationale": rationale,
            })
        output["transitions"] = transitions
        output["break_candidates"] = candidates
        output["break_scoring_status"] = "complete"
    finally:
        shutil.rmtree(segmented["pool_dir"], ignore_errors=True)


def run(request: dict[str, Any]) -> dict[str, Any]:
    video_path = Path(str(request.get("video_path", ""))).resolve()
    work_dir = Path(str(request.get("work_dir", ""))).resolve()
    brands_path = Path(str(request.get("brands_path", "assets/brands.json"))).resolve()
    if not video_path.is_file():
        raise WorkerError("The uploaded video could not be found.")
    brands = load_brand_catalog(brands_path)
    content_hash = _file_sha256(video_path)
    claimed_hash = str(request.get("content_hash", "")).strip()
    if claimed_hash and claimed_hash != content_hash:
        raise WorkerError("The uploaded video hash did not match the analysis job.")
    brand_catalog_hash = _file_sha256(brands_path)
    cache_key = _cache_key(content_hash, brand_catalog_hash=brand_catalog_hash)
    cache_path = work_dir / f"analysis-{cache_key}.json"
    from_phase = str(request.get("from_phase", "")).strip()
    if from_phase not in ("", "transcription", "scene_analysis"):
        raise WorkerError("Unknown analysis phase to retry from.")
    cached = _read_cache(cache_path, content_hash, cache_key) if not from_phase else None
    if cached is not None:
        return cached

    work_dir.mkdir(parents=True, exist_ok=True)
    transcript = None
    if from_phase != "transcription":
        transcript = _reusable_transcript(work_dir, content_hash, request.get("transcript"))
    if transcript is None and from_phase == "scene_analysis":
        raise WorkerError("No saved transcript from the current ASR model matches this video; retry transcription.")
    evidence_cache_hit = transcript is not None
    if transcript is None:
        progress("transcribing_bengali_speech", 28, "Preparing audio and transcribing Bengali speech.")
        audio_path = extract_audio(video_path, work_dir)
        try:
            transcript = transcribe(audio_path, video_path.name)
        finally:
            audio_path.unlink(missing_ok=True)
        _save_transcript(work_dir, content_hash, transcript)
    else:
        progress("transcribing_bengali_speech", 32, "Reusing the saved Bengali transcript for this video.")

    output: dict[str, Any] = {
        **transcript,
        "scenes": [], "transitions": [], "silence_intervals": [], "speech_free_intervals": [],
        "pause_detection_status": "unavailable", "pause_detection_error": "",
        "shot_boundaries": [], "shot_transitions": [], "shot_count": 0,
        "shot_detection_status": "unavailable", "shot_detection_error": "",
        "scene_analysis_status": "unavailable", "scene_analysis_error": "", "text_signal_error": "",
        "scene_model": SCENE_MODEL, "scene_prompt_version": SCENE_DESCRIBE_PROMPT_VERSION,
        "break_model": BREAK_MODEL, "break_prompt_version": BOUNDARY_PROMPT_VERSION,
        "break_candidates": [], "break_scoring_status": "unavailable", "break_scoring_error": "",
        "brand_catalog_version": brand_catalog_hash[:12],
        "pipeline": {
            "version": PIPELINE_CACHE_VERSION, "shot_detector": SHOT_DETECTOR_VERSION,
            "visual_model": CLIP_MODEL_VERSION, "audio_model": AUDIO_MODEL_VERSION, "fusion": FUSION_VERSION,
            "text_embedding_model": EMBEDDING_MODEL, "boundary_model": BREAK_MODEL, "scene_model": SCENE_MODEL,
        },
        "content_hash": content_hash, "cache_key": cache_key, "cache_hit": False,
        "evidence_cache_hit": evidence_cache_hit,
    }
    progress("detecting_pauses", 34, "Finding low-audio pauses.")
    try:
        output["silence_intervals"] = detect_silences(video_path, transcript["duration"])
        output["pause_detection_status"] = "complete"
    except WorkerError as exc:
        output["pause_detection_error"] = str(exc)

    analyze_programme(output, video_path, work_dir, brands)
    if output["break_scoring_status"] != "complete" and not output["break_scoring_error"]:
        output["break_scoring_error"] = output["scene_analysis_error"] or "Scene evidence is unavailable."
    if output["scene_analysis_status"] == "complete" and output["break_scoring_status"] == "complete":
        _write_cache(cache_path, {"content_hash": content_hash, "cache_key": cache_key, "result": output})
    progress("finishing", 90, "Applying the break policy.")
    return output


def main() -> int:
    try:
        request = json.load(sys.stdin)
        result = run(request)
        json.dump(result, sys.stdout, ensure_ascii=False, separators=(",", ":"))
        sys.stdout.write("\n")
        return 0
    except WorkerError as exc:
        print(str(exc), file=sys.stderr)
        return 2
    except Exception:
        print("AI worker failed unexpectedly; inspect the service logs.", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
