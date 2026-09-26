"""Pipeline component versions. Dependency-free so the cache key can be computed without the ML stack;
the Go server mirrors these values in currentAnalysisCacheKey."""

import os

PIPELINE_CACHE_VERSION = "scene-fusion-pipeline-v1"
SHOT_DETECTOR_VERSION = "pyscenedetect-adaptive-threshold+twin-dissolve-v1"
CLIP_MODEL_VERSION = "clip-vit-b32-onnx-int8-d15189d"
AUDIO_MODEL_VERSION = "yamnet-onnx-qaihub-0.63.0"
FUSION_VERSION = "scene-fusion-v1"
EMBEDDING_MODEL = os.environ.get("OPENAI_EMBEDDING_MODEL", "text-embedding-3-small").strip() or "text-embedding-3-small"
