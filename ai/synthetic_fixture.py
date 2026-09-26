#!/usr/bin/env python3
"""Build a synthetic programme with known shot and scene boundaries for tests and evaluation.

Scene A (0-20 s) alternates two crops of one image every 4 s, like shot/reverse-shot dialogue:
four shot cuts, no scene change. A hard cut starts scene B, which zooms slowly (camera motion,
no cut). A fade through black starts scene C and a cross-dissolve starts scene D. Each scene has
its own audio bed, and the audio drops out briefly around every true scene change.
"""

from __future__ import annotations

import json
import subprocess
import sys
import tempfile
from pathlib import Path

FPS = 25
SIZE = "640x360"
FADE = 1.0
DISSOLVE = 1.0
PAUSE = 0.6  # seconds of silence on each side of a true scene change


def _run(args: list[str]) -> None:
    subprocess.run(["ffmpeg", "-hide_banner", "-loglevel", "error", "-y", *args], check=True)


def build(output: Path) -> dict:
    with tempfile.TemporaryDirectory(prefix="scenesense-fixture-") as temp:
        root = Path(temp)
        still = root / "a.png"
        _run(["-f", "lavfi", "-i", "mandelbrot=size=1280x720", "-frames:v", "1", str(still)])
        source_a = f"movie={still},loop=loop=-1:size=1,setpts=N/{FPS}/TB"
        clips = {
            # Two framings of the same still image: shot / reverse shot inside one scene.
            "a1": (f"{source_a},crop=640:360:0:0", 4),
            "a2": (f"{source_a},crop=640:360:520:300", 4),
            "b": (f"testsrc2=size=1280x720:rate={FPS},zoompan=z='1+0.004*on':d=1:s={SIZE}:fps={FPS}", 20),
            "c": (f"smptehdbars=size={SIZE}:rate={FPS}", 15 + FADE),
            "d": (f"cellauto=size={SIZE}:rate={FPS}:rule=110:scroll=0:full=1", 15 + DISSOLVE),
        }
        for name, (source, seconds) in clips.items():
            _run(["-f", "lavfi", "-i", f"{source}", "-t", str(seconds), "-r", str(FPS), "-s", SIZE,
                  "-pix_fmt", "yuv420p", "-c:v", "libx264", "-preset", "ultrafast", str(root / f"{name}.mp4")])
        order = ["a1", "a2", "a1", "a2", "a1", "b"]
        (root / "hard.txt").write_text("".join(f"file '{root / name}.mp4'\n" for name in order))
        _run(["-f", "concat", "-safe", "0", "-i", str(root / "hard.txt"), "-c", "copy", str(root / "ab.mp4")])
        # 40 s of A+B, then fade through black to C (boundary at the fade midpoint), then dissolve to D.
        fade_offset = 40 - FADE / 2
        dissolve_offset = fade_offset + 15 + FADE / 2 - DISSOLVE / 2
        _run(["-i", str(root / "ab.mp4"), "-i", str(root / "c.mp4"), "-i", str(root / "d.mp4"), "-filter_complex",
              f"[0:v]settb=AVTB,fps={FPS}[v0];[1:v]settb=AVTB,fps={FPS}[v1];[2:v]settb=AVTB,fps={FPS}[v2];"
              f"[v0][v1]xfade=transition=fadeblack:duration={FADE}:offset={fade_offset}[v01];"
              f"[v01][v2]xfade=transition=dissolve:duration={DISSOLVE}:offset={dissolve_offset}[v]",
              "-map", "[v]", "-pix_fmt", "yuv420p", "-c:v", "libx264", "-preset", "ultrafast", str(root / "video.mp4")])
        duration = dissolve_offset + 15 + DISSOLVE
        scene_changes = [20.0, 40.0, round(dissolve_offset + DISSOLVE / 2, 3)]
        beds = [
            "sine=frequency=440:sample_rate=16000,volume=0.25",  # A: steady tone
            "anoisesrc=color=pink:sample_rate=16000:amplitude=0.25",  # B: broadband noise
            "sine=frequency=180:sample_rate=16000,volume=0.3",  # C: low hum
            "anoisesrc=color=brown:sample_rate=16000:amplitude=0.4",  # D: rumble
        ]
        edges = [0.0, *scene_changes, duration]
        chains = []
        for index, bed in enumerate(beds):
            length = edges[index + 1] - edges[index]
            quiet = []
            if index > 0:
                quiet.append(f"lt(t,{PAUSE})")
            if index < len(beds) - 1:
                quiet.append(f"gte(t,{length - PAUSE:.3f})")
            chain = f"{bed},atrim=0:{length:.3f}"
            if quiet:
                chain += f",volume=enable='{'+'.join(quiet)}':volume=0"
            chains.append(f"{chain}[a{index}]")
        graph = ";".join(chains) + ";" + "".join(f"[a{index}]" for index in range(len(beds)))
        graph += f"concat=n={len(beds)}:v=0:a=1[a]"
        _run(["-i", str(root / "video.mp4"), "-filter_complex", graph, "-map", "0:v", "-map", "[a]",
              "-c:v", "copy", "-c:a", "aac", "-shortest", str(output)])
    truth = {
        "duration": round(duration, 3),
        "shot_boundaries": [4.0, 8.0, 12.0, 16.0, 20.0, 40.0, scene_changes[2]],
        "shot_kinds": {"20.0": "cut", "40.0": "fade", str(scene_changes[2]): "dissolve"},
        "scene_changes": scene_changes,
        "same_scene_cuts": [4.0, 8.0, 12.0, 16.0],
    }
    output.with_suffix(".truth.json").write_text(json.dumps(truth, indent=2))
    return truth


if __name__ == "__main__":
    print(json.dumps(build(Path(sys.argv[1] if len(sys.argv) > 1 else "synthetic-fixture.mp4")), indent=2))
