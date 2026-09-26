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
            "evidence": ["Two people visible."],
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
        with tempfile.NamedTemporaryFile(suffix=".jpg") as frame:
            frame.write(b"jpeg")
            frame.flush()
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
