package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCounterfactualSimulationReusesSafetyEvidenceWithoutMutatingJob(t *testing.T) {
	transcript := policyFixture(1800, 200, 400, 600, 800, 1000, 1200, 1400, 1600)
	transcript.BreakCandidates[3].PrecedingSensitiveContexts = []string{"mourning"}
	applyBreakPolicy(&transcript)
	app := newServer(slog.New(slog.NewTextHandler(io.Discard, nil)), t.TempDir())
	app.jobs["fixture"] = Job{ID: "fixture", Status: "completed", Transcript: &transcript}
	before, _ := json.Marshal(app.jobs["fixture"])
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/jobs/fixture/simulate", strings.NewReader(`{"min_gap_seconds":120,"max_breaks_per_hour":12,"max_ad_load_percent":30}`))
	app.routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("simulation failed: %d %s", response.Code, response.Body.String())
	}
	var result struct {
		Baseline OptimizeResult `json:"baseline"`
		Scenario OptimizeResult `json:"scenario"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Scenario.K <= result.Baseline.K {
		t.Fatalf("relaxed pacing did not change eligible placements: %d versus %d", result.Scenario.K, result.Baseline.K)
	}
	for _, item := range result.Scenario.Selected {
		if item.Time == 800 {
			t.Fatal("sensitive context was selected by simulator")
		}
	}
	after, _ := json.Marshal(app.jobs["fixture"])
	if string(before) != string(after) {
		t.Fatal("simulation changed saved job state")
	}
}

func TestCounterfactualSimulationRejectsUnsafeParameters(t *testing.T) {
	transcript := policyFixture(600, 300)
	applyBreakPolicy(&transcript)
	app := newServer(slog.New(slog.NewTextHandler(io.Discard, nil)), t.TempDir())
	app.jobs["fixture"] = Job{ID: "fixture", Status: "completed", Transcript: &transcript}
	response := httptest.NewRecorder()
	app.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/jobs/fixture/simulate", strings.NewReader(`{"min_gap_seconds":0,"max_breaks_per_hour":99,"max_ad_load_percent":100}`)))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("bad limits accepted: %d %s", response.Code, response.Body.String())
	}
}
