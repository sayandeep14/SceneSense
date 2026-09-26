package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"time"
)

const (
	manifestVersion   = "scenesense.ad-manifest/1.0"
	manifestSchemaURL = "/schema/ad-manifest-v1.json"
	maxReviewBreaks   = 32
)

// ReviewState is the reviewer's working plan for a job, saved on the server so it survives across browsers.
type ReviewState struct {
	Target            int                 `json:"target"`
	Selected          []PlaybackSelection `json:"selected"`
	ExcludedAI        []string            `json:"excluded_ai"`
	UpdatedAt         time.Time           `json:"updated_at"`
	FinalizedRevision int                 `json:"finalized_revision"`
	Dirty             bool                `json:"dirty"`
}

type ManifestRecord struct {
	Revision    int             `json:"revision"`
	FinalizedAt time.Time       `json:"finalized_at"`
	BreakCount  int             `json:"break_count"`
	Manifest    json.RawMessage `json:"manifest"`
}

type AdManifest struct {
	Schema      string            `json:"$schema"`
	Version     string            `json:"manifest_version"`
	Revision    int               `json:"revision"`
	Status      string            `json:"status"`
	GeneratedAt time.Time         `json:"generated_at"`
	FinalizedBy string            `json:"finalized_by"`
	Programme   ManifestProgramme `json:"programme"`
	Policy      ManifestPolicy    `json:"policy"`
	Analysis    ManifestAnalysis  `json:"analysis"`
	Breaks      []ManifestBreak   `json:"breaks"`
	Delivery    ManifestDelivery  `json:"delivery"`
}

type ManifestProgramme struct {
	ID            string  `json:"id"`
	Title         string  `json:"title"`
	URL           string  `json:"url"`
	DurationSec   float64 `json:"duration_sec"`
	FrameRate     float64 `json:"frame_rate"`
	TimecodeBasis string  `json:"timecode_basis"`
	Width         int     `json:"width"`
	Height        int     `json:"height"`
	ContentHash   string  `json:"content_hash"`
}

type ManifestPolicy struct {
	Version          string     `json:"version"`
	MinGapSec        float64    `json:"min_gap_sec"`
	MaxBreaksPerHour int        `json:"max_breaks_per_hour"`
	MaxAdLoadPercent float64    `json:"max_ad_load_percent"`
	AdLoadPercent    float64    `json:"ad_load_percent"`
	TotalAdSec       int        `json:"total_ad_sec"`
	EdgeGuardSec     [2]float64 `json:"edge_guard_sec"`
}

type ManifestAnalysis struct {
	Pipeline         map[string]string `json:"pipeline"`
	ShotCount        int               `json:"shot_count"`
	SceneChangeCount int               `json:"scene_change_count"`
	RecommendedCount int               `json:"recommended_break_count"`
	SelectedCount    int               `json:"selected_break_count"`
}

type ManifestScene struct {
	SceneID           string   `json:"scene_id"`
	StartSec          float64  `json:"start_sec"`
	EndSec            float64  `json:"end_sec"`
	Summary           string   `json:"summary"`
	Mood              []string `json:"mood"`
	Activities        []string `json:"activities"`
	SensitiveContexts []string `json:"sensitive_contexts"`
}

type ManifestPlacement struct {
	Source         string             `json:"source"`
	CandidateID    string             `json:"candidate_id,omitempty"`
	ConfidenceTier string             `json:"confidence_tier"`
	AdScore        *float64           `json:"ad_score"`
	Rationale      string             `json:"rationale"`
	SignalScores   map[string]float64 `json:"signal_scores,omitempty"`
	ShotTransition string             `json:"shot_transition,omitempty"`
	ChangeType     string             `json:"change_type,omitempty"`
	FromContext    string             `json:"from_context,omitempty"`
	ToContext      string             `json:"to_context,omitempty"`
}

type ManifestAd struct {
	Sequence        int    `json:"sequence"`
	AdID            string `json:"ad_id"`
	BrandID         string `json:"brand_id"`
	BrandName       string `json:"brand_name"`
	CreativeURL     string `json:"creative_url"`
	MimeType        string `json:"mime_type"`
	DurationSec     int    `json:"duration_sec"`
	Language        string `json:"language"`
	Skippable       bool   `json:"skippable"`
	SkipOffsetSec   *int   `json:"skip_offset_sec"`
	ClickThroughURL string `json:"click_through_url,omitempty"`
	CTALabel        string `json:"cta_label,omitempty"`
	VASTURL         string `json:"vast_url"`
}

type ManifestBreak struct {
	BreakID         string            `json:"break_id"`
	Position        string            `json:"position"`
	OffsetSec       float64           `json:"offset_sec"`
	Timecode        string            `json:"timecode"`
	Frame           int               `json:"frame"`
	ResumeOffsetSec float64           `json:"resume_offset_sec"`
	Placement       ManifestPlacement `json:"placement"`
	PrecedingScene  *ManifestScene    `json:"preceding_scene"`
	FollowingScene  *ManifestScene    `json:"following_scene"`
	PodDurationSec  int               `json:"pod_duration_sec"`
	Ads             []ManifestAd      `json:"ads"`
	OnAdError       string            `json:"on_ad_error"`
}

type ManifestDelivery struct {
	VMAPURL  string `json:"vmap_url"`
	DebugURL string `json:"debug_url"`
}

func manifestScene(scene *SceneEvidence) *ManifestScene {
	if scene == nil {
		return nil
	}
	return &ManifestScene{SceneID: scene.SceneID, StartSec: scene.Start, EndSec: scene.End, Summary: scene.Summary,
		Mood: nonNil(scene.Tone), Activities: nonNil(scene.Activities), SensitiveContexts: nonNil(scene.SensitiveContexts)}
}

func nonNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// smpteTimecode renders HH:MM:SS:FF at the nominal (rounded) frame rate, non-drop-frame.
func smpteTimecode(seconds, frameRate float64) (string, int) {
	nominal := int(math.Round(frameRate))
	if nominal <= 0 {
		nominal = 25
	}
	frame := int(math.Round(seconds * frameRate))
	if frameRate <= 0 {
		frame = int(math.Round(seconds * float64(nominal)))
	}
	total := frame / nominal
	return fmt.Sprintf("%02d:%02d:%02d:%02d", total/3600, total%3600/60, total%60, frame%nominal), frame
}

func buildManifest(job Job, plan PlaybackPlan, revision int, finalizedBy string, r *http.Request) AdManifest {
	transcript := job.Transcript
	manifest := AdManifest{
		Schema: absoluteURL(r, manifestSchemaURL), Version: manifestVersion, Revision: revision, Status: "final",
		GeneratedAt: time.Now().UTC(), FinalizedBy: finalizedBy,
		Programme: ManifestProgramme{
			ID: job.ID, Title: job.FileName, URL: plan.ProgrammeURL, DurationSec: job.Media.DurationSeconds,
			FrameRate: job.Media.FrameRate, TimecodeBasis: "non-drop-frame", Width: job.Media.Width,
			Height: job.Media.Height, ContentHash: job.ContentHash,
		},
		Policy: ManifestPolicy{
			Version: transcript.BreakPolicy.Version, MinGapSec: transcript.BreakPolicy.MinGapSeconds,
			MaxBreaksPerHour: transcript.BreakPolicy.MaxBreaksPerHour, MaxAdLoadPercent: transcript.BreakPolicy.MaxAdLoadPercent,
			EdgeGuardSec: [2]float64{minLeadSeconds, minTailSeconds},
		},
		Analysis: ManifestAnalysis{
			Pipeline: transcript.Pipeline, ShotCount: len(transcript.ShotBoundaries),
			SceneChangeCount: transcript.BreakPolicy.SceneChangeCount, RecommendedCount: transcript.BreakPolicy.AutoBreakCount,
			SelectedCount: len(plan.Breaks),
		},
		Breaks:   make([]ManifestBreak, 0, len(plan.Breaks)),
		Delivery: ManifestDelivery{VMAPURL: plan.VMAPURL, DebugURL: plan.DebugURL},
	}
	if manifest.Analysis.Pipeline == nil {
		manifest.Analysis.Pipeline = map[string]string{}
	}
	for _, item := range plan.Breaks {
		timecode, frame := smpteTimecode(item.Time, job.Media.FrameRate)
		placement := ManifestPlacement{Source: item.Source, ConfidenceTier: "Reviewer", Rationale: "Placed by a human reviewer."}
		for i := range transcript.BreakCandidates {
			candidate := &transcript.BreakCandidates[i]
			if math.Abs(candidate.Time-item.Time) >= 0.5 {
				continue
			}
			score := candidate.AdScore
			placement = ManifestPlacement{
				Source: item.Source, CandidateID: candidate.CandidateID, ConfidenceTier: candidate.Tier, AdScore: &score,
				Rationale: candidate.Rationale, SignalScores: candidate.SignalScores, ShotTransition: candidate.ShotTransition,
				ChangeType: candidate.TransitionKind, FromContext: candidate.FromContext, ToContext: candidate.ToContext,
			}
			if item.Source == "manual" {
				placement.Rationale = "Reviewer placement at an analysed boundary: " + candidate.Rationale
			}
			break
		}
		var skipOffset *int
		if item.AllowSkip {
			offset := item.SkipAfterSec
			skipOffset = &offset
		}
		manifest.Breaks = append(manifest.Breaks, ManifestBreak{
			BreakID: item.BreakID, Position: "midroll", OffsetSec: item.Time, Timecode: timecode, Frame: frame,
			ResumeOffsetSec: item.Time, Placement: placement,
			PrecedingScene: manifestScene(sceneBeforeMarker(transcript, item.Time)),
			FollowingScene: manifestScene(sceneAfterMarker(transcript, item.Time)),
			PodDurationSec: item.DurationSec,
			Ads: []ManifestAd{{
				Sequence: 1, AdID: item.CreativeID, BrandID: item.BrandID, BrandName: item.BrandName,
				CreativeURL: item.CreativeURL, MimeType: "video/mp4", DurationSec: item.DurationSec, Language: item.Language,
				Skippable: item.AllowSkip, SkipOffsetSec: skipOffset, ClickThroughURL: item.ClickURL, CTALabel: item.CTALabel,
				VASTURL: absoluteURL(r, "/api/jobs/"+job.ID+"/vast/"+item.BreakID),
			}},
			OnAdError: "skip_ad_and_resume",
		})
		manifest.Policy.TotalAdSec += item.DurationSec
	}
	if job.Media.DurationSeconds > 0 {
		total := float64(manifest.Policy.TotalAdSec)
		manifest.Policy.AdLoadPercent = math.Round(total/(job.Media.DurationSeconds+total)*1000) / 10
	}
	return manifest
}

func decodeSelections(w http.ResponseWriter, r *http.Request) ([]PlaybackSelection, *ReviewState, error) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var body struct {
		Breaks     []PlaybackSelection `json:"breaks"`
		Target     *int                `json:"target"`
		ExcludedAI []string            `json:"excluded_ai"`
	}
	if err := decoder.Decode(&body); err != nil {
		return nil, nil, errors.New("the review request is invalid JSON")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, nil, errors.New("the review request must contain one JSON object")
	}
	if len(body.Breaks) > maxReviewBreaks || len(body.ExcludedAI) > maxBreakCandidates {
		return nil, nil, errors.New("the review request has too many entries")
	}
	for _, selection := range body.Breaks {
		if math.IsNaN(selection.Time) || math.IsInf(selection.Time, 0) || selection.Time < 0 ||
			(selection.Source != "ai" && selection.Source != "manual") {
			return nil, nil, errors.New("each break needs a valid time and an ai or manual source")
		}
	}
	review := &ReviewState{Selected: body.Breaks, ExcludedAI: body.ExcludedAI, UpdatedAt: time.Now().UTC()}
	if review.Selected == nil {
		review.Selected = []PlaybackSelection{}
	}
	if review.ExcludedAI == nil {
		review.ExcludedAI = []string{}
	}
	review.Target = len(review.Selected)
	if body.Target != nil && *body.Target >= 0 && *body.Target <= maxReviewBreaks {
		review.Target = *body.Target
	}
	return body.Breaks, review, nil
}

func (s *server) saveReview(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.getJobSnapshot(id); !ok {
		writeError(w, http.StatusNotFound, "Analysis job not found.")
		return
	}
	_, review, err := decodeSelections(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Review could not be saved: "+err.Error()+".")
		return
	}
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	job, ok := s.jobs[id]
	if !ok {
		writeError(w, http.StatusNotFound, "Analysis job not found.")
		return
	}
	if job.Review != nil {
		review.FinalizedRevision = job.Review.FinalizedRevision
	}
	review.Dirty = review.FinalizedRevision > 0
	job.Review = review
	s.jobs[id] = job
	if err := s.persistJob(job); err != nil {
		s.logger.Error("persist review", "job_id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "Could not save the review.")
		return
	}
	writeJSON(w, http.StatusOK, review)
}

func (s *server) finalizePlan(w http.ResponseWriter, r *http.Request) {
	job, ok := s.getJobSnapshot(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "Analysis job not found.")
		return
	}
	if job.Transcript == nil || job.Status != "completed" {
		writeError(w, http.StatusConflict, "Finalizing needs a completed analysis.")
		return
	}
	selections, review, err := decodeSelections(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Plan could not be finalized: "+err.Error()+".")
		return
	}
	if len(selections) == 0 {
		writeError(w, http.StatusUnprocessableEntity, "Add at least one ad break before finalizing.")
		return
	}
	brands, err := s.loadCatalog()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	plan, err := buildPlaybackPlan(job, selections, brands, r)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if job.Media.FrameRate <= 0 {
		if media, probeErr := probeVideo(r.Context(), s.uploadPath(job.ID)); probeErr == nil {
			job.Media.FrameRate = media.FrameRate
		}
	}
	finalizedBy, _, _ := r.BasicAuth()
	if finalizedBy == "" {
		finalizedBy = "reviewer"
	}

	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	current, exists := s.jobs[job.ID]
	if !exists {
		writeError(w, http.StatusNotFound, "Analysis job not found.")
		return
	}
	current.Media.FrameRate = job.Media.FrameRate
	revision := len(current.Manifests) + 1
	manifest := buildManifest(current, plan, revision, finalizedBy, r)
	encoded, err := json.Marshal(manifest)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Could not encode the manifest.")
		return
	}
	current.Manifests = append(current.Manifests, ManifestRecord{
		Revision: revision, FinalizedAt: manifest.GeneratedAt, BreakCount: len(plan.Breaks), Manifest: encoded,
	})
	review.FinalizedRevision, review.Dirty = revision, false
	current.Review = review
	current.PlaybackPlan = &plan
	current.PlaybackEvents = []PlaybackEvent{}
	s.jobs[current.ID] = current
	if err := s.persistJob(current); err != nil {
		s.logger.Error("persist finalized manifest", "job_id", current.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "Could not save the finalized plan.")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"revision": revision, "manifest": manifest, "plan": plan, "review": review,
		"manifest_url": absoluteURL(r, "/api/jobs/"+current.ID+"/manifest.json?revision="+strconv.Itoa(revision)),
	})
}

func (s *server) getManifest(w http.ResponseWriter, r *http.Request) {
	job, ok := s.getJobSnapshot(r.PathValue("id"))
	if !ok || len(job.Manifests) == 0 {
		writeError(w, http.StatusNotFound, "No finalized manifest exists for this job yet.")
		return
	}
	record := job.Manifests[len(job.Manifests)-1]
	if value := r.URL.Query().Get("revision"); value != "" {
		revision, err := strconv.Atoi(value)
		if err != nil || revision < 1 || revision > len(job.Manifests) {
			writeError(w, http.StatusNotFound, "That manifest revision does not exist.")
			return
		}
		record = job.Manifests[revision-1]
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="scenesense-manifest-%s-r%d.json"`, job.ID, record.Revision))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(record.Manifest)
}
