package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
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
	SceneID           string    `json:"scene_id"`
	Start             float64   `json:"start"`
	End               float64   `json:"end"`
	Summary           string    `json:"summary"`
	Activities        []string  `json:"activities"`
	Tone              []string  `json:"tone"`
	SensitiveContexts []string  `json:"sensitive_contexts"`
	DialogueState     string    `json:"dialogue_state"`
	Confidence        float64   `json:"confidence"`
	Evidence          []string  `json:"evidence"`
	ShotBoundaries    []float64 `json:"shot_boundaries"`
}

type SilenceInterval struct {
	Start    float64 `json:"start"`
	End      float64 `json:"end"`
	Duration float64 `json:"duration"`
}

type Transcript struct {
	Language             string              `json:"language"`
	Duration             float64             `json:"duration"`
	Text                 string              `json:"text"`
	Segments             []TranscriptSegment `json:"segments"`
	Model                string              `json:"model"`
	TimestampAdjustments int                 `json:"timestamp_adjustments"`
	Scenes               []SceneEvidence     `json:"scenes"`
	SilenceIntervals     []SilenceInterval   `json:"silence_intervals"`
	PauseDetectionStatus string              `json:"pause_detection_status"`
	PauseDetectionError  string              `json:"pause_detection_error,omitempty"`
	SceneAnalysisStatus  string              `json:"scene_analysis_status"`
	SceneModel           string              `json:"scene_model"`
	ScenePromptVersion   string              `json:"scene_prompt_version"`
	SceneAnalysisError   string              `json:"scene_analysis_error,omitempty"`
	ShotBoundaries       []float64           `json:"shot_boundaries"`
	ShotDetectionStatus  string              `json:"shot_detection_status"`
	ShotDetectionError   string              `json:"shot_detection_error,omitempty"`
	ContentHash          string              `json:"content_hash"`
	CacheKey             string              `json:"cache_key"`
	CacheHit             bool                `json:"cache_hit"`
}

type workerRequest struct {
	VideoPath   string `json:"video_path"`
	WorkDir     string `json:"work_dir"`
	BrandsPath  string `json:"brands_path"`
	ContentHash string `json:"content_hash"`
}

func runAIWorker(ctx context.Context, pythonBin, workerPath, videoPath, workDir, brandsPath, contentHash string) (Transcript, error) {
	input, err := json.Marshal(workerRequest{VideoPath: videoPath, WorkDir: workDir, BrandsPath: brandsPath, ContentHash: contentHash})
	if err != nil {
		return Transcript{}, errors.New("could not prepare AI worker input")
	}
	command := exec.CommandContext(ctx, pythonBin, workerPath)
	command.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return Transcript{}, errors.New("AI analysis timed out")
		}
		message := strings.TrimSpace(stderr.String())
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
		previousSceneStart = scene.Start
	}
	previousCut := -1.0
	for _, cut := range transcript.ShotBoundaries {
		if cut <= 0 || (transcript.Duration > 0 && cut >= transcript.Duration) || cut <= previousCut {
			return errors.New("AI worker returned invalid shot-cut timestamps")
		}
		previousCut = cut
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
	return nil
}

func (s *server) transcribeJob(id string) {
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
	s.jobs[id] = job
	if err := s.persistJob(job); err != nil {
		s.logger.Error("persist running job state", "job_id", id, "error", err)
	}
	s.jobsMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	transcript, err := runAIWorker(ctx, s.pythonBin, s.workerPath, s.uploadPath(id), s.uploadDir, s.brandsPath, job.ContentHash)
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	job, exists = s.jobs[id]
	if !exists {
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
			if transcript.PauseDetectionError != "" {
				job.Message = "Bengali transcript and AI scene evidence are ready. " + transcript.PauseDetectionError
			}
		} else if transcript.SceneAnalysisError != "" {
			job.Message += " Scene analysis is unavailable: " + transcript.SceneAnalysisError
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
}

func (s *server) uploadPath(id string) string {
	return filepath.Join(s.uploadDir, id+".mp4")
}
