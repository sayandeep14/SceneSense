#!/usr/bin/env python3
"""Download the pinned local models used by the worker and verify their SHA-256 digests."""

from __future__ import annotations

import hashlib
import io
import os
import sys
import urllib.request
import zipfile
from pathlib import Path

MODELS = {
    # OpenAI CLIP ViT-B/32 image encoder, ONNX export (Xenova/clip-vit-base-patch32), dynamic int8.
    "clip_vision_quantized.onnx": {
        "url": "https://huggingface.co/Xenova/clip-vit-base-patch32/resolve/"
               "d15189d7028b43f1d3e65039190477f6af591c2a/onnx/vision_model_quantized.onnx",
        "sha256": "583fd1110a514667812fee7d684952aaf82a99b959760c8d7dca7e0ab9839299",
    },
    # Google YAMNet AudioSet classifier, ONNX export by Qualcomm AI Hub (MIT), 521 classes.
    "yamnet": {
        "url": "https://qaihub-public-assets.s3.us-west-2.amazonaws.com/qai-hub-models/models/yamnet/"
               "releases/v0.63.0/yamnet-onnx-float.zip",
        "sha256": "a48856df4a7354895273b655307c169a5c3cd233283663fb776e39a5bacaac18",
        "extract": ("yamnet.onnx", "yamnet.data", "labels.txt"),
    },
}


def _download(url: str, expected: str) -> bytes:
    request = urllib.request.Request(url, headers={"User-Agent": "scenesense-model-fetch/1"})
    with urllib.request.urlopen(request, timeout=300) as response:
        data = response.read()
    digest = hashlib.sha256(data).hexdigest()
    if digest != expected:
        raise SystemExit(f"checksum mismatch for {url}: got {digest}")
    return data


def main() -> int:
    root = Path(sys.argv[1] if len(sys.argv) > 1 else os.environ.get("MODELS_DIR", "models"))
    root.mkdir(parents=True, exist_ok=True)
    clip = root / "clip_vision_quantized.onnx"
    if not clip.exists():
        spec = MODELS["clip_vision_quantized.onnx"]
        clip.write_bytes(_download(spec["url"], spec["sha256"]))
        print(f"fetched {clip}")
    yamnet_dir = root / "yamnet"
    spec = MODELS["yamnet"]
    if not all((yamnet_dir / name).exists() for name in spec["extract"]):
        archive = zipfile.ZipFile(io.BytesIO(_download(spec["url"], spec["sha256"])))
        yamnet_dir.mkdir(exist_ok=True)
        for member in archive.namelist():
            name = Path(member).name
            if name in spec["extract"]:
                (yamnet_dir / name).write_bytes(archive.read(member))
        print(f"fetched {yamnet_dir}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
