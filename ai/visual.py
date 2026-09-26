"""CLIP ViT-B/32 image embeddings for shot keyframes (ONNX Runtime, CPU)."""

from __future__ import annotations

import os
from functools import lru_cache
from pathlib import Path

import numpy as np
from PIL import Image

_MEAN = np.array([0.48145466, 0.4578275, 0.40821073], dtype=np.float32)
_STD = np.array([0.26862954, 0.26130258, 0.27577711], dtype=np.float32)
_SIZE = 224


class VisualModelError(Exception):
    """A safe error to surface when visual embeddings are unavailable."""


def models_dir() -> Path:
    return Path(os.environ.get("MODELS_DIR", Path(__file__).resolve().parent.parent / "models"))


@lru_cache(maxsize=1)
def _session():
    import onnxruntime as ort

    path = models_dir() / "clip_vision_quantized.onnx"
    if not path.is_file():
        raise VisualModelError("The CLIP model is missing; run `python ai/fetch_models.py`.")
    options = ort.SessionOptions()
    options.log_severity_level = 3
    return ort.InferenceSession(str(path), options, providers=["CPUExecutionProvider"])


def _preprocess(path: str) -> np.ndarray:
    with Image.open(path) as image:
        image = image.convert("RGB")
        scale = _SIZE / min(image.size)
        image = image.resize((max(_SIZE, round(image.width * scale)), max(_SIZE, round(image.height * scale))),
                             Image.Resampling.BICUBIC)
        left, top = (image.width - _SIZE) // 2, (image.height - _SIZE) // 2
        image = image.crop((left, top, left + _SIZE, top + _SIZE))
        pixels = (np.asarray(image, dtype=np.float32) / 255 - _MEAN) / _STD
    return pixels.transpose(2, 0, 1)


def embed_images(paths: list[str], batch_size: int = 16) -> np.ndarray:
    """Return L2-normalised 512-d embeddings, one row per image path."""
    if not paths:
        return np.zeros((0, 512), dtype=np.float32)
    session = _session()
    rows = []
    for offset in range(0, len(paths), batch_size):
        batch = np.stack([_preprocess(path) for path in paths[offset:offset + batch_size]])
        rows.append(session.run(["image_embeds"], {"pixel_values": batch})[0])
    embeddings = np.concatenate(rows).astype(np.float32)
    return embeddings / np.maximum(np.linalg.norm(embeddings, axis=1, keepdims=True), 1e-8)


def shot_embeddings(shots: list[dict]) -> np.ndarray:
    """One embedding per shot: the normalised mean of its keyframe embeddings."""
    paths = [frame["path"] for shot in shots for frame in shot["keyframes"]]
    frames = embed_images(paths)
    rows, offset = [], 0
    for shot in shots:
        count = len(shot["keyframes"])
        mean = frames[offset:offset + count].mean(axis=0)
        rows.append(mean / max(float(np.linalg.norm(mean)), 1e-8))
        offset += count
    return np.stack(rows) if rows else np.zeros((0, 512), dtype=np.float32)
