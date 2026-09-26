import io
import json
import tempfile
import urllib.error
import unittest
from pathlib import Path
from unittest.mock import patch

import worker


class WorkerTests(unittest.TestCase):
    def test_loads_hackathon_brand_catalogue(self):
        catalog_path = Path(__file__).resolve().parent.parent / "assets" / "brands.json"
        brands = worker.load_brand_catalog(catalog_path)
        self.assertEqual(len(brands), 8)
        self.assertEqual(sum(len(brand["creatives"]) for brand in brands), 21)
        self.assertTrue(all(brand["target_contexts"] and brand["negative_contexts"] for brand in brands))

    def test_rejects_duplicate_brand_ids(self):
        brand = {
            "brand_id": "same", "display_name": "Sample", "category": "sample",
            "target_contexts": ["context"], "negative_contexts": ["grief"],
            "creatives": [{"id": "spot", "duration_sec": 15, "language": "bn", "url": "ads/spot.mp4"}],
        }
        with tempfile.TemporaryDirectory() as directory:
            catalog_path = Path(directory) / "brands.json"
            catalog_path.write_text(json.dumps([brand, brand]), encoding="utf-8")
            with self.assertRaisesRegex(worker.WorkerError, "unique"):
                worker.load_brand_catalog(catalog_path)

    def test_normalizes_segment_and_word_timestamps(self):
        result = worker._validate_transcript({
            "language": "bengali",
            "duration": 4.0,
            "text": " নমস্কার ",
            "segments": [{
                "text": " নমস্কার ", "start": 0.2, "end": 1.4,
                "words": [{"word": " নমস্কার", "start": 0.2, "end": 1.1}],
            }],
        })
        self.assertEqual(result["language"], "bengali")
        self.assertEqual(result["segments"][0]["text"], "নমস্কার")
        self.assertEqual(result["segments"][0]["words"][0]["end"], 1.1)

    def test_rejects_non_monotonic_timestamps(self):
        with self.assertRaisesRegex(worker.WorkerError, "invalid timestamps"):
            worker._validate_transcript({
                "duration": 3,
                "segments": [
                    {"text": "one", "start": 1, "end": 2},
                    {"text": "two", "start": 1.5, "end": 2.5},
                ],
            })

    def test_clips_and_drops_transcript_timestamps_past_duration(self):
        result = worker._validate_transcript({
            "duration": 10,
            "text": "kept tail plus hallucinated tail",
            "segments": [
                {"text": "kept tail", "start": 9, "end": 12.5,
                 "words": [{"word": "tail", "start": 9.2, "end": 12.5}]},
                {"text": "hallucinated tail", "start": 10.1, "end": 11},
            ],
        })
        self.assertEqual(result["segments"][0]["end"], 10)
        self.assertEqual(result["segments"][0]["words"][0]["end"], 10)
        self.assertEqual(len(result["segments"]), 1)
        self.assertEqual(result["timestamp_adjustments"], 3)
        self.assertEqual(result["text"], "kept tail")

    def test_word_only_response_becomes_seekable_segments(self):
        result = worker._validate_transcript({
            "duration": 2,
            "words": [
                {"word": "Hello", "start": 0.1, "end": 0.5},
                {"word": "there", "start": 0.6, "end": 1.0},
            ],
        })
        self.assertEqual(len(result["segments"]), 2)
        self.assertEqual(result["segments"][1]["text"], "there")

    def test_multipart_contains_model_and_timestamps(self):
        with tempfile.TemporaryDirectory() as directory:
            audio_path = Path(directory) / "audio.mp3"
            audio_path.write_bytes(b"synthetic-audio")
            body, content_type = worker._multipart(audio_path, "episode.mp4")
        self.assertIn(worker.MODEL.encode(), body)
        self.assertIn(b"\r\nbn\r\n", body)
        self.assertEqual(body.count(b'timestamp_granularities[]'), 2)
        self.assertIn(b"episode.mp4.mp3", body)
        self.assertIn("boundary=----SceneSenseBoundary", content_type)

    def test_sarvam_multipart_uses_bengali_v4_and_timestamped_wav(self):
        with tempfile.TemporaryDirectory() as directory:
            audio_path = Path(directory) / "audio.wav"
            audio_path.write_bytes(b"synthetic-wav")
            body, content_type = worker._sarvam_multipart(audio_path, "saaras:v4")
        for value in (b"saaras:v4", b"bn-IN", b"transcribe", b"with_timestamps", b"true"):
            self.assertIn(value, body)
        self.assertIn(b"filename=\"audio.wav\"", body)
        self.assertIn(b"audio/wav", body)
        self.assertIn(b"synthetic-wav", body)
        self.assertIn("multipart/form-data; boundary=", content_type)

    def test_sarvam_phrase_timestamps_are_seekable_and_offset_per_chunk(self):
        segments = worker._sarvam_segments({
            "transcript": "আমি ভালো আছি।",
            "timestamps": {
                "words": ["আমি ভালো আছি।"],
                "start_time_seconds": [1.2],
                "end_time_seconds": [2.8],
            },
        }, offset=25.0, duration=25.0)
        self.assertEqual(segments, [{
            "text": "আমি ভালো আছি।", "start": 26.2, "end": 27.8, "words": [],
        }])

    def test_sarvam_rejects_mismatched_timestamp_arrays(self):
        with self.assertRaisesRegex(worker.WorkerError, "mismatched"):
            worker._sarvam_segments({
                "transcript": "আমি ভালো আছি।",
                "timestamps": {"words": ["আমি"], "start_time_seconds": [], "end_time_seconds": [1]},
            }, offset=0, duration=25)

    def test_sarvam_preserves_text_when_provider_has_no_phrase_times(self):
        segments = worker._sarvam_segments({
            "transcript": "কথা আছে।", "timestamps": {
                "words": [], "start_time_seconds": [], "end_time_seconds": [],
            },
        }, offset=25.0, duration=10.0)
        self.assertEqual(segments, [{
            "text": "কথা আছে।", "start": 25.0, "end": 35.0, "words": [],
        }])

    def test_sarvam_provider_and_model_are_part_of_cache_key(self):
        digest = "a" * 64
        groq_key = worker._cache_key(digest)
        with patch.dict("os.environ", {"ASR_PROVIDER": "sarvam"}):
            sarvam_key = worker._cache_key(digest)
            self.assertNotEqual(sarvam_key, groq_key)
            with patch.dict("os.environ", {"SARVAM_ASR_MODEL": "saaras:v3"}):
                self.assertNotEqual(worker._cache_key(digest), sarvam_key)

    def test_sarvam_chunk_request_uses_subscription_key_and_normalizes_response(self):
        class FakeResponse:
            def __enter__(self):
                return self

            def __exit__(self, *_args):
                return False

            def read(self, _limit):
                return json.dumps({
                    "transcript": "শুভ সকাল।", "language_code": "bn-IN",
                    "timestamps": {
                        "words": ["শুভ সকাল।"], "start_time_seconds": [0.3],
                        "end_time_seconds": [1.4],
                    },
                }).encode()

        with tempfile.TemporaryDirectory() as directory:
            audio_path = Path(directory) / "chunk.wav"
            audio_path.write_bytes(b"synthetic-wav")
            with patch.dict("os.environ", {"SARVAM_API_KEY": "test-sarvam-key"}):
                with patch("worker.urllib.request.urlopen", return_value=FakeResponse()) as request:
                    payload = worker._transcribe_sarvam_chunk(audio_path, "saaras:v4")
        self.assertEqual(payload["language_code"], "bn-IN")
        self.assertEqual(request.call_args.args[0].full_url, worker.SARVAM_API_URL)
        self.assertEqual(request.call_args.args[0].get_header("Api-subscription-key"), "test-sarvam-key")
        self.assertIn(b"saaras:v4", request.call_args.args[0].data)

    def test_extracts_lossless_wav_when_sarvam_is_selected(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            video = root / "episode.mp4"
            video.write_bytes(b"fixture")

            def write_audio(command, **_kwargs):
                Path(command[-1]).write_bytes(b"wav-audio")

            with patch.dict("os.environ", {"ASR_PROVIDER": "sarvam"}):
                with patch("worker.subprocess.run", side_effect=write_audio) as run_process:
                    audio = worker.extract_audio(video, root)
            self.assertEqual(audio.suffix, ".wav")
            self.assertIn("pcm_s16le", run_process.call_args.args[0])
            self.assertEqual(audio.read_bytes(), b"wav-audio")
            audio.unlink()

    def test_extracts_compact_audio_and_removes_oversized_output(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            video = root / "episode.mp4"
            video.write_bytes(b"fixture")

            def write_compact_audio(command, **_kwargs):
                Path(command[-1]).write_bytes(b"small-audio")

            with patch("worker.subprocess.run", side_effect=write_compact_audio) as run_process:
                audio = worker.extract_audio(video, root)
                self.assertEqual(audio.read_bytes(), b"small-audio")
                self.assertIn("16000", run_process.call_args.args[0])
                audio.unlink()

            def write_oversized_audio(command, **_kwargs):
                with open(command[-1], "wb") as output:
                    output.truncate(worker.MAX_AUDIO_BYTES + 1)

            with patch("worker.subprocess.run", side_effect=write_oversized_audio):
                with self.assertRaisesRegex(worker.WorkerError, "upload limit is 25 MB"):
                    worker.extract_audio(video, root)
            self.assertEqual(list(root.glob("transcription-*.mp3")), [])

    def test_detects_closed_and_trailing_low_audio_intervals(self):
        result = type("FFmpegResult", (), {
            "returncode": 0,
            "stdout": "",
            "stderr": "[silencedetect] silence_start: 1.000\n[silencedetect] silence_end: 2.250 | silence_duration: 1.250\n[silencedetect] silence_start: 8.000",
        })()
        with patch("worker.subprocess.run", return_value=result):
            pauses = worker.detect_silences(Path("episode.mp4"), 10.0)
        self.assertEqual(pauses, [
            {"start": 1.0, "end": 2.25, "duration": 1.25},
            {"start": 8.0, "end": 10.0, "duration": 2.0},
        ])

    def test_detects_shot_cut_timestamps_and_deduplicates_nearby_hits(self):
        result = type("FFmpegResult", (), {
            "returncode": 0, "stdout": "",
            "stderr": "[Parsed_showinfo] n:0 pts_time:1.200\n[Parsed_showinfo] n:1 pts_time:1.400\n[Parsed_showinfo] n:2 pts_time:2.000\n[Parsed_showinfo] n:3 pts_time:10.000",
        })()
        with patch("worker.subprocess.run", return_value=result) as call:
            cuts = worker.detect_shot_boundaries(Path("episode.mp4"), 10.0)
        self.assertEqual(cuts, [1.2, 2.0])
        self.assertIn("gt(scene,0.3)", call.call_args.args[0][call.call_args.args[0].index("-vf") + 1])

    def test_prompt_cut_sampling_preserves_whole_programme_coverage(self):
        cuts = [float(i) for i in range(1000)]
        sampled = worker._prompt_shot_boundaries(cuts)
        self.assertEqual(len(sampled), worker.MAX_SHOT_BOUNDARIES_IN_PROMPT)
        self.assertEqual(sampled[0], cuts[0])
        self.assertEqual(sampled[-1], cuts[-1])
        self.assertTrue(all(left < right for left, right in zip(sampled, sampled[1:])))

    def test_samples_bounded_low_resolution_frames_and_cleans_temporary_files(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)

            def create_frames(command, **_kwargs):
                pattern = Path(command[-1])
                pattern.parent.mkdir(parents=True, exist_ok=True)
                (pattern.parent / "frame-001.jpg").write_bytes(b"jpeg-a")
                (pattern.parent / "frame-002.jpg").write_bytes(b"jpeg-b")

            with patch("worker.subprocess.run", side_effect=create_frames) as ffmpeg:
                frames = worker.sample_frames(root / "episode.mp4", root, 120.0)
            self.assertEqual(frames[0]["time"], 0.0)
            self.assertAlmostEqual(frames[1]["time"], 8.0, places=1)
            self.assertTrue(all(frame["data_url"].startswith("data:image/jpeg;base64,") for frame in frames))
            self.assertIn("640:-2", ffmpeg.call_args.args[0][ffmpeg.call_args.args[0].index("-vf") + 1])
            self.assertEqual(list(root.glob("scene-frames-*")), [])

    def test_openai_structured_scene_response_is_validated(self):
        scene = {
            "scene_id": "scene-01", "start": 0, "end": 9.5,
            "summary": "Two people talk indoors.", "activities": ["conversation"],
            "tone": ["calm"], "sensitive_contexts": [], "dialogue_state": "completed_thought",
            "confidence": 0.87, "evidence": ["Two people are visible", "Dialogue transcript"],
        }

        class FakeResponse:
            def __enter__(self):
                return self

            def __exit__(self, *_args):
                return False

            def read(self, _limit):
                return json.dumps({"output": [{"type": "message", "content": [{
                    "type": "output_text", "text": json.dumps({"scenes": [scene]}),
                }]}]}).encode()

        frames = [{"time": 0.0, "data_url": "data:image/jpeg;base64,ZmFrZQ=="}]
        transcript = {"segments": [{"start": 0, "end": 1, "text": "নমস্কার"}]}
        with patch.dict("os.environ", {"OPENAI_API_KEY": "fake-openai-key"}):
            with patch("worker.urllib.request.urlopen", return_value=FakeResponse()) as request:
                scenes = worker.analyze_scenes(frames, transcript, [], 10.0, [9.0])
        self.assertEqual(scenes[0]["scene_id"], "scene-01")
        self.assertEqual(scenes[0]["end"], 9.0)
        self.assertEqual(scenes[0]["shot_boundaries"], [9.0])
        self.assertEqual(scenes[0]["sensitive_contexts"], [])
        request_payload = json.loads(request.call_args.args[0].data)
        self.assertEqual(request_payload["model"], worker.SCENE_MODEL)
        self.assertFalse(request_payload["store"])
        self.assertEqual(request_payload["text"]["format"]["type"], "json_schema")
        user_input = request_payload["input"][1]["content"]
        self.assertEqual(sum(item["type"] == "input_image" for item in user_input), 1)
        self.assertIn("9.00", user_input[0]["text"])
        self.assertEqual(request.call_args.args[0].get_header("User-agent"), "hoichoi-contextual-ad-lab/0.1")

    def test_content_hash_cache_hits_and_invalidates_with_pipeline_version(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            video = root / "tiny.mp4"
            video.write_bytes(b"fixture-video")
            digest = worker._file_sha256(video)
            key = worker._cache_key(digest)
            cached = {
                "language": "bengali", "duration": 2.0, "text": "নমস্কার", "segments": [],
                "model": worker.MODEL, "scenes": [], "silence_intervals": [],
                "scene_analysis_status": "complete", "scene_model": worker.SCENE_MODEL,
                "scene_prompt_version": worker.SCENE_PROMPT_VERSION, "content_hash": digest,
                "cache_key": key, "cache_hit": False,
            }
            worker._write_cache(root / f"analysis-{key}.json", {
                "content_hash": digest, "cache_key": key, "result": cached,
            })
            brands = Path(__file__).resolve().parent.parent / "assets" / "brands.json"
            with patch("worker.extract_audio", side_effect=AssertionError("cache should skip providers")):
                result = worker.run({
                    "video_path": str(video), "work_dir": str(root), "brands_path": str(brands),
                    "content_hash": digest,
                })
            self.assertTrue(result["cache_hit"])
            self.assertEqual(result["cache_key"], key)
            with patch.object(worker, "SCENE_PROMPT_VERSION", "scene-evidence-v-next"):
                self.assertNotEqual(worker._cache_key(digest), key)
            cache_file = root / f"analysis-{key}.json"
            self.assertEqual(cache_file.stat().st_mode & 0o777, 0o600)

    def test_rejects_scene_outside_programme_duration(self):
        with self.assertRaisesRegex(worker.WorkerError, "out-of-range"):
            worker._validate_scenes({"scenes": [{
                "scene_id": "late", "start": 9, "end": 12, "summary": "Too late",
                "activities": [], "tone": [], "sensitive_contexts": [],
                "dialogue_state": "unclear", "confidence": 0.5, "evidence": [],
            }]}, 10)

    def test_provider_error_does_not_echo_response_or_key(self):
        with tempfile.TemporaryDirectory() as directory:
            audio_path = Path(directory) / "audio.mp3"
            audio_path.write_bytes(b"fake")
            with patch.dict("os.environ", {"GROQ_API_KEY": "never-print-this"}):
                http_error = urllib.error.HTTPError(
                    worker.API_URL, 401, "Unauthorized", {}, io.BytesIO(b"never-print-this")
                )
                with patch("worker.urllib.request.urlopen", side_effect=http_error):
                    with self.assertRaisesRegex(worker.WorkerError, "HTTP 401") as error:
                        worker.transcribe(audio_path, "episode")
        self.assertNotIn("never-print-this", str(error.exception))

    def test_successful_provider_response_is_normalized(self):
        class FakeResponse:
            def __enter__(self):
                return self

            def __exit__(self, *_args):
                return False

            def read(self, _limit):
                return json.dumps({
                    "language": "bengali", "duration": 2, "text": "নমস্কার",
                    "segments": [{"text": "নমস্কার", "start": 0.1, "end": 1.2,
                                  "words": [{"word": "নমস্কার", "start": 0.1, "end": 1.1}]}],
                }).encode()

        with tempfile.TemporaryDirectory() as directory:
            audio_path = Path(directory) / "audio.mp3"
            audio_path.write_bytes(b"synthetic-audio")
            with patch.dict("os.environ", {"GROQ_API_KEY": "fake-key"}):
                with patch("worker.urllib.request.urlopen", return_value=FakeResponse()) as request:
                    transcript = worker.transcribe(audio_path, "episode")
        sent_request = request.call_args.args[0]
        self.assertEqual(sent_request.get_header("Authorization"), "Bearer fake-key")
        self.assertEqual(sent_request.get_header("User-agent"), "hoichoi-contextual-ad-lab/0.1")
        self.assertEqual(transcript["language"], "bengali")
        self.assertEqual(transcript["segments"][0]["words"][0]["word"], "নমস্কার")


if __name__ == "__main__":
    unittest.main()
