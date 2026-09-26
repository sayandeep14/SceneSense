#!/usr/bin/env python3
"""Manually smoke-test Groq Whisper transcription without exposing the API key."""

from __future__ import annotations

import argparse
import json
import math
import os
import secrets
import struct
import sys
import urllib.error
import urllib.request
import wave
from pathlib import Path


API_URL = "https://api.groq.com/openai/v1/audio/transcriptions"
MODEL = "whisper-large-v3-turbo"
SAMPLE_RATE = 16_000


def synthetic_wav() -> bytes:
    """Create a short, non-speech WAV so the default test sends no user media."""
    samples = bytearray()
    for index in range(SAMPLE_RATE * 2):
        amplitude = int(5_000 * math.sin(2 * math.pi * 440 * index / SAMPLE_RATE))
        samples.extend(struct.pack("<h", amplitude))

    import io

    output = io.BytesIO()
    with wave.open(output, "wb") as wav_file:
        wav_file.setnchannels(1)
        wav_file.setsampwidth(2)
        wav_file.setframerate(SAMPLE_RATE)
        wav_file.writeframes(samples)
    return output.getvalue()


def multipart_body(audio: bytes, filename: str, content_type: str) -> tuple[bytes, str]:
    boundary = "----GroqSmokeTest" + secrets.token_hex(16)
    fields = [
        ("model", MODEL),
        ("language", "bn"),
        ("response_format", "verbose_json"),
        ("timestamp_granularities[]", "segment"),
    ]
    parts: list[bytes] = []
    for name, value in fields:
        parts.append(
            f"--{boundary}\r\n"
            f'Content-Disposition: form-data; name="{name}"\r\n\r\n'
            f"{value}\r\n".encode("utf-8")
        )
    safe_filename = Path(filename).name.replace('"', "_").replace("\r", "_").replace("\n", "_")
    parts.append(
        f"--{boundary}\r\n"
        f'Content-Disposition: form-data; name="file"; filename="{safe_filename}"\r\n'
        f"Content-Type: {content_type}\r\n\r\n".encode("utf-8")
    )
    parts.append(audio)
    parts.append(f"\r\n--{boundary}--\r\n".encode("ascii"))
    return b"".join(parts), f"multipart/form-data; boundary={boundary}"


def print_headers(headers: object) -> None:
    if headers is None:
        return
    wanted = {"content-type", "x-request-id", "request-id", "x-groq-region"}
    for name, value in headers.items():  # type: ignore[attr-defined]
        lower_name = name.lower()
        if lower_name in wanted or lower_name.startswith("x-groq-"):
            print(f"{name}: {value}")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--audio",
        type=Path,
        help="optional local audio file (otherwise use a generated 2-second synthetic WAV)",
    )
    args = parser.parse_args()

    api_key = os.environ.get("GROQ_API_KEY", "").strip()
    if not api_key:
        print("GROQ_API_KEY is not set. Load it from your ignored .env first; do not paste it into this script.", file=sys.stderr)
        return 2

    try:
        if args.audio:
            audio = args.audio.read_bytes()
            filename = args.audio.name
            suffix = args.audio.suffix.lower()
            content_type = {
                ".wav": "audio/wav",
                ".mp3": "audio/mpeg",
                ".m4a": "audio/mp4",
                ".mp4": "audio/mp4",
                ".flac": "audio/flac",
                ".ogg": "audio/ogg",
                ".webm": "audio/webm",
            }.get(suffix, "application/octet-stream")
            print(f"Audio: {args.audio} ({len(audio)} bytes)")
        else:
            audio = synthetic_wav()
            filename = "synthetic-tone.wav"
            content_type = "audio/wav"
            print(f"Audio: generated synthetic 2-second WAV ({len(audio)} bytes)")

        if not audio:
            print("Audio file is empty.", file=sys.stderr)
            return 2
        if len(audio) > 25 * 1024 * 1024:
            print("Audio exceeds the 25 MiB test limit; use a shorter/smaller clip.", file=sys.stderr)
            return 2

        body, content_type_header = multipart_body(audio, filename, content_type)
        request = urllib.request.Request(
            API_URL,
            data=body,
            headers={
                "Authorization": f"Bearer {api_key}",
                "Content-Type": content_type_header,
                # urllib's default Python-urllib UA can be rejected by edge bot checks.
                "User-Agent": "hoichoi-contextual-ad-lab/0.1",
            },
            method="POST",
        )
        print(f"POST {API_URL}")
        print(f"Model: {MODEL}; language: bn; response_format: verbose_json")

        try:
            response = urllib.request.urlopen(request, timeout=120)
        except urllib.error.HTTPError as error:
            print(f"HTTP status: {error.code}")
            print_headers(error.headers)
            raw_error = error.read(8 * 1024).decode("utf-8", errors="replace")
            # Avoid echoing the credential if an unexpected upstream response repeats it.
            safe_error = raw_error.replace(api_key, "[REDACTED API KEY]")
            print("Provider error body:")
            print(safe_error or "<empty>")
            return 1

        with response:
            print(f"HTTP status: {response.status}")
            print_headers(response.headers)
            payload = json.loads(response.read(8 * 1024 * 1024))
        segments = payload.get("segments") or []
        print("Transcription request succeeded.")
        print(f"Returned language: {payload.get('language', '<not provided>')}")
        print(f"Segment count: {len(segments)}")
        print(f"Transcript characters: {len(payload.get('text', ''))}")
        print("Transcript text omitted.")
        return 0
    except (OSError, TimeoutError, urllib.error.URLError) as error:
        print(f"Request/setup error: {type(error).__name__}: {error}", file=sys.stderr)
        return 2
    except (json.JSONDecodeError, ValueError) as error:
        print(f"Could not parse provider response: {type(error).__name__}: {error}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
