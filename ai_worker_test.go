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
	transcript, err := runAIWorker(context.Background(), python, worker, "unused.mp4", t.TempDir(), "assets/brands.json", "fixture-hash")
	if err != nil {
		t.Fatalf("run AI worker: %v", err)
	}
	if transcript.Language != "bengali" || len(transcript.Segments) != 1 || transcript.Model != "fixture" ||
		len(transcript.Scenes) != 1 || len(transcript.SilenceIntervals) != 1 || transcript.SceneAnalysisStatus != "complete" {
		t.Fatalf("unexpected transcript: %+v", transcript)
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
	_, err = runAIWorker(context.Background(), python, worker, "unused.mp4", t.TempDir(), "assets/brands.json", "fixture-hash")
	if err == nil || !strings.Contains(err.Error(), "invalid segment timestamps") {
		t.Fatalf("error = %v, want invalid segment timestamps", err)
	}
}
