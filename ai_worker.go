package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"path/filepath"
	"strings"
)

type TranscriptWord struct {
	Word  string  `json:"word"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

type TranscriptSegment struct {
	Text  string           `json:"text"`
	Start float64          `json:"start"`
	End   float64          `json:"end"`
	Words []TranscriptWord `json:"words,omitempty"`
}

type SceneEvidence struct {
	SceneID           string            `json:"scene_id"`
	Start             float64           `json:"start"`
	End               float64           `json:"end"`
	Summary           string            `json:"summary"`
	Activities        []string          `json:"activities"`
	Tone              []string          `json:"tone"`
	SensitiveContexts []string          `json:"sensitive_contexts"`
	DialogueState     string            `json:"dialogue_state"`
	Confidence        float64           `json:"confidence"`
	Evidence          []string          `json:"evidence"`
	ShotBoundaries    []float64         `json:"shot_boundaries"`
	ShotCount         int               `json:"shot_count,omitempty"`
	BrandMatches      []SceneBrandMatch `json:"brand_matches"`
	// Emotional intensity (0-1) and valence (-1 to 1) from the scene model; absent on older analyses.
	EmotionalIntensity *float64 `json:"emotional_intensity,omitempty"`
	Valence            *float64 `json:"valence,omitempty"`
}

// PacingMap is the emotional pacing curve on a fixed grid, with its notable moments.
type PacingMap struct {
	Version    string        `json:"version"`
	StepSec    float64       `json:"step_sec"`
	Tension    []float64     `json:"tension"`
	Components PacingParts   `json:"components"`
	Peaks      []PacingPoint `json:"peaks"`
	Valleys    []PacingPoint `json:"valleys"`
	Summary    string        `json:"summary"`
}

type PacingParts struct {
	Scene []*float64 `json:"scene"`
	Audio []float64  `json:"audio"`
	Cuts  []float64  `json:"cuts"`
}

type PacingPoint struct {
	Time        float64  `json:"time"`
	Value       float64  `json:"value"`
	Label       string   `json:"label"`
	SceneID     string   `json:"scene_id"`
	Cue         string   `json:"cue"`
	Driver      string   `json:"driver"`
	Kind        string   `json:"kind,omitempty"`
	Start       float64  `json:"start,omitempty"`
	End         float64  `json:"end,omitempty"`
	SceneChange *float64 `json:"scene_change,omitempty"`
}

type ShotTransition struct {
	Time     float64 `json:"time"`
	Kind     string  `json:"kind"`
	Strength float64 `json:"strength"`
}

type TimeInterval struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

type SceneBrandMatch struct {
	BrandID         string   `json:"brand_id"`
	DisplayName     string   `json:"display_name"`
	Category        string   `json:"category"`
	FitScore        float64  `json:"fit_score"`
	FitSource       string   `json:"fit_source,omitempty"`
	MatchedContexts []string `json:"matched_contexts"`
	Reason          string   `json:"reason"`
	BlockedContexts []string `json:"blocked_contexts"`
	Blocked         bool     `json:"blocked"`
	Recommended     bool     `json:"recommended"`
}

type TransitionEvidence struct {
	ProbeID    string  `json:"probe_id"`
	Time       float64 `json:"time"`
	Kind       string  `json:"kind"`
	Continuity string  `json:"continuity"`
	Confidence float64 `json:"confidence"`
	Evidence   string  `json:"evidence"`
}

type SilenceInterval struct {
	Start    float64 `json:"start"`
	End      float64 `json:"end"`
	Duration float64 `json:"duration"`
}

type Transcript struct {
	Language             string               `json:"language"`
	Duration             float64              `json:"duration"`
	Text                 string               `json:"text"`
	Segments             []TranscriptSegment  `json:"segments"`
	Model                string               `json:"model"`
	TimestampAdjustments int                  `json:"timestamp_adjustments"`
	Scenes               []SceneEvidence      `json:"scenes"`
	Transitions          []TransitionEvidence `json:"transitions"`
	BrandCatalogVersion  string               `json:"brand_catalog_version"`
	SilenceIntervals     []SilenceInterval    `json:"silence_intervals"`
	PauseDetectionStatus string               `json:"pause_detection_status"`
	PauseDetectionError  string               `json:"pause_detection_error,omitempty"`
	SceneAnalysisStatus  string               `json:"scene_analysis_status"`
	SceneModel           string               `json:"scene_model"`
	ScenePromptVersion   string               `json:"scene_prompt_version"`
	SceneAnalysisError   string               `json:"scene_analysis_error,omitempty"`
	ShotBoundaries       []float64            `json:"shot_boundaries"`
	ShotTransitions      []ShotTransition     `json:"shot_transitions"`
	ShotCount            int                  `json:"shot_count"`
	SpeechFreeIntervals  []TimeInterval       `json:"speech_free_intervals"`
	FusionShortlistCount int                  `json:"fusion_shortlist_count"`
	UnheardSegmentCount  int                  `json:"unheard_segment_count"`
	TextSignalError      string               `json:"text_signal_error,omitempty"`
	Pipeline             map[string]string    `json:"pipeline,omitempty"`
	Pacing               *PacingMap           `json:"pacing,omitempty"`
	ShotDetectionStatus  string               `json:"shot_detection_status"`
	ShotDetectionError   string               `json:"shot_detection_error,omitempty"`
	ContentHash          string               `json:"content_hash"`
	CacheKey             string               `json:"cache_key"`
	CacheHit             bool                 `json:"cache_hit"`
	EvidenceCacheHit     bool                 `json:"evidence_cache_hit"`
	BreakModel           string               `json:"break_model"`
	BreakPromptVersion   string               `json:"break_prompt_version"`
	BreakScoringStatus   string               `json:"break_scoring_status"`
	BreakScoringError    string               `json:"break_scoring_error,omitempty"`
	BreakCandidates      []BreakCandidate     `json:"break_candidates"`
	BreakPolicy          BreakPolicyInfo      `json:"break_policy"`
}

type workerRequest struct {
	VideoPath   string `json:"video_path"`
	WorkDir     string `json:"work_dir"`
	BrandsPath  string `json:"brands_path"`
	ContentHash string `json:"content_hash"`
	// FromPhase is "" (use any cached result), "transcription" (run ASR again), or
	// "scene_analysis" (reuse Transcript and redo everything after ASR).
	FromPhase  string           `json:"from_phase,omitempty"`
	Transcript *savedTranscript `json:"transcript,omitempty"`
}

// savedTranscript is the ASR output a scene-analysis retry reuses instead of transcribing again.
type savedTranscript struct {
	Language             string              `json:"language"`
	Duration             float64             `json:"duration"`
	Text                 string              `json:"text"`
	Segments             []TranscriptSegment `json:"segments"`
	Model                string              `json:"model"`
	TimestampAdjustments int                 `json:"timestamp_adjustments"`
}

// asrOnly keeps only the transcription so the job can show it while later phases re-run.
func asrOnly(transcript *Transcript) *Transcript {
	if transcript == nil {
		return nil
	}
	return &Transcript{Language: transcript.Language, Duration: transcript.Duration, Text: transcript.Text,
		Segments: transcript.Segments, Model: transcript.Model, TimestampAdjustments: transcript.TimestampAdjustments,
		ContentHash: transcript.ContentHash, SceneAnalysisStatus: "unavailable", BreakScoringStatus: "unavailable"}
}

// progressWriter forwards "@@progress {json}" lines from the worker's stderr and keeps the rest as error text.
type progressWriter struct {
	pending  []byte
	errors   bytes.Buffer
	onUpdate func(stage string, progress int, message string)
}

func (p *progressWriter) Write(data []byte) (int, error) {
	p.pending = append(p.pending, data...)
	for {
		index := bytes.IndexByte(p.pending, '\n')
		if index < 0 {
			break
		}
		p.handle(string(p.pending[:index]))
		p.pending = p.pending[index+1:]
	}
	return len(data), nil
}

func (p *progressWriter) handle(line string) {
	if payload, ok := strings.CutPrefix(line, "@@progress "); ok {
		var update struct {
			Stage    string `json:"stage"`
			Progress int    `json:"progress"`
			Message  string `json:"message"`
		}
		if json.Unmarshal([]byte(payload), &update) == nil && p.onUpdate != nil && update.Stage != "" &&
			update.Progress >= 0 && update.Progress <= 100 && len(update.Message) <= 300 {
			p.onUpdate(update.Stage, update.Progress, update.Message)
		}
		return
	}
	p.errors.WriteString(line + "\n")
}

func runAIWorker(ctx context.Context, pythonBin, workerPath string, request workerRequest,
	onProgress func(stage string, progress int, message string)) (Transcript, error) {
	input, err := json.Marshal(request)
	if err != nil {
		return Transcript{}, errors.New("could not prepare AI worker input")
	}
	command := exec.CommandContext(ctx, pythonBin, workerPath)
	command.Stdin = bytes.NewReader(input)
	var stdout bytes.Buffer
	stderr := &progressWriter{onUpdate: onProgress}
	command.Stdout = &stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return Transcript{}, errors.New("AI analysis timed out")
		}
		if len(stderr.pending) > 0 {
			stderr.handle(string(stderr.pending))
		}
		message := strings.TrimSpace(stderr.errors.String())
		if lines := strings.Split(message, "\n"); len(lines) > 3 {
			message = strings.Join(lines[len(lines)-3:], "\n")
		}
		if message == "" {
			return Transcript{}, errors.New("AI worker could not complete transcription")
		}
		if len(message) > 500 {
			message = message[:500]
		}
		return Transcript{}, fmt.Errorf("%s", message)
	}
	var transcript Transcript
	if err := json.Unmarshal(stdout.Bytes(), &transcript); err != nil {
		return Transcript{}, errors.New("AI worker returned invalid transcript JSON")
	}
	if err := validateTranscriptGo(transcript); err != nil {
		return Transcript{}, err
	}
	applyBreakPolicy(&transcript)
	return transcript, nil
}

func validateTranscriptGo(transcript Transcript) error {
	if transcript.Duration < 0 {
		return errors.New("AI worker returned an invalid transcript duration")
	}
	previousEnd := 0.0
	for _, segment := range transcript.Segments {
		if segment.Start < 0 || segment.End < segment.Start || segment.Start+0.05 < previousEnd {
			return errors.New("AI worker returned invalid segment timestamps")
		}
		if transcript.Duration > 0 && segment.End > transcript.Duration+2 {
			return errors.New("AI worker timestamps exceed the audio duration")
		}
		previousEnd = segment.End
	}
	previousSceneStart := -1.0
	for _, scene := range transcript.Scenes {
		if scene.SceneID == "" || scene.Summary == "" || scene.Start < 0 || scene.End <= scene.Start ||
			scene.Start < previousSceneStart || scene.Confidence < 0 || scene.Confidence > 1 {
			return errors.New("AI worker returned invalid scene evidence")
		}
		if transcript.Duration > 0 && scene.End > transcript.Duration+1 {
			return errors.New("AI worker returned scene evidence outside the media duration")
		}
		previousCut := scene.Start - 1
		for _, cut := range scene.ShotBoundaries {
			if cut < scene.Start || cut > scene.End || cut <= previousCut {
				return errors.New("AI worker returned invalid scene shot-cut evidence")
			}
			previousCut = cut
		}
		if err := validateBrandMatches(scene.BrandMatches); err != nil {
			return err
		}
		previousSceneStart = scene.Start
	}
	previousCut := -1.0
	for _, cut := range transcript.ShotBoundaries {
		if cut <= 0 || (transcript.Duration > 0 && cut >= transcript.Duration) || cut <= previousCut {
			return errors.New("AI worker returned invalid shot-cut timestamps")
		}
		previousCut = cut
	}
	if err := validatePacing(transcript); err != nil {
		return err
	}
	previousShot := 0.0
	validShotKinds := map[string]bool{"cut": true, "fade": true, "dissolve": true}
	for _, shot := range transcript.ShotTransitions {
		if shot.Time <= previousShot || (transcript.Duration > 0 && shot.Time >= transcript.Duration) ||
			!validShotKinds[shot.Kind] || !unitInterval(shot.Strength) {
			return errors.New("AI worker returned invalid shot transition evidence")
		}
		previousShot = shot.Time
	}
	previousTransition := -1.0
	seenProbes := make(map[string]bool, len(transcript.Transitions))
	validTransitionKinds := map[string]bool{"camera_only": true, "setting_change": true, "activity_change": true, "time_or_story_change": true, "unclear": true}
	validContinuities := map[string]bool{"same_scene": true, "new_scene": true, "uncertain": true}
	for _, transition := range transcript.Transitions {
		if transition.ProbeID == "" || seenProbes[transition.ProbeID] || transition.Time <= previousTransition ||
			transition.Time <= 0 || transition.Time >= transcript.Duration || math.IsNaN(transition.Time) ||
			transition.Confidence < 0 || transition.Confidence > 1 || math.IsNaN(transition.Confidence) ||
			!validTransitionKinds[transition.Kind] || !validContinuities[transition.Continuity] || strings.TrimSpace(transition.Evidence) == "" {
			return errors.New("AI worker returned invalid visual transition evidence")
		}
		seenProbes[transition.ProbeID] = true
		previousTransition = transition.Time
	}
	previousPauseEnd := 0.0
	for _, pause := range transcript.SilenceIntervals {
		if pause.Start < 0 || pause.End <= pause.Start || pause.Start < previousPauseEnd ||
			pause.Duration < 0 || pause.Duration > pause.End-pause.Start+0.05 {
			return errors.New("AI worker returned invalid pause evidence")
		}
		if transcript.Duration > 0 && pause.End > transcript.Duration+0.1 {
			return errors.New("AI worker returned pause evidence outside the media duration")
		}
		previousPauseEnd = pause.End
	}
	if err := validateBreakCandidates(transcript); err != nil {
		return err
	}
	return nil
}

func validatePacing(transcript Transcript) error {
	for _, scene := range transcript.Scenes {
		if (scene.EmotionalIntensity != nil && !unitInterval(*scene.EmotionalIntensity)) ||
			(scene.Valence != nil && (math.IsNaN(*scene.Valence) || *scene.Valence < -1 || *scene.Valence > 1)) {
			return errors.New("AI worker returned scene emotion outside its range")
		}
	}
	pacing := transcript.Pacing
	if pacing == nil {
		return nil
	}
	if pacing.StepSec <= 0 || math.IsNaN(pacing.StepSec) || !unitInterval(pacing.Tension...) || !unitInterval(pacing.Components.Audio...) ||
		!unitInterval(pacing.Components.Cuts...) || float64(len(pacing.Tension)) > transcript.Duration/pacing.StepSec+2 {
		return errors.New("AI worker returned an invalid pacing curve")
	}
	for _, value := range pacing.Components.Scene {
		if value != nil && !unitInterval(*value) {
			return errors.New("AI worker returned an invalid pacing curve")
		}
	}
	for _, point := range append(append([]PacingPoint{}, pacing.Peaks...), pacing.Valleys...) {
		if point.Time < 0 || point.Time > transcript.Duration+pacing.StepSec || !unitInterval(point.Value) {
			return errors.New("AI worker returned pacing moments outside the programme")
		}
	}
	return nil
}

func validateBrandMatches(matches []SceneBrandMatch) error {
	seen := make(map[string]bool, len(matches))
	for _, match := range matches {
		if match.BrandID == "" || seen[match.BrandID] || match.DisplayName == "" || match.Category == "" ||
			match.FitScore < 0 || match.FitScore > 1 || math.IsNaN(match.FitScore) || strings.TrimSpace(match.Reason) == "" ||
			(match.Blocked && match.Recommended) {
			return errors.New("AI worker returned invalid or unsafe brand fit evidence")
		}
		seen[match.BrandID] = true
	}
	return nil
}

func (s *server) transcribeJob(id, fromPhase string) {
	s.analysisSlots <- struct{}{}
	defer func() { <-s.analysisSlots }()

	s.jobsMu.Lock()
	job, exists := s.jobs[id]
	if !exists || job.Status != "queued" {
		s.jobsMu.Unlock()
		return
	}
	job.Status, job.Stage, job.Progress = "processing", "transcribing_bengali_speech", 28
	job.Message = "Preparing audio and transcribing Bengali speech with timestamps."
	request := workerRequest{VideoPath: s.uploadPath(id), WorkDir: s.uploadDir, BrandsPath: s.brandsPath,
		ContentHash: job.ContentHash, FromPhase: fromPhase}
	// A transcript with no segments is valid: the programme may have no dialogue.
	if fromPhase != "transcription" && job.Transcript != nil && job.Transcript.Model != "" {
		saved := job.Transcript
		request.Transcript = &savedTranscript{Language: saved.Language, Duration: saved.Duration, Text: saved.Text,
			Segments: saved.Segments, Model: saved.Model, TimestampAdjustments: saved.TimestampAdjustments}
	}
	if fromPhase == "scene_analysis" {
		job.Stage, job.Progress, job.Message = "detecting_shots", 34, "Re-running scene analysis with the saved transcript."
	}
	s.jobs[id] = job
	if err := s.persistJob(job); err != nil {
		s.logger.Error("persist running job state", "job_id", id, "error", err)
	}
	s.jobsMu.Unlock()
	s.observeStage(id, job.Stage)

	ctx, cancel := context.WithTimeout(context.Background(), analysisTimeout)
	defer cancel()
	transcript, err := runAIWorker(ctx, s.pythonBin, s.workerPath, request,
		func(stage string, progress int, message string) { s.updateJobProgress(id, stage, progress, message) })
	s.jobsMu.Lock()
	job, exists = s.jobs[id]
	if !exists {
		s.jobsMu.Unlock()
		return
	}
	if err != nil {
		job.Status, job.Stage, job.Progress = "failed", "transcription_failed", 28
		job.Message = "Bengali transcription did not complete: " + err.Error()
		s.logger.Warn("transcription failed", "job_id", id, "error", err)
	} else {
		job.Status, job.Stage, job.Progress = "completed", "transcription_complete", 55
		job.Message = "Bengali speech transcription is ready. Scene understanding is next."
		if transcript.SceneAnalysisStatus == "complete" {
			job.Stage, job.Progress = "scene_evidence_complete", 70
			job.Message = "Bengali transcript, low-audio pauses, and AI scene evidence are ready."
			if transcript.BreakScoringStatus == "complete" {
				job.Stage, job.Progress = "break_policy_complete", 80
				job.Message = fmt.Sprintf("AI break scoring and safety policy are ready: %d safe break(s).", transcript.BreakPolicy.AcceptedCount)
			}
			if transcript.PauseDetectionError != "" {
				job.Message = "Bengali transcript and AI scene evidence are ready. " + transcript.PauseDetectionError
			}
		} else if transcript.SceneAnalysisError != "" {
			job.Message = "The transcript is saved, but scene analysis did not finish: " + transcript.SceneAnalysisError +
				" Retry scene analysis to continue without transcribing again."
		}
		job.Transcript = &transcript
		s.logger.Info("AI evidence complete", "job_id", id, "segments", len(transcript.Segments),
			"scenes", len(transcript.Scenes), "pauses", len(transcript.SilenceIntervals),
			"scene_analysis", transcript.SceneAnalysisStatus, "language", transcript.Language)
	}
	s.jobs[id] = job
	if err := s.persistJob(job); err != nil {
		s.logger.Error("persist final job state", "job_id", id, "error", err)
	}
	s.jobsMu.Unlock()
	if err != nil {
		s.observeFinish(id, "error")
	} else {
		s.observeFinish(id, "ok")
	}
}

// updateJobProgress records the worker's current stage so the dashboard can show it while analysis runs.
func (s *server) updateJobProgress(id, stage string, progress int, message string) {
	s.jobsMu.Lock()
	job, exists := s.jobs[id]
	if !exists || job.Status != "processing" {
		s.jobsMu.Unlock()
		return
	}
	changed := job.Stage != stage
	job.Stage, job.Progress, job.Message = stage, progress, message
	s.jobs[id] = job
	s.jobsMu.Unlock()
	if changed {
		s.observeStage(id, stage)
	}
}

func (s *server) uploadPath(id string) string {
	return filepath.Join(s.uploadDir, id+".mp4")
}
