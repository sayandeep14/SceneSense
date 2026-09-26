package main

import (
	"errors"
	"math"
	"sort"
	"strings"
)

const (
	breakPolicyVersion = "break-policy-v1"
	minLeadSeconds     = 15.0
	minTailSeconds     = 10.0
	minBreakGap        = 180.0
	contextGuard       = 8.0
	plannedAdSeconds   = 15.0
	maxAdLoad          = 0.20
	maxBreaksPerHour   = 4
)

type BreakCandidate struct {
	CandidateID    string         `json:"candidate_id"`
	Time           float64        `json:"time"`
	Signals        []string       `json:"signals"`
	Evidence       []string       `json:"evidence"`
	BeforeText     string         `json:"before_text"`
	AfterText      string         `json:"after_text"`
	SceneContext   string         `json:"scene_context"`
	Naturalness    float64        `json:"naturalness"`
	DisruptionRisk float64        `json:"disruption_risk"`
	Confidence     float64        `json:"confidence"`
	AIReason       string         `json:"ai_reason"`
	AIModel        string         `json:"ai_model"`
	AIPromptVer    string         `json:"ai_prompt_version"`
	Decision       string         `json:"decision"`
	Reasons        []PolicyReason `json:"reasons"`
}

type PolicyReason struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type BreakPolicyInfo struct {
	Version          string  `json:"version"`
	MinGapSeconds    float64 `json:"min_gap_seconds"`
	MaxBreaksPerHour int     `json:"max_breaks_per_hour"`
	MaxAdLoadPercent float64 `json:"max_ad_load_percent"`
	PlannedAdSeconds float64 `json:"planned_ad_seconds"`
	AcceptedCount    int     `json:"accepted_count"`
}

func validateBreakCandidates(transcript Transcript) error {
	if len(transcript.BreakCandidates) > 32 {
		return errors.New("AI worker returned too many break candidates")
	}
	if transcript.BreakScoringStatus == "complete" && (transcript.BreakModel == "" || transcript.BreakPromptVersion == "") {
		return errors.New("AI worker omitted break scoring provenance")
	}
	previous := 0.0
	seen := make(map[string]bool, len(transcript.BreakCandidates))
	for _, candidate := range transcript.BreakCandidates {
		if candidate.CandidateID == "" || seen[candidate.CandidateID] || candidate.Time <= previous ||
			candidate.Time <= 0 || candidate.Time >= transcript.Duration || math.IsNaN(candidate.Time) ||
			len(candidate.Signals) == 0 {
			return errors.New("AI worker returned invalid break candidate times or IDs")
		}
		for _, score := range []float64{candidate.Naturalness, candidate.DisruptionRisk, candidate.Confidence} {
			if math.IsNaN(score) || math.IsInf(score, 0) || score < 0 || score > 1 {
				return errors.New("AI worker returned invalid break scores")
			}
		}
		if transcript.BreakScoringStatus == "complete" &&
			(candidate.AIReason == "" || candidate.AIModel == "" || candidate.AIPromptVer == "") {
			return errors.New("AI worker omitted a candidate score explanation")
		}
		seen[candidate.CandidateID] = true
		previous = candidate.Time
	}
	return nil
}

func addReason(candidate *BreakCandidate, code, message string) {
	for _, existing := range candidate.Reasons {
		if existing.Code == code {
			return
		}
	}
	candidate.Reasons = append(candidate.Reasons, PolicyReason{Code: code, Message: message})
}

func terminalSentence(text string) bool {
	runes := []rune(strings.TrimSpace(text))
	return len(runes) > 0 && strings.ContainsRune("।.!?？", runes[len(runes)-1])
}

func completedSceneBoundary(transcript *Transcript, candidate *BreakCandidate) bool {
	hasSceneEndSignal := false
	for _, signal := range candidate.Signals {
		if signal == "scene_end" {
			hasSceneEndSignal = true
			break
		}
	}
	if !hasSceneEndSignal {
		return false
	}
	for _, scene := range transcript.Scenes {
		if math.Abs(scene.End-candidate.Time) <= 1.2 && scene.DialogueState == "completed_thought" {
			return true
		}
	}
	return false
}

func candidateEvidenceReasons(transcript *Transcript, candidate *BreakCandidate) {
	time := candidate.Time
	if transcript.SceneAnalysisStatus != "complete" {
		addReason(candidate, "scene_unavailable", "Scene context is unavailable.")
	}
	if transcript.BreakScoringStatus != "complete" {
		addReason(candidate, "ai_unavailable", "AI naturalness scoring is unavailable.")
	} else {
		if candidate.Confidence < 0.65 {
			addReason(candidate, "low_ai_confidence", "AI confidence is below 65%.")
		}
		if candidate.Naturalness < 0.70 {
			addReason(candidate, "low_naturalness", "AI naturalness is below 70%.")
		}
		if candidate.DisruptionRisk > 0.40 {
			addReason(candidate, "high_disruption", "AI disruption risk exceeds 40%.")
		}
	}
	if time < minLeadSeconds || time > transcript.Duration-minTailSeconds {
		addReason(candidate, "edge_guard", "The programme needs a clear opening and ending.")
	}

	pauseFound := false
	for _, pause := range transcript.SilenceIntervals {
		if pause.Duration >= 0.55 && pause.Start+0.15 <= time && time <= pause.End-0.15 {
			pauseFound = true
			break
		}
	}
	if !pauseFound {
		addReason(candidate, "no_verified_pause", "No verified low-audio pause covers this moment.")
	}

	var prior *TranscriptSegment
	for index := range transcript.Segments {
		segment := &transcript.Segments[index]
		if segment.Start < time+0.1 && segment.End > time-0.1 {
			if segment.End-segment.Start <= 6 || len(segment.Words) > 0 {
				addReason(candidate, "active_speech", "A spoken phrase overlaps the proposed cut.")
			} else if !pauseFound || !completedSceneBoundary(transcript, candidate) {
				addReason(candidate, "coarse_asr_timing", "Broad transcript timing requires a verified pause at a completed scene boundary.")
			}
			break
		}
		if segment.End <= time+0.1 {
			prior = segment
		}
	}

	leftCovered, rightCovered := false, false
	for _, scene := range transcript.Scenes {
		if scene.Start <= time-0.4 && scene.End >= time-0.4 {
			leftCovered = true
		}
		if scene.Start <= time+0.4 && scene.End >= time+0.4 {
			rightCovered = true
		}
		if scene.End < time-contextGuard || scene.Start > time+contextGuard {
			continue
		}
		if scene.Confidence < 0.65 || scene.DialogueState == "unclear" {
			addReason(candidate, "uncertain_context", "Nearby scene context is uncertain.")
		}
		if scene.DialogueState == "ongoing" && scene.Start <= time && scene.End >= time {
			addReason(candidate, "ongoing_dialogue", "The scene still has an ongoing spoken thought.")
		}
		if len(scene.SensitiveContexts) > 0 {
			addReason(candidate, "sensitive_context", "A sensitive scene is adjacent to the proposed break.")
		}
	}
	if !leftCovered || !rightCovered {
		addReason(candidate, "unknown_context", "Scene evidence does not cover both sides of the break.")
	}
	if prior != nil && time-prior.End < 3 && !terminalSentence(prior.Text) {
		addReason(candidate, "unfinished_sentence", "The previous spoken thought may be unfinished.")
	}
}

func applyBreakPolicy(transcript *Transcript) {
	transcript.BreakPolicy = BreakPolicyInfo{
		Version: breakPolicyVersion, MinGapSeconds: minBreakGap,
		MaxBreaksPerHour: maxBreaksPerHour, MaxAdLoadPercent: maxAdLoad * 100,
		PlannedAdSeconds: plannedAdSeconds,
	}
	eligible := make([]int, 0, len(transcript.BreakCandidates))
	for index := range transcript.BreakCandidates {
		candidate := &transcript.BreakCandidates[index]
		candidate.Decision, candidate.Reasons = "rejected", nil
		candidateEvidenceReasons(transcript, candidate)
		if len(candidate.Reasons) == 0 {
			eligible = append(eligible, index)
		}
	}
	// The highest quality eligible moment wins; ties resolve by earlier time.
	sort.Slice(eligible, func(left, right int) bool {
		a := transcript.BreakCandidates[eligible[left]]
		b := transcript.BreakCandidates[eligible[right]]
		qualityA, qualityB := a.Naturalness-a.DisruptionRisk, b.Naturalness-b.DisruptionRisk
		if qualityA != qualityB {
			return qualityA > qualityB
		}
		if a.Confidence != b.Confidence {
			return a.Confidence > b.Confidence
		}
		return a.Time < b.Time
	})
	maxCount := int(math.Ceil(transcript.Duration / 3600 * maxBreaksPerHour))
	if maxCount < 1 {
		maxCount = 1
	}
	selected := make([]float64, 0, maxCount)
	for _, index := range eligible {
		candidate := &transcript.BreakCandidates[index]
		if len(selected) >= maxCount {
			addReason(candidate, "max_break_count", "The programme's maximum break count is reached.")
			continue
		}
		if float64(len(selected)+1)*plannedAdSeconds/(transcript.Duration+float64(len(selected)+1)*plannedAdSeconds) > maxAdLoad {
			addReason(candidate, "ad_load", "Adding this ad would exceed the 20% ad-load limit.")
			continue
		}
		tooClose := false
		for _, prior := range selected {
			if math.Abs(candidate.Time-prior) < minBreakGap {
				tooClose = true
				break
			}
		}
		if tooClose {
			addReason(candidate, "minimum_gap", "Another selected break is within 180 seconds.")
			continue
		}
		candidate.Decision = "accepted"
		selected = append(selected, candidate.Time)
	}
	transcript.BreakPolicy.AcceptedCount = len(selected)
}
