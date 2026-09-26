package main

import (
	"reflect"
	"testing"
)

func policyFixture(duration float64, times ...float64) Transcript {
	transcript := Transcript{
		Duration: duration, SceneAnalysisStatus: "complete", BreakScoringStatus: "complete",
		BreakModel: "fixture-model", BreakPromptVersion: "fixture-prompt",
		Scenes: []SceneEvidence{{SceneID: "whole", Start: 0, End: duration, Summary: "A calm conversation",
			DialogueState: "completed_thought", Confidence: 0.9}},
		Segments: []TranscriptSegment{{Text: "কথা শেষ।", Start: 1, End: 8}},
	}
	for index, at := range times {
		transcript.SilenceIntervals = append(transcript.SilenceIntervals, SilenceInterval{Start: at - 1, End: at + 1, Duration: 2})
		transcript.BreakCandidates = append(transcript.BreakCandidates, BreakCandidate{
			CandidateID: string(rune('a' + index)), Time: at, Signals: []string{"low_audio_pause"},
			Naturalness: 0.90, DisruptionRisk: 0.10, Confidence: 0.90,
			AIReason: "A completed thought and calm pause.", AIModel: "fixture-model", AIPromptVer: "fixture-prompt",
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

func TestBreakPolicyAcceptsVerifiedNaturalPause(t *testing.T) {
	transcript := policyFixture(80, 40)
	if err := validateBreakCandidates(transcript); err != nil {
		t.Fatal(err)
	}
	applyBreakPolicy(&transcript)
	if transcript.BreakCandidates[0].Decision != "accepted" || transcript.BreakPolicy.AcceptedCount != 1 {
		t.Fatalf("expected one accepted break: %+v", transcript.BreakCandidates[0])
	}
	if transcript.BreakPolicy.Version != breakPolicyVersion {
		t.Fatalf("wrong policy version: %+v", transcript.BreakPolicy)
	}
}

func TestBreakPolicyRejectsSpeechAndUnfinishedThought(t *testing.T) {
	transcript := policyFixture(80, 40)
	transcript.Segments = []TranscriptSegment{{Text: "আমি এখন বলছি", Start: 38, End: 42}}
	applyBreakPolicy(&transcript)
	if !hasReason(transcript.BreakCandidates[0], "active_speech") {
		t.Fatalf("speech overlap was accepted: %+v", transcript.BreakCandidates[0])
	}

	transcript = policyFixture(80, 40)
	transcript.Segments = []TranscriptSegment{{Text: "আমি এখন বলছি", Start: 30, End: 39}}
	transcript.Scenes[0].DialogueState = "ongoing"
	applyBreakPolicy(&transcript)
	if !hasReason(transcript.BreakCandidates[0], "unfinished_sentence") ||
		!hasReason(transcript.BreakCandidates[0], "ongoing_dialogue") {
		t.Fatalf("unfinished phrase was accepted: %+v", transcript.BreakCandidates[0])
	}
}

func TestCoarseASRNeedsPauseAtCompletedSceneBoundary(t *testing.T) {
	transcript := policyFixture(80, 40)
	transcript.Segments = []TranscriptSegment{{Text: "একটি দীর্ঘ সারভাম বাক্য।", Start: 0, End: 75}}
	applyBreakPolicy(&transcript)
	if !hasReason(transcript.BreakCandidates[0], "coarse_asr_timing") {
		t.Fatalf("mid-scene pause with broad ASR timing was accepted: %+v", transcript.BreakCandidates[0])
	}

	transcript.Scenes = []SceneEvidence{
		{SceneID: "one", Start: 0, End: 40, Summary: "The conversation finishes", DialogueState: "completed_thought", Confidence: 0.9},
		{SceneID: "two", Start: 40, End: 80, Summary: "A new scene begins", DialogueState: "completed_thought", Confidence: 0.9},
	}
	transcript.BreakCandidates[0].Signals = []string{"low_audio_pause", "scene_end"}
	applyBreakPolicy(&transcript)
	if transcript.BreakCandidates[0].Decision != "accepted" {
		t.Fatalf("verified pause at completed scene boundary was rejected: %+v", transcript.BreakCandidates[0])
	}
}

func TestBreakPolicyFailsClosedOnContextAndMissingEvidence(t *testing.T) {
	transcript := policyFixture(80, 40)
	transcript.Scenes[0].SensitiveContexts = []string{"mourning"}
	transcript.Scenes[0].Confidence = 0.5
	applyBreakPolicy(&transcript)
	if !hasReason(transcript.BreakCandidates[0], "sensitive_context") ||
		!hasReason(transcript.BreakCandidates[0], "uncertain_context") {
		t.Fatalf("sensitive/uncertain scene was accepted: %+v", transcript.BreakCandidates[0])
	}

	transcript = policyFixture(80, 40)
	transcript.SilenceIntervals = nil
	transcript.BreakScoringStatus = "unavailable"
	transcript.Scenes = nil
	applyBreakPolicy(&transcript)
	for _, code := range []string{"no_verified_pause", "ai_unavailable", "unknown_context"} {
		if !hasReason(transcript.BreakCandidates[0], code) {
			t.Fatalf("missing fail-closed reason %s: %+v", code, transcript.BreakCandidates[0])
		}
	}
}

func TestBreakPolicyAppliesGapCountAndAdLoadDeterministically(t *testing.T) {
	transcript := policyFixture(1000, 60, 90, 400, 800)
	transcript.BreakCandidates[1].Naturalness = 0.95
	applyBreakPolicy(&transcript)
	if transcript.BreakCandidates[1].Decision != "accepted" || !hasReason(transcript.BreakCandidates[0], "minimum_gap") {
		t.Fatalf("nearest lower-ranked candidate should lose: %+v", transcript.BreakCandidates)
	}
	if transcript.BreakPolicy.AcceptedCount != 2 || !hasReason(transcript.BreakCandidates[3], "max_break_count") {
		t.Fatalf("hourly count was not enforced: %+v", transcript.BreakCandidates)
	}
	first := append([]BreakCandidate(nil), transcript.BreakCandidates...)
	applyBreakPolicy(&transcript)
	if !reflect.DeepEqual(first, transcript.BreakCandidates) {
		t.Fatal("same input and policy produced different decisions")
	}

	short := policyFixture(40, 20)
	applyBreakPolicy(&short)
	if !hasReason(short.BreakCandidates[0], "ad_load") {
		t.Fatalf("ad load was not enforced: %+v", short.BreakCandidates[0])
	}
}

func TestBreakCandidateContractRejectsOutOfRangeScoresAndUnorderedTimes(t *testing.T) {
	transcript := policyFixture(80, 40)
	transcript.BreakCandidates[0].DisruptionRisk = 1.2
	if err := validateBreakCandidates(transcript); err == nil {
		t.Fatal("out-of-range model score was accepted")
	}
	transcript = policyFixture(80, 40, 30)
	if err := validateBreakCandidates(transcript); err == nil {
		t.Fatal("unsorted candidate times were accepted")
	}
}
