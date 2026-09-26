"""Transcript continuity: embed the dialogue before and after each boundary and compare."""

from __future__ import annotations

import hashlib
import json
import os
import urllib.error
import urllib.request
from typing import Any

import numpy as np

from versions import EMBEDDING_MODEL

TEXT_WINDOW_SECONDS = 40.0


def window_text(segments: list[dict[str, Any]], start: float, end: float, limit: int = 800) -> str:
    text = " ".join(str(segment.get("text", "")).strip() for segment in segments
                    if float(segment["end"]) > start and float(segment["start"]) < end)
    return text[:limit].strip()


def _embed(texts: list[str], api_key: str) -> list[list[float]]:
    request = urllib.request.Request(
        "https://api.openai.com/v1/embeddings",
        data=json.dumps({"model": EMBEDDING_MODEL, "input": texts}, ensure_ascii=False).encode("utf-8"),
        headers={"Authorization": f"Bearer {api_key}", "Content-Type": "application/json",
                 "User-Agent": "hoichoi-contextual-ad-lab/0.2"}, method="POST",
    )
    with urllib.request.urlopen(request, timeout=120) as response:
        payload = json.loads(response.read(32 * 1024 * 1024))
    return [item["embedding"] for item in sorted(payload["data"], key=lambda item: item["index"])]


def text_shifts(segments: list[dict[str, Any]], times: list[float]) -> tuple[dict[float, float], str]:
    """Return {time: 1 - cosine(before, after)} for boundaries with dialogue on both sides, and an error."""
    api_key = os.environ.get("OPENAI_API_KEY", "").strip()
    if not api_key or not segments or not times:
        return {}, "" if api_key else "OPENAI_API_KEY is not configured for transcript embeddings."
    pairs: dict[float, tuple[str, str]] = {}
    for time in times:
        before = window_text(segments, time - TEXT_WINDOW_SECONDS, time)
        after = window_text(segments, time, time + TEXT_WINDOW_SECONDS)
        if len(before) >= 12 and len(after) >= 12:
            pairs[time] = (before, after)
    unique = sorted({text for pair in pairs.values() for text in pair})
    if not unique:
        return {}, ""
    vectors: dict[str, np.ndarray] = {}
    try:
        for offset in range(0, len(unique), 256):
            batch = unique[offset:offset + 256]
            for text, vector in zip(batch, _embed(batch, api_key)):
                array = np.asarray(vector, dtype=np.float32)
                vectors[hashlib.sha256(text.encode()).hexdigest()] = array / max(float(np.linalg.norm(array)), 1e-8)
    except (urllib.error.URLError, TimeoutError, KeyError, ValueError, json.JSONDecodeError) as exc:
        return {}, f"Transcript embeddings were unavailable ({type(exc).__name__}); scene fusion used other signals."
    shifts = {}
    for time, (before, after) in pairs.items():
        a = vectors[hashlib.sha256(before.encode()).hexdigest()]
        b = vectors[hashlib.sha256(after.encode()).hexdigest()]
        shifts[time] = round(1 - float(a @ b), 4)
    return shifts, ""
