package main

import (
	"math"
	"reflect"
	"strings"
	"testing"
)

// policyFixture builds a programme whose candidates are all calm, clean scene changes.
func policyFixture(duration float64, times ...float64) Transcript {
	transcript := Transcript{
		Duration: duration, SceneAnalysisStatus: "complete", BreakScoringStatus: "complete",
		BreakModel: "fixture-model", BreakPromptVersion: "fixture-prompt",
		Scenes: []SceneEvidence{{SceneID: "whole", Start: 0, End: duration, Summary: "A calm conversation",
			DialogueState: "completed_thought", Confidence: 0.9}},
	}
	for index, at := range times {
		transcript.BreakCandidates = append(transcript.BreakCandidates, BreakCandidate{
			CandidateID: "candidate-" + string(rune('a'+index)), Time: at, Signals: []string{"shot_cut"},
			SceneChange: true, Continuity: "new_scene", DialogueComplete: true, Tension: 0.1,
			Naturalness: 0.9, DisruptionRisk: 0.1, Confidence: 0.9, AdScore: 0.8, Tier: "High",
			AIReason: "A clean change of place.", AIModel: "fixture-model", AIPromptVer: "fixture-prompt",
		})
	}
	return transcript
}

func hasReason(candidate BreakCandidate, code string) bool {
	for _, reason := range candidate.Reasons {
		if reason.Code == code {
			return true
		}
	}
	return false
}

func accepted(transcript Transcript) []float64 {
	times := []float64{}
	for _, candidate := range transcript.BreakCandidates {
		if candidate.Decision == "accepted" {
			times = append(times, candidate.Time)
		}
	}
	return times
}

func TestBreakPolicyAcceptsCleanSceneChange(t *testing.T) {
	transcript := policyFixture(600, 300)
	if err := validateBreakCandidates(transcript); err != nil {
		t.Fatal(err)
	}
	applyBreakPolicy(&transcript)
	if transcript.BreakCandidates[0].Decision != "accepted" || transcript.BreakPolicy.AcceptedCount != 1 ||
		transcript.BreakPolicy.AutoBreakCount != 1 || transcript.BreakPolicy.SceneChangeCount != 1 {
		t.Fatalf("expected one accepted break: %+v %+v", transcript.BreakCandidates[0], transcript.BreakPolicy)
	}
	if !strings.HasPrefix(transcript.BreakCandidates[0].SelectionNote, "Selected: High") {
		t.Fatalf("selected break has no explanation: %q", transcript.BreakCandidates[0].SelectionNote)
	}
}

func TestBreakPolicyBlocksCameraCutsSpeechTensionAndUnfinishedDialogue(t *testing.T) {
	cases := map[string]func(*BreakCandidate){
		"same_scene":          func(c *BreakCandidate) { c.SceneChange, c.Continuity = false, "same_scene" },
		"active_speech":       func(c *BreakCandidate) { c.SpeechAtCut = 0.8 },
		"high_tension":        func(c *BreakCandidate) { c.Tension = 0.85 },
		"unfinished_dialogue": func(c *BreakCandidate) { c.DialogueComplete = false },
		"uncertain_context":   func(c *BreakCandidate) { c.Continuity = "uncertain" },
		"sensitive_context":   func(c *BreakCandidate) { c.PrecedingSensitiveContexts = []string{"funeral"} },
		"edge_guard":          func(c *BreakCandidate) { c.Time = 8 },
	}
	for code, mutate := range cases {
		transcript := policyFixture(600, 300)
		mutate(&transcript.BreakCandidates[0])
		applyBreakPolicy(&transcript)
		if !hasReason(transcript.BreakCandidates[0], code) || transcript.BreakCandidates[0].Decision == "accepted" {
			t.Errorf("%s was not enforced: %+v", code, transcript.BreakCandidates[0])
		}
	}
	transcript := policyFixture(600, 300)
	transcript.Segments = []TranscriptSegment{{Text: "কথা", Start: 299, End: 301,
		Words: []TranscriptWord{{Word: "কথা", Start: 299.8, End: 300.3}}}}
	applyBreakPolicy(&transcript)
	if !hasReason(transcript.BreakCandidates[0], "active_speech") {
		t.Fatalf("a word spanning the cut was accepted: %+v", transcript.BreakCandidates[0])
	}
}

func TestBreakPolicyFailsClosedWithoutEvidence(t *testing.T) {
	transcript := policyFixture(600, 300)
	transcript.BreakScoringStatus = "unavailable"
	transcript.Scenes = nil
	applyBreakPolicy(&transcript)
	for _, code := range []string{"ai_unavailable", "unknown_context"} {
		if !hasReason(transcript.BreakCandidates[0], code) {
			t.Fatalf("missing fail-closed reason %s: %+v", code, transcript.BreakCandidates[0])
		}
	}
	if transcript.BreakPolicy.AcceptedCount != 0 {
		t.Fatal("a break was accepted without evidence")
	}
}

func TestOptimizerNeverPlacesBreaksCloserThanTheMinimumGap(t *testing.T) {
	// Dense candidates every 20 s across 30 minutes, with high scores clustered near the start.
	times := []float64{}
	for at := 30.0; at < 1780; at += 20 {
		times = append(times, at)
	}
	transcript := policyFixture(1800, times...)
	for i := range transcript.BreakCandidates {
		transcript.BreakCandidates[i].AdScore = math.Max(0.55, 0.95-float64(i)*0.004)
	}
	applyBreakPolicy(&transcript)
	chosen := accepted(transcript)
	if len(chosen) != maxBreakCount(1800) || transcript.BreakPolicy.AutoBreakCount != len(chosen) {
		t.Fatalf("auto k = %d, chosen %v, max %d", transcript.BreakPolicy.AutoBreakCount, chosen, maxBreakCount(1800))
	}
	for i := 1; i < len(chosen); i++ {
		if chosen[i]-chosen[i-1] < minBreakGap {
			t.Fatalf("breaks %v are closer than %v s", chosen, minBreakGap)
		}
	}
	// Even spacing: the optimiser must not bunch the breaks where scores are highest.
	if chosen[len(chosen)-1] < 1200 {
		t.Fatalf("breaks are bunched at the start: %v", chosen)
	}
	for _, candidate := range transcript.BreakCandidates {
		if candidate.Decision != "accepted" && candidate.SelectionNote == "" {
			t.Fatalf("unselected candidate at %v has no explanation", candidate.Time)
		}
	}
	first := append([]BreakCandidate(nil), transcript.BreakCandidates...)
	applyBreakPolicy(&transcript)
	if !reflect.DeepEqual(first, transcript.BreakCandidates) {
		t.Fatal("same input and policy produced different decisions")
	}
}

func TestOptimizerHonoursRequestedKPinsExclusionsAndLowTier(t *testing.T) {
	transcript := policyFixture(1800, 200, 520, 900, 1250, 1600)
	transcript.BreakCandidates[2].Tier, transcript.BreakCandidates[2].AdScore = "Low", 0.4
	applyBreakPolicy(&transcript)
	if transcript.BreakPolicy.AutoBreakCount != 4 || contains(accepted(transcript), 900) {
		t.Fatalf("low-tier spot should not be used automatically: %v auto=%d", accepted(transcript), transcript.BreakPolicy.AutoBreakCount)
	}

	two := 2
	result, err := optimizeBreaks(&transcript, OptimizeRequest{K: &two})
	if err != nil || result.K != 2 || len(result.Selected) != 2 {
		t.Fatalf("k=2 result = %+v, %v", result, err)
	}

	four := 4
	result, err = optimizeBreaks(&transcript, OptimizeRequest{K: &four, Excluded: []string{"candidate-d"}})
	if err != nil || len(result.Selected) != 4 || result.Selected[2].Tier != "Low" {
		t.Fatalf("asking for more breaks than strong spots allow should fall back to the Low spot: %+v, %v", result, err)
	}

	result, err = optimizeBreaks(&transcript, OptimizeRequest{Pinned: []float64{700}, Excluded: []string{"candidate-a"}})
	if err != nil {
		t.Fatal(err)
	}
	var times []float64
	for _, entry := range result.Selected {
		times = append(times, entry.Time)
		if entry.CandidateID == "candidate-a" {
			t.Fatal("an excluded candidate was selected")
		}
	}
	if !contains(times, 700) || contains(times, 520) || contains(times, 900) {
		t.Fatalf("pinned reviewer break must stay and crowd out its neighbours: %v", times)
	}

	if _, err := optimizeBreaks(&transcript, OptimizeRequest{Pinned: []float64{400, 500}}); err == nil {
		t.Fatal("reviewer placements closer than the minimum gap were accepted")
	}
}

func TestMaxBreakCountRespectsHourlyCapAdLoadAndGap(t *testing.T) {
	for duration, want := range map[float64]int{60: 1, 600: 2, 1800: 4, 2700: 6, 20: 0} {
		if got := maxBreakCount(duration); got != want {
			t.Errorf("maxBreakCount(%v) = %d, want %d", duration, got, want)
		}
	}
}

func TestBreakCandidateContractRejectsBadScoresTiersAndBlockedRecommendations(t *testing.T) {
	transcript := policyFixture(600, 300)
	transcript.BreakCandidates[0].AdScore = 1.4
	if err := validateBreakCandidates(transcript); err == nil {
		t.Fatal("out-of-range ad score was accepted")
	}
	transcript = policyFixture(600, 300)
	transcript.BreakCandidates[0].Tier = "Maybe"
	if err := validateBreakCandidates(transcript); err == nil {
		t.Fatal("unknown tier was accepted")
	}
	transcript = policyFixture(600, 300, 200)
	if err := validateBreakCandidates(transcript); err == nil {
		t.Fatal("unordered candidates were accepted")
	}
	transcript = policyFixture(600, 300)
	transcript.BreakCandidates[0].BrandRecommendations = []SceneBrandMatch{{
		BrandID: "food", DisplayName: "Food Brand", Category: "food", FitScore: 0.95,
		Reason: "Strong fit", Blocked: true, BlockedContexts: []string{"medical"}, Recommended: true,
	}}
	if err := validateBreakCandidates(transcript); err == nil {
		t.Fatal("blocked brand recommendation was accepted")
	}
}

func contains(values []float64, target float64) bool {
	for _, value := range values {
		if math.Abs(value-target) < 0.01 {
			return true
		}
	}
	return false
}
