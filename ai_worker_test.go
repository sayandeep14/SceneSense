package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAIWorkerJSONBoundaryAndTranscriptValidation(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	worker := filepath.Join(t.TempDir(), "worker.py")
	contents := `import json,sys
request=json.load(sys.stdin)
json.dump({"language":"bengali","duration":2,"text":"নমস্কার","segments":[{"text":"নমস্কার","start":0.1,"end":1.0}],"model":"fixture","scenes":[{"scene_id":"scene-01","start":0,"end":1.5,"summary":"Two people talk","activities":["conversation"],"tone":["calm"],"sensitive_contexts":[],"dialogue_state":"completed_thought","confidence":0.9,"evidence":["two people visible"]}],"silence_intervals":[{"start":1.1,"end":1.8,"duration":0.7}],"scene_analysis_status":"complete","scene_model":"fixture-vision","scene_prompt_version":"test-v1"},sys.stdout)`
	if err := os.WriteFile(worker, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	transcript, err := runAIWorker(context.Background(), python, worker, workerRequest{VideoPath: "unused.mp4", WorkDir: t.TempDir(), BrandsPath: "assets/brands.json", ContentHash: "fixture-hash"}, nil)
	if err != nil {
		t.Fatalf("run AI worker: %v", err)
	}
	if transcript.Language != "bengali" || len(transcript.Segments) != 1 || transcript.Model != "fixture" ||
		len(transcript.Scenes) != 1 || len(transcript.SilenceIntervals) != 1 || transcript.SceneAnalysisStatus != "complete" {
		t.Fatalf("unexpected transcript: %+v", transcript)
	}
}

func TestAIWorkerBoundaryAppliesPolicyToModelScores(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	path := filepath.Join(t.TempDir(), "worker.py")
	contents := `import json,sys
json.load(sys.stdin)
json.dump({"duration":80,"segments":[{"start":2,"end":8,"text":"কথা শেষ।"}],"scenes":[{"scene_id":"one","start":0,"end":80,"summary":"A calm talk","dialogue_state":"completed_thought","confidence":0.9,"sensitive_contexts":[]}],"scene_analysis_status":"complete","silence_intervals":[{"start":39,"end":41,"duration":2}],"break_scoring_status":"complete","break_model":"test-model","break_prompt_version":"test-v1","break_candidates":[{"candidate_id":"candidate-001","time":40,"signals":["low_audio_pause"],"naturalness":0.9,"disruption_risk":0.1,"confidence":0.9,"ai_reason":"The pause feels natural.","ai_model":"test-model","ai_prompt_version":"test-v1","preceding_scene_context":"A quiet family conversation","preceding_scene_mood":"calm, warm","scene_change":True,"continuity":"new_scene","dialogue_complete":True,"tension":0.1,"ad_score":0.8,"tier":"High","rationale":"Setting change + 2.0-second silence.","signal_scores":{"pause":1.0}}]},sys.stdout)`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	transcript, err := runAIWorker(context.Background(), python, path, workerRequest{VideoPath: "unused.mp4", WorkDir: t.TempDir(), BrandsPath: "assets/brands.json", ContentHash: "fixture-hash"}, nil)
	if err != nil {
		t.Fatalf("run worker: %v", err)
	}
	if transcript.BreakPolicy.AcceptedCount != 1 || transcript.BreakCandidates[0].Decision != "accepted" {
		t.Fatalf("model scores were not filtered through the policy: %+v", transcript.BreakCandidates)
	}
	if transcript.BreakCandidates[0].PrecedingSceneMood != "calm, warm" ||
		transcript.BreakCandidates[0].PrecedingSceneContext != "A quiet family conversation" ||
		transcript.BreakCandidates[0].Tier != "High" || transcript.BreakCandidates[0].SignalScores["pause"] != 1 {
		t.Fatalf("preceding scene context was lost at the Python/Go boundary: %+v", transcript.BreakCandidates[0])
	}
}

func TestAIWorkerProgressLinesUpdateStageAndStayOutOfErrors(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	path := filepath.Join(t.TempDir(), "worker.py")
	contents := `import json,sys
json.load(sys.stdin)
print('@@progress {"stage":"detecting_shots","progress":36,"message":"Detecting shots."}', file=sys.stderr, flush=True)
print("Shot detection failed: the file is truncated.", file=sys.stderr)
sys.exit(2)`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	var stages []string
	_, err = runAIWorker(context.Background(), python, path, workerRequest{VideoPath: "unused.mp4", WorkDir: t.TempDir(), BrandsPath: "assets/brands.json", ContentHash: "fixture-hash"},
		func(stage string, progress int, message string) { stages = append(stages, stage) })
	if len(stages) != 1 || stages[0] != "detecting_shots" {
		t.Fatalf("progress stages = %v", stages)
	}
	if err == nil || err.Error() != "Shot detection failed: the file is truncated." {
		t.Fatalf("error should be the worker message without progress lines, got %v", err)
	}
}

func TestAIWorkerRejectsOutOfRangeSceneEvidence(t *testing.T) {
	transcript := Transcript{
		Duration: 10,
		Scenes:   []SceneEvidence{{SceneID: "late", Start: 9, End: 12, Summary: "Beyond video", Confidence: 0.8}},
	}
	if err := validateTranscriptGo(transcript); err == nil || !strings.Contains(err.Error(), "outside the media duration") {
		t.Fatalf("scene validation error = %v, want out-of-range error", err)
	}
}

func TestAIWorkerRejectsOutOfRangeShotCutEvidence(t *testing.T) {
	transcript := Transcript{
		Duration: 10, ShotBoundaries: []float64{2, 10},
	}
	if err := validateTranscriptGo(transcript); err == nil || !strings.Contains(err.Error(), "shot-cut timestamps") {
		t.Fatalf("shot-cut validation error = %v, want out-of-range error", err)
	}
	transcript = Transcript{
		Duration: 10,
		Scenes:   []SceneEvidence{{SceneID: "one", Start: 1, End: 5, Summary: "Scene", Confidence: 0.8, ShotBoundaries: []float64{5, 2}}},
	}
	if err := validateTranscriptGo(transcript); err == nil || !strings.Contains(err.Error(), "scene shot-cut") {
		t.Fatalf("scene shot-cut validation error = %v, want ordering error", err)
	}
}

func TestAIWorkerRejectsInvalidOutput(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	worker := filepath.Join(t.TempDir(), "worker.py")
	if err := os.WriteFile(worker, []byte(`print('{"duration":1,"segments":[{"start":0.8,"end":1},{"start":0.2,"end":0.7}]}')`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = runAIWorker(context.Background(), python, worker, workerRequest{VideoPath: "unused.mp4", WorkDir: t.TempDir(), BrandsPath: "assets/brands.json", ContentHash: "fixture-hash"}, nil)
	if err == nil || !strings.Contains(err.Error(), "invalid segment timestamps") {
		t.Fatalf("error = %v, want invalid segment timestamps", err)
	}
}

func TestPacingAndSceneEmotionAreRangeChecked(t *testing.T) {
	high, low := 1.4, -2.0
	for _, transcript := range []Transcript{
		{Duration: 10, Scenes: []SceneEvidence{{SceneID: "a", Start: 0, End: 5, Summary: "x", Confidence: 0.9, EmotionalIntensity: &high}}},
		{Duration: 10, Scenes: []SceneEvidence{{SceneID: "a", Start: 0, End: 5, Summary: "x", Confidence: 0.9, Valence: &low}}},
		{Duration: 10, Pacing: &PacingMap{StepSec: 2, Tension: []float64{0.2, 1.3}}},
		{Duration: 10, Pacing: &PacingMap{StepSec: 2, Tension: make([]float64, 40)}},
		{Duration: 10, Pacing: &PacingMap{StepSec: 2, Tension: []float64{0.2}, Peaks: []PacingPoint{{Time: 99, Value: 0.8}}}},
	} {
		if err := validateTranscriptGo(transcript); err == nil {
			t.Fatalf("invalid emotion or pacing accepted: %+v", transcript)
		}
	}
	valid := Transcript{Duration: 10, Pacing: &PacingMap{StepSec: 2, Tension: []float64{0.1, 0.9, 0.4},
		Peaks: []PacingPoint{{Time: 2, Value: 0.9, Kind: "cliffhanger"}}}}
	if err := validateTranscriptGo(valid); err != nil {
		t.Fatal(err)
	}
}
