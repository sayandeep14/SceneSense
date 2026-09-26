import contextlib
import io
import json
import re
import shutil
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import fusion
import scene_ai
import worker

ROOT = Path(__file__).resolve().parent.parent
BRANDS = ROOT / "assets" / "brands.json"
HAS_FFMPEG = bool(shutil.which("ffmpeg") and shutil.which("ffprobe"))
HAS_MODELS = (ROOT / "models" / "clip_vision_quantized.onnx").is_file() and (ROOT / "models" / "yamnet" / "yamnet.onnx").is_file()


def fake_model(scene_changes=True):
    """Stand-in for the Responses API: every boundary is a calm new scene; every scene is a calm chat."""
    brands = json.loads(BRANDS.read_text())

    def call(model, name, schema, system, content, max_tokens):
        text = " ".join(item.get("text", "") for item in content if item["type"] == "input_text")
        if name == "boundary_judgements":
            return {"boundaries": [{
                "candidate_id": candidate_id, "continuity": "new_scene" if scene_changes else "same_scene",
                "change_type": "setting_change", "from_context": "indoor room", "to_context": "open street",
                "topic_shift": 0.7, "tension": 0.2, "dialogue_complete": True, "naturalness": 0.9,
                "disruption_risk": 0.1, "sensitive_contexts": [], "confidence": 0.85, "reason": "New place.",
            } for candidate_id in re.findall(r"Candidate (candidate-\d+) at", text)]}
        return {"scenes": [{
            "scene_id": scene_id, "summary": "Two people talk calmly.", "activities": ["conversation"],
            "tone": ["calm"], "sensitive_contexts": [], "dialogue_state": "completed_thought", "confidence": 0.9,
            "emotional_intensity": 0.2, "valence": 0.3, "evidence": ["Two people visible."],
            "brand_matches": [{"brand_id": brand["brand_id"], "fit_score": 0.6, "matched_contexts": ["conversation"],
                               "reason": "Everyday talk."} for brand in brands],
        } for scene_id in re.findall(r"Scene (scene-\d+) \(", text)]}
    return call


class FusionTests(unittest.TestCase):
    def test_pause_is_local_and_long_non_dialogue_passages_are_not_lulls(self):
        self.assertEqual(fusion.pause_at(10.0, [{"start": 9.5, "end": 10.7}], []), (1.2, "silence"))
        self.assertEqual(fusion.pause_at(10.0, [], [{"start": 0, "end": 60}]), (0.0, ""))
        seconds, source = fusion.pause_at(10.0, [], [{"start": 8, "end": 14}])
        self.assertEqual((seconds, source), (2.8, "lull in dialogue"))

    def test_shortlist_keeps_strongest_boundary_per_neighbourhood(self):
        scored = [{"time": t, "scene_score": s} for t, s in [(10, 0.5), (14, 0.8), (40, 0.25), (60, 0.4), (70, 0.35)]]
        self.assertEqual([item["time"] for item in fusion.shortlist(scored, 100)], [14, 60])


class PacingTests(unittest.TestCase):
    def test_intense_scene_is_a_peak_a_cliffhanger_before_a_scene_change_and_calm_is_a_valley(self):
        import pacing

        scenes = [
            {"scene_id": "calm", "start": 0.0, "end": 60.0, "tone": ["calm"], "emotional_intensity": 0.1},
            {"scene_id": "fight", "start": 60.0, "end": 95.0, "tone": ["confrontational", "tense"], "emotional_intensity": 0.95},
            {"scene_id": "after", "start": 95.0, "end": 200.0, "tone": ["quiet"], "emotional_intensity": 0.15},
        ]
        rapid_cuts = [60.0 + i * 1.5 for i in range(22)]  # fast editing during the fight
        result = pacing.build_pacing(200.0, scenes, None, rapid_cuts, [95.0])
        self.assertEqual(len(result["tension"]), 100)
        self.assertTrue(all(0 <= value <= 1 for value in result["tension"]))
        self.assertEqual(len(result["peaks"]), 1, result["peaks"])
        peak = result["peaks"][0]
        self.assertTrue(60 <= peak["time"] <= 95)
        self.assertEqual((peak["kind"], peak["scene_change"], peak["label"]), ("cliffhanger", 95.0, "confrontational, tense"))
        self.assertTrue(any(valley["time"] < 55 or valley["time"] > 110 for valley in result["valleys"]))
        self.assertIn("1 dramatic peak(s), 1 just before a scene change", result["summary"])

    def test_without_scene_ratings_the_curve_uses_audio_and_editing_only(self):
        import pacing

        result = pacing.build_pacing(30.0, [{"scene_id": "old", "start": 0, "end": 30}], None, [5, 6, 7], [])
        self.assertTrue(all(value is None for value in result["components"]["scene"]))
        self.assertGreater(max(result["tension"]), 0)


class AdFriendlinessTests(unittest.TestCase):
    candidate = {"scene_score": 0.8, "signal_scores": {"pause": 1.0}, "speech_at_cut": 0.0, "shot_transition": "fade",
                 "pause_seconds": 1.8, "pause_source": "silence", "audio": {"before": "Speech", "after": "Music", "shift": 0.5}}
    judgement = {"continuity": "new_scene", "change_type": "setting_change", "from_context": "indoor office",
                 "to_context": "outdoor park", "topic_shift": 0.8, "tension": 0.1, "dialogue_complete": True,
                 "naturalness": 0.9, "disruption_risk": 0.1, "confidence": 0.9, "sensitive_contexts": [], "reason": "x"}

    def test_calm_setting_change_with_pause_is_high_with_readable_rationale(self):
        score, tier, rationale = scene_ai.ad_friendliness(self.candidate, self.judgement)
        self.assertEqual(tier, "High")
        self.assertGreater(score, 0.8)
        self.assertTrue(rationale.startswith("Setting change from indoor office to outdoor park (fade) + 1.8-second silence"))
        self.assertIn("audio speech → music", rationale)

    def test_tense_unfinished_dialogue_with_speech_is_low(self):
        tense = {**self.judgement, "tension": 0.9, "dialogue_complete": False, "naturalness": 0.3, "disruption_risk": 0.8}
        score, tier, rationale = scene_ai.ad_friendliness({**self.candidate, "speech_at_cut": 0.9,
                                                           "signal_scores": {"pause": 0.0}}, tense)
        self.assertEqual(tier, "Low")
        self.assertIn("dialogue continues", rationale)
        self.assertIn("tense moment", rationale)


class BoundaryJudgeTests(unittest.TestCase):
    def test_unknown_ids_and_out_of_range_scores_fail_closed(self):
        from PIL import Image

        with tempfile.NamedTemporaryFile(suffix=".jpg") as frame:
            Image.new("RGB", (398, 224), (40, 80, 60)).save(frame.name)
            candidates = [{"candidate_id": "candidate-001", "time": 5.0, "shot_transition": "cut", "pause_seconds": 0.0,
                           "speech_at_cut": 0.0, "before_text": "", "after_text": ""}]
            good = fake_model()("m", "boundary_judgements", {}, "", [{"type": "input_text", "text": "Candidate candidate-001 at"}], 1)
            bad_id = {"boundaries": [{**good["boundaries"][0], "candidate_id": "candidate-999"}]}
            bad_range = {"boundaries": [{**good["boundaries"][0], "tension": 1.5}]}
            for payload, message in ((bad_id, "unknown or duplicate"), ({"boundaries": []}, "omitted")):
                with patch("scene_ai.call_model", return_value=payload):
                    with self.assertRaisesRegex(scene_ai.SceneAIError, message):
                        scene_ai.judge_boundaries(candidates, lambda _t: frame.name, "m", 10.0)
            # An out-of-range score makes only that boundary uncertain, so the policy blocks it.
            with patch("scene_ai.call_model", return_value=bad_range):
                judged = scene_ai.judge_boundaries(candidates, lambda _t: frame.name, "m", 10.0)["candidate-001"]
            self.assertEqual((judged["continuity"], judged["confidence"], judged["tension"]), ("uncertain", 0.0, 1.0))
            # With no dialogue on either side and no speech at the cut, there is no thought to interrupt.
            unfinished = {"boundaries": [{**good["boundaries"][0], "dialogue_complete": False}]}
            with patch("scene_ai.call_model", return_value=unfinished):
                self.assertTrue(scene_ai.judge_boundaries(candidates, lambda _t: frame.name, "m", 10.0)["candidate-001"]["dialogue_complete"])
            talking = [{**candidates[0], "before_text": "আমি বলছি"}]
            with patch("scene_ai.call_model", return_value=unfinished):
                self.assertFalse(scene_ai.judge_boundaries(talking, lambda _t: frame.name, "m", 10.0)["candidate-001"]["dialogue_complete"])
            self.assertIn("outside 0–1", judged["reason"])


class RateLimitTests(unittest.TestCase):
    class Response:
        def __init__(self, body):
            self.body = body

        def __enter__(self):
            return self

        def __exit__(self, *_args):
            return False

        def read(self, _limit):
            return json.dumps(self.body).encode()

    @staticmethod
    def http_error(code, error_code, headers):
        import email.message
        import urllib.error

        message = email.message.Message()
        for name, value in headers.items():
            message[name] = value
        return urllib.error.HTTPError(scene_ai.OPENAI_API_URL, code, "error", message,
                                      io.BytesIO(json.dumps({"error": {"code": error_code}}).encode()))

    def test_retry_after_headers_are_parsed(self):
        self.assertEqual(scene_ai._retry_after({"retry-after": "3"}), 3.0)
        self.assertAlmostEqual(scene_ai._retry_after({"x-ratelimit-reset-tokens": "1m2.5s", "x-ratelimit-reset-requests": "250ms"}), 62.5)
        self.assertIsNone(scene_ai._retry_after({}))

    def test_rate_limit_is_retried_after_the_server_wait_and_quota_fails_fast(self):
        ok = self.Response({"output": [{"content": [{"type": "output_text", "text": '{"boundaries": []}'}]}]})
        limited = self.http_error(429, "rate_limit_exceeded", {"x-ratelimit-reset-tokens": "1.5s"})
        with patch.dict("os.environ", {"OPENAI_API_KEY": "k"}), patch("scene_ai.time.sleep") as sleep, \
                patch("scene_ai.urllib.request.urlopen", side_effect=[limited, ok]) as request:
            self.assertEqual(scene_ai.call_model("m", "n", {}, "s", [], 10), {"boundaries": []})
        self.assertEqual(request.call_count, 2)
        self.assertGreaterEqual(sleep.call_args.args[0], 1.5)
        quota = self.http_error(429, "insufficient_quota", {})
        with patch.dict("os.environ", {"OPENAI_API_KEY": "k"}), patch("scene_ai.time.sleep"), \
                patch("scene_ai.urllib.request.urlopen", side_effect=[quota]) as request:
            with self.assertRaisesRegex(scene_ai.SceneAIError, "quota is exhausted"):
                scene_ai.call_model("m", "n", {}, "s", [], 10)
        self.assertEqual(request.call_count, 1)
        errors = [self.http_error(429, "rate_limit_exceeded", {"retry-after": "1"}) for _ in range(scene_ai.MAX_ATTEMPTS)]
        with patch.dict("os.environ", {"OPENAI_API_KEY": "k"}), patch("scene_ai.time.sleep"), \
                patch("scene_ai.urllib.request.urlopen", side_effect=errors):
            with self.assertRaisesRegex(scene_ai.SceneAIError, "HTTP 429 \\(rate_limit_exceeded\\) after 6 attempt"):
                scene_ai.call_model("m", "n", {}, "s", [], 10)

    def test_token_budget_waits_for_the_minute_window(self):
        budget = scene_ai.TokenBudget(100)
        clock = [0.0]
        with patch("scene_ai.time.monotonic", side_effect=lambda: clock[0]), \
                patch("scene_ai.time.sleep", side_effect=lambda seconds: clock.__setitem__(0, clock[0] + seconds)) as sleep:
            budget.acquire(80)
            budget.acquire(80)  # would exceed 100 within the minute, so it waits for the window
            self.assertEqual(sleep.call_count, 1)
            self.assertGreaterEqual(clock[0], 60)
            budget.acquire(500)  # larger than the whole budget: allowed once the window is empty
        self.assertGreaterEqual(clock[0], 120)

    def test_frames_are_tiled_into_one_image(self):
        from PIL import Image

        with tempfile.TemporaryDirectory() as directory:
            paths = []
            for index in range(3):
                path = Path(directory) / f"f{index}.jpg"
                Image.new("RGB", (398, 224), (index * 60, 20, 20)).save(path)
                paths.append(str(path))
            image = scene_ai._composite(paths, columns=2)
        import base64

        sheet = Image.open(io.BytesIO(base64.b64decode(image["image_url"].split(",", 1)[1])))
        self.assertEqual((image["detail"], sheet.size), ("low", (802, 454)))
        self.assertEqual(scene_ai.estimate_tokens([image, {"type": "input_text", "text": "x" * 100}], "s" * 100, 50),
                         scene_ai.IMAGE_TOKENS + 100 + 50)


class RetryPhaseTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.root = Path(self.directory.name)
        self.video = self.root / "episode.mp4"
        self.video.write_bytes(b"video")
        self.request = {"video_path": str(self.video), "work_dir": str(self.root), "brands_path": str(BRANDS)}
        self.transcript = {"language": "bn", "duration": 10.0, "text": "কথা", "model": worker._asr_label(),
                           "segments": [{"text": "কথা", "start": 0.5, "end": 2.0}], "timestamp_adjustments": 0}

    def tearDown(self):
        self.directory.cleanup()

    def run_worker(self, request, transcribe_result=None):
        with patch("worker.extract_audio", return_value=self.root / "audio.wav"), \
                patch("worker.transcribe", side_effect=[transcribe_result] if transcribe_result else AssertionError("ASR ran")) as asr, \
                patch("worker.detect_silences", return_value=[]), patch("worker.analyze_programme"), \
                contextlib.redirect_stderr(io.StringIO()):
            result = worker.run(request)
        return result, asr.call_count

    def test_scene_retry_uses_the_supplied_transcript_without_asr(self):
        result, calls = self.run_worker({**self.request, "from_phase": "scene_analysis", "transcript": self.transcript})
        self.assertEqual((calls, result["segments"][0]["text"], result["evidence_cache_hit"]), (0, "কথা", True))
        silent = {**self.transcript, "text": "", "segments": []}  # no dialogue is still a transcript
        result, calls = self.run_worker({**self.request, "from_phase": "scene_analysis", "transcript": silent})
        self.assertEqual((calls, result["segments"]), (0, []))

    def test_transcript_is_saved_right_after_asr_so_a_failed_analysis_can_retry_cheaply(self):
        with patch("worker.extract_audio", return_value=self.root / "audio.wav"), \
                patch("worker.transcribe", return_value=self.transcript), patch("worker.detect_silences", return_value=[]), \
                patch("worker.analyze_programme", side_effect=RuntimeError("worker crashed after ASR")), \
                contextlib.redirect_stderr(io.StringIO()):
            with self.assertRaises(RuntimeError):
                worker.run(self.request)
        result, calls = self.run_worker({**self.request, "from_phase": "scene_analysis"})
        self.assertEqual(calls, 0)
        self.assertEqual(result["segments"][0]["text"], "কথা")

    def test_transcription_retry_ignores_saved_transcripts_and_scene_retry_needs_one(self):
        fresh = {**self.transcript, "text": "নতুন", "segments": [{"text": "নতুন", "start": 0.5, "end": 2.0}]}
        result, calls = self.run_worker({**self.request, "from_phase": "transcription", "transcript": self.transcript}, fresh)
        self.assertEqual((calls, result["segments"][0]["text"]), (1, "নতুন"))
        for path in self.root.glob("transcript-*.json"):
            path.unlink()
        with self.assertRaisesRegex(worker.WorkerError, "retry transcription"):
            self.run_worker({**self.request, "from_phase": "scene_analysis",
                             "transcript": {**self.transcript, "model": "another-asr"}})


@unittest.skipUnless(HAS_FFMPEG, "FFmpeg is not installed")
class ShotDetectionTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        import synthetic_fixture

        cls.directory = tempfile.TemporaryDirectory()
        cls.root = Path(cls.directory.name)
        cls.video = cls.root / "fixture.mp4"
        cls.truth = synthetic_fixture.build(cls.video)

    @classmethod
    def tearDownClass(cls):
        cls.directory.cleanup()

    def test_detects_cuts_fades_and_dissolves_without_camera_motion_false_positives(self):
        import shots

        result = shots.segment_shots(self.video, self.root, self.truth["duration"])
        try:
            found = [(item["time"], item["kind"]) for item in result["boundaries"]]
            self.assertEqual(len(found), len(self.truth["shot_boundaries"]), found)
            for expected, (time, _kind) in zip(self.truth["shot_boundaries"], found):
                self.assertAlmostEqual(time, expected, delta=0.25)
            kinds = {round(time): kind for time, kind in found}
            self.assertEqual((kinds[20], kinds[40], kinds[55]), ("cut", "fade", "dissolve"))
            self.assertTrue(all(Path(frame["path"]).is_file() for shot in result["shots"] for frame in shot["keyframes"]))
        finally:
            shutil.rmtree(result["pool_dir"])

    @unittest.skipUnless(HAS_MODELS, "Local models are not downloaded")
    def test_pipeline_finds_scene_changes_not_reverse_angles_and_reuses_transcript(self):
        # Whisper-style hallucinated text over tones and noise; YAMNet hears no speech, so it must be ignored.
        invented = [{"text": "কাল্পনিক সংলাপ।", "start": start, "end": start + 5} for start in (15.0, 36.0, 52.0)]
        transcript = {"language": "bn", "duration": self.truth["duration"], "text": "", "segments": invented,
                      "model": worker._asr_label(), "timestamp_adjustments": 0}
        work = self.root / "work"
        work.mkdir(exist_ok=True)
        request = {"video_path": str(self.video), "work_dir": str(work), "brands_path": str(BRANDS)}
        with patch.dict("os.environ", {"OPENAI_API_KEY": "", "ASR_PROVIDER": "groq"}):
            with patch("worker.extract_audio", return_value=self.root / "unused.mp3"), \
                    patch("worker.transcribe", return_value=transcript) as asr, \
                    patch("scene_ai.call_model", side_effect=fake_model()), \
                    contextlib.redirect_stderr(io.StringIO()) as log:
                result = worker.run(request)
        self.assertEqual(asr.call_count, 1)
        self.assertIn("@@progress", log.getvalue())
        self.assertEqual(result["scene_analysis_status"], "complete", result["scene_analysis_error"])
        changes = [item["time"] for item in result["break_candidates"] if item["scene_change"]]
        self.assertEqual(len(changes), 3, changes)
        for expected, time in zip(self.truth["scene_changes"], changes):
            self.assertAlmostEqual(time, expected, delta=0.25)
        self.assertEqual(len(result["shot_boundaries"]), 7)
        self.assertEqual(result["unheard_segment_count"], 3)
        self.assertEqual(len(result["pacing"]["tension"]), int(-(-self.truth["duration"] // 2)))
        self.assertTrue(all(scene["emotional_intensity"] == 0.2 for scene in result["scenes"]))
        self.assertTrue(all(not item["before_text"] and not item["after_text"] for item in result["break_candidates"]))
        self.assertEqual(len(result["scenes"]), 4)
        self.assertTrue(all(item["tier"] in ("High", "Medium", "Low") and item["rationale"] for item in result["break_candidates"]))

        # A pipeline upgrade re-runs analysis but reuses the saved transcript instead of paying for ASR again.
        with patch.object(worker, "PIPELINE_CACHE_VERSION", "scene-fusion-pipeline-next"), \
                patch("worker.transcribe", side_effect=AssertionError("transcript should be reused")), \
                patch("scene_ai.call_model", side_effect=fake_model()), contextlib.redirect_stderr(io.StringIO()):
            upgraded = worker.run(request)
        self.assertTrue(upgraded["evidence_cache_hit"])
        self.assertFalse(upgraded["cache_hit"])


if __name__ == "__main__":
    unittest.main()
