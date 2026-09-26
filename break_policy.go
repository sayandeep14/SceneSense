package main

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

const (
	breakPolicyVersion  = "break-policy-v2-scene-fusion"
	minLeadSeconds      = 15.0
	minTailSeconds      = 10.0
	minBreakGap         = 300.0
	contextGuard        = 8.0
	plannedAdSeconds    = 15.0
	maxAdLoad           = 0.20
	maxBreaksPerHour    = 8
	maxBreakCandidates  = 64
	speechBlock         = 0.5
	tensionBlock        = 0.7
	minJudgeConfidence  = 0.5
	minSceneConfidence  = 0.65
	spacingPenalty      = 0.25
	manualPlacementLoad = 0.6
)

var validTiers = map[string]bool{"High": true, "Medium": true, "Low": true}

type AudioChange struct {
	Shift            float64 `json:"shift"`
	LoudnessChangeDB float64 `json:"loudness_change_db"`
	Before           string  `json:"before"`
	After            string  `json:"after"`
	MusicChange      float64 `json:"music_change"`
}

type BreakCandidate struct {
	CandidateID                string             `json:"candidate_id"`
	Time                       float64            `json:"time"`
	Signals                    []string           `json:"signals"`
	Evidence                   []string           `json:"evidence"`
	BeforeText                 string             `json:"before_text"`
	AfterText                  string             `json:"after_text"`
	TimingQuality              string             `json:"timing_quality,omitempty"`
	SceneContext               string             `json:"scene_context"`
	PrecedingSceneContext      string             `json:"preceding_scene_context,omitempty"`
	PrecedingSceneMood         string             `json:"preceding_scene_mood,omitempty"`
	PrecedingSensitiveContexts []string           `json:"preceding_sensitive_contexts,omitempty"`
	TransitionKind             string             `json:"transition_kind,omitempty"`
	TransitionEvidence         string             `json:"transition_evidence,omitempty"`
	BrandRecommendations       []SceneBrandMatch  `json:"brand_recommendations,omitempty"`
	BlockedBrandMatches        []SceneBrandMatch  `json:"blocked_brand_matches,omitempty"`
	Continuity                 string             `json:"continuity,omitempty"`
	SceneChange                bool               `json:"scene_change"`
	FromContext                string             `json:"from_context,omitempty"`
	ToContext                  string             `json:"to_context,omitempty"`
	TopicShift                 float64            `json:"topic_shift"`
	Tension                    float64            `json:"tension"`
	DialogueComplete           bool               `json:"dialogue_complete"`
	SceneScore                 float64            `json:"scene_score"`
	SignalScores               map[string]float64 `json:"signal_scores,omitempty"`
	ShotTransition             string             `json:"shot_transition,omitempty"`
	PauseSeconds               float64            `json:"pause_seconds"`
	PauseSource                string             `json:"pause_source,omitempty"`
	SpeechAtCut                float64            `json:"speech_at_cut"`
	AudioChange                *AudioChange       `json:"audio_change,omitempty"`
	AdScore                    float64            `json:"ad_score"`
	Tier                       string             `json:"tier,omitempty"`
	Rationale                  string             `json:"rationale,omitempty"`
	Naturalness                float64            `json:"naturalness"`
	DisruptionRisk             float64            `json:"disruption_risk"`
	Confidence                 float64            `json:"confidence"`
	AIReason                   string             `json:"ai_reason"`
	AIModel                    string             `json:"ai_model"`
	AIPromptVer                string             `json:"ai_prompt_version"`
	Decision                   string             `json:"decision"`
	Potential                  bool               `json:"potential"`
	SelectionNote              string             `json:"selection_note,omitempty"`
	Reasons                    []PolicyReason     `json:"reasons"`
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
	MaxBreakCount    int     `json:"max_break_count"`
	ShotCount        int     `json:"shot_count"`
	SceneChangeCount int     `json:"scene_change_count"`
	EligibleCount    int     `json:"eligible_count"`
	AutoBreakCount   int     `json:"auto_break_count"`
	Optimizer        string  `json:"optimizer"`
	Summary          string  `json:"summary,omitempty"`
}

func unitInterval(values ...float64) bool {
	for _, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1 {
			return false
		}
	}
	return true
}

func validateBreakCandidates(transcript Transcript) error {
	if len(transcript.BreakCandidates) > maxBreakCandidates {
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
		if !unitInterval(candidate.Naturalness, candidate.DisruptionRisk, candidate.Confidence, candidate.Tension,
			candidate.TopicShift, candidate.SceneScore, candidate.SpeechAtCut, candidate.AdScore) ||
			candidate.PauseSeconds < 0 || math.IsNaN(candidate.PauseSeconds) {
			return errors.New("AI worker returned invalid break scores")
		}
		for _, score := range candidate.SignalScores {
			if !unitInterval(score) {
				return errors.New("AI worker returned invalid break signal scores")
			}
		}
		if candidate.Tier != "" && !validTiers[candidate.Tier] {
			return errors.New("AI worker returned an unknown confidence tier")
		}
		if transcript.BreakScoringStatus == "complete" &&
			(candidate.AIReason == "" || candidate.AIModel == "" || candidate.AIPromptVer == "" || candidate.Tier == "") {
			return errors.New("AI worker omitted a candidate score explanation")
		}
		if err := validateBrandMatches(candidate.BrandRecommendations); err != nil {
			return err
		}
		if err := validateBrandMatches(candidate.BlockedBrandMatches); err != nil {
			return err
		}
		for _, match := range candidate.BrandRecommendations {
			if match.Blocked || !match.Recommended {
				return errors.New("AI worker recommended a brand blocked by a negative context")
			}
		}
		for _, match := range candidate.BlockedBrandMatches {
			if !match.Blocked {
				return errors.New("AI worker marked a safe brand as blocked")
			}
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

// maxBreakCount bounds k by breaks per hour, planned ad load, and how many minimum gaps fit.
func maxBreakCount(duration float64) int {
	return maxBreakCountWithLimits(duration, minBreakGap, maxBreaksPerHour, maxAdLoad)
}

func maxBreakCountWithLimits(duration, gap float64, hourly int, load float64) int {
	usable := duration - minLeadSeconds - minTailSeconds
	if usable < 0 {
		return 0
	}
	count := int(math.Ceil(duration / 3600 * float64(hourly)))
	if byLoad := int(math.Floor(load * duration / ((1 - load) * plannedAdSeconds))); byLoad < count {
		count = byLoad
	}
	if byGap := int(math.Floor(usable/gap)) + 1; byGap < count {
		count = byGap
	}
	return max(count, 0)
}

func candidateEvidenceReasons(transcript *Transcript, candidate *BreakCandidate) {
	at := candidate.Time
	if transcript.SceneAnalysisStatus != "complete" {
		addReason(candidate, "scene_unavailable", "Scene context is unavailable.")
	}
	if transcript.BreakScoringStatus != "complete" {
		addReason(candidate, "ai_unavailable", "AI boundary judgement is unavailable.")
	}
	if !candidate.SceneChange {
		addReason(candidate, "same_scene", "Camera change inside the same scene, not a context switch.")
	}
	if at < minLeadSeconds || at > transcript.Duration-minTailSeconds {
		addReason(candidate, "edge_guard", "The programme needs a clear opening and ending.")
	}
	if candidate.SpeechAtCut >= speechBlock {
		addReason(candidate, "active_speech", "Speech is detected across the cut.")
	}
	for _, segment := range transcript.Segments {
		for _, word := range segment.Words {
			if word.Start < at-0.05 && word.End > at+0.05 {
				addReason(candidate, "active_speech", "A spoken word overlaps the cut.")
			}
		}
	}
	if candidate.Tension >= tensionBlock {
		addReason(candidate, "high_tension", "A tense or dramatic moment; an ad here would break the story.")
	}
	if transcript.BreakScoringStatus == "complete" && !candidate.DialogueComplete {
		addReason(candidate, "unfinished_dialogue", "The spoken thought continues across the cut.")
	}
	if candidate.Continuity == "uncertain" || (transcript.BreakScoringStatus == "complete" && candidate.Confidence < minJudgeConfidence) {
		addReason(candidate, "uncertain_context", "The model is not confident the scene changes here.")
	}
	if len(candidate.PrecedingSensitiveContexts) > 0 {
		addReason(candidate, "sensitive_context", "Sensitive context next to the break: "+strings.Join(candidate.PrecedingSensitiveContexts, ", ")+".")
	}
	leftCovered, rightCovered := false, false
	for _, scene := range transcript.Scenes {
		if scene.Start <= at-0.4 && scene.End >= at-0.4 {
			leftCovered = true
		}
		if scene.Start <= at+0.4 && scene.End >= at+0.4 {
			rightCovered = true
		}
		if scene.End < at-contextGuard || scene.Start > at+contextGuard {
			continue
		}
		if scene.Confidence < minSceneConfidence || scene.DialogueState == "unclear" {
			addReason(candidate, "uncertain_context", "Nearby scene context is uncertain.")
		}
		if len(scene.SensitiveContexts) > 0 {
			addReason(candidate, "sensitive_context", "A sensitive scene is adjacent to the proposed break.")
		}
	}
	if !leftCovered || !rightCovered {
		addReason(candidate, "unknown_context", "Scene evidence does not cover both sides of the break.")
	}
}

func applyBreakPolicy(transcript *Transcript) {
	transcript.BreakPolicy = BreakPolicyInfo{
		Version: breakPolicyVersion, MinGapSeconds: minBreakGap, MaxBreaksPerHour: maxBreaksPerHour,
		MaxAdLoadPercent: maxAdLoad * 100, PlannedAdSeconds: plannedAdSeconds,
		MaxBreakCount: maxBreakCount(transcript.Duration), ShotCount: len(transcript.ShotBoundaries),
		Optimizer: optimizerVersion,
	}
	for index := range transcript.BreakCandidates {
		candidate := &transcript.BreakCandidates[index]
		candidate.Decision, candidate.Reasons, candidate.SelectionNote = "rejected", nil, ""
		candidateEvidenceReasons(transcript, candidate)
		candidate.Potential = len(candidate.Reasons) == 0
		if candidate.SceneChange {
			transcript.BreakPolicy.SceneChangeCount++
		}
		if candidate.Potential && candidate.Tier != "Low" {
			transcript.BreakPolicy.EligibleCount++
		}
	}
	result, _ := optimizeBreaks(transcript, OptimizeRequest{})
	notes := make(map[string]CandidateOutcome, len(result.Outcomes))
	for _, outcome := range result.Outcomes {
		notes[outcome.CandidateID] = outcome
	}
	for index := range transcript.BreakCandidates {
		candidate := &transcript.BreakCandidates[index]
		outcome := notes[candidate.CandidateID]
		candidate.SelectionNote = outcome.Note
		if outcome.Status == "selected" {
			candidate.Decision = "accepted"
		}
	}
	transcript.BreakPolicy.AcceptedCount = len(result.Selected)
	transcript.BreakPolicy.AutoBreakCount = result.AutoK
	transcript.BreakPolicy.Summary = result.Message
}

func clockLabel(seconds float64) string {
	whole := int(math.Max(0, seconds))
	if whole >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", whole/3600, whole%3600/60, whole%60)
	}
	return fmt.Sprintf("%d:%02d", whole/60, whole%60)
}
