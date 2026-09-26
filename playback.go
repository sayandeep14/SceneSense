package main

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	playbackPlanVersion = "playback-v1"
	maxPlaybackEvents   = 500
)

type CatalogCreative struct {
	ID          string `json:"id"`
	DurationSec int    `json:"duration_sec"`
	Language    string `json:"language"`
	SourceURL   string `json:"url"`
}

type CatalogBrand struct {
	BrandID          string            `json:"brand_id"`
	DisplayName      string            `json:"display_name"`
	Category         string            `json:"category"`
	NegativeContexts []string          `json:"negative_contexts"`
	Creatives        []CatalogCreative `json:"creatives"`
}

type PlaybackSelection struct {
	Time       float64 `json:"time"`
	BrandID    string  `json:"brand_id"`
	CreativeID string  `json:"creative_id"`
	Source     string  `json:"source"`
}

type PlaybackBreak struct {
	BreakID        string  `json:"break_id"`
	Time           float64 `json:"time"`
	BrandID        string  `json:"brand_id"`
	BrandName      string  `json:"brand_name"`
	CreativeID     string  `json:"creative_id"`
	SourceFilename string  `json:"source_filename"`
	CreativeTitle  string  `json:"creative_title"`
	DurationSec    int     `json:"duration_sec"`
	Language       string  `json:"language"`
	CreativeURL    string  `json:"creative_url"`
	Source         string  `json:"source"`
	SceneMood      string  `json:"preceding_scene_mood,omitempty"`
	SceneContext   string  `json:"preceding_scene_context,omitempty"`
}

type PlaybackPlan struct {
	Version      string          `json:"version"`
	GeneratedAt  time.Time       `json:"generated_at"`
	Duration     float64         `json:"programme_duration_sec"`
	ProgrammeURL string          `json:"programme_url"`
	VMAPURL      string          `json:"vmap_url"`
	DebugURL     string          `json:"debug_url"`
	Breaks       []PlaybackBreak `json:"breaks"`
}

type PlaybackEvent struct {
	Event       string    `json:"event"`
	BreakID     string    `json:"break_id"`
	ProgrammeAt float64   `json:"programme_time_sec"`
	Detail      string    `json:"detail,omitempty"`
	RecordedAt  time.Time `json:"recorded_at"`
}

type playbackPlanRequest struct {
	Breaks []PlaybackSelection `json:"breaks"`
}

func loadBrandCatalog(path string) ([]CatalogBrand, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("brand catalogue is unavailable")
	}
	var brands []CatalogBrand
	if err := json.Unmarshal(data, &brands); err != nil || len(brands) == 0 {
		return nil, errors.New("brand catalogue is invalid")
	}
	seenBrands := make(map[string]bool, len(brands))
	for _, brand := range brands {
		if brand.BrandID == "" || brand.DisplayName == "" || seenBrands[brand.BrandID] || len(brand.Creatives) == 0 {
			return nil, errors.New("brand catalogue contains an invalid entry")
		}
		seenBrands[brand.BrandID] = true
		seenCreatives := make(map[string]bool, len(brand.Creatives))
		for _, creative := range brand.Creatives {
			if creative.ID == "" || seenCreatives[creative.ID] || creative.DurationSec <= 0 || creative.DurationSec > 120 ||
				creative.Language == "" || creative.SourceURL == "" ||
				!strings.HasPrefix(creative.SourceURL, "ads/"+brand.BrandID+"/") ||
				filepath.Clean(filepath.FromSlash(creative.SourceURL)) != filepath.FromSlash(creative.SourceURL) {
				return nil, errors.New("brand catalogue contains an invalid creative")
			}
			seenCreatives[creative.ID] = true
		}
	}
	return brands, nil
}

func (s *server) listBrands(w http.ResponseWriter, _ *http.Request) {
	brands, err := loadBrandCatalog(s.brandsPath)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"brands": brands, "creative_mode": "locally generated static placeholder MP4s"})
}

func findBrand(brands []CatalogBrand, id string) (CatalogBrand, bool) {
	for _, brand := range brands {
		if brand.BrandID == id {
			return brand, true
		}
	}
	return CatalogBrand{}, false
}

func (s *server) createPlaybackPlan(w http.ResponseWriter, r *http.Request) {
	job, ok := s.getJobSnapshot(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "Analysis job not found.")
		return
	}
	if job.Transcript == nil || job.Status != "completed" {
		writeError(w, http.StatusConflict, "Playback needs a completed analysis.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var request playbackPlanRequest
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "Playback selections are invalid JSON.")
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "Playback request must contain one JSON object.")
		return
	}
	brands, err := loadBrandCatalog(s.brandsPath)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	plan, err := buildPlaybackPlan(job, request.Breaks, brands, r)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.jobsMu.Lock()
	current, exists := s.jobs[job.ID]
	if !exists {
		s.jobsMu.Unlock()
		writeError(w, http.StatusNotFound, "Analysis job not found.")
		return
	}
	current.PlaybackPlan = &plan
	current.PlaybackEvents = []PlaybackEvent{}
	s.jobs[current.ID] = current
	if err := s.persistJob(current); err != nil {
		s.logger.Error("persist playback plan", "job_id", job.ID, "error", err)
		s.jobsMu.Unlock()
		writeError(w, http.StatusInternalServerError, "Could not save the playback plan.")
		return
	}
	s.jobsMu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{"plan": plan, "break_count": len(plan.Breaks)})
}

func buildPlaybackPlan(job Job, selections []PlaybackSelection, brands []CatalogBrand, r *http.Request) (PlaybackPlan, error) {
	if job.Transcript == nil || job.Media.DurationSeconds <= 0 || job.Transcript.Duration <= 0 {
		return PlaybackPlan{}, errors.New("completed media evidence is required")
	}
	policy := job.Transcript.BreakPolicy
	if policy.Version == "" {
		return PlaybackPlan{}, errors.New("a current break policy is required")
	}
	maxCount := policy.MaxBreakCount
	if maxCount <= 0 {
		maxCount = int(math.Ceil(job.Media.DurationSeconds / 3600 * float64(maxBreaksPerHour)))
		if maxCount < 1 {
			maxCount = 1
		}
	}
	if len(selections) > maxCount {
		return PlaybackPlan{}, fmt.Errorf("this programme allows at most %d ad breaks", maxCount)
	}
	ordered := append([]PlaybackSelection(nil), selections...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Time < ordered[j].Time })
	plan := PlaybackPlan{
		Version: playbackPlanVersion, GeneratedAt: time.Now().UTC(),
		Duration: job.Media.DurationSeconds, ProgrammeURL: absoluteURL(r, "/media/"+job.ID),
		VMAPURL:  absoluteURL(r, "/api/jobs/"+job.ID+"/vmap.xml"),
		DebugURL: absoluteURL(r, "/api/jobs/"+job.ID+"/debug.json"),
		Breaks:   make([]PlaybackBreak, 0, len(ordered)),
	}
	var totalAdSeconds float64
	for i, selection := range ordered {
		if math.IsNaN(selection.Time) || math.IsInf(selection.Time, 0) || selection.Time < minLeadSeconds ||
			selection.Time > job.Media.DurationSeconds-minTailSeconds {
			return PlaybackPlan{}, errors.New("each ad break must leave 15 seconds at the start and 10 seconds at the end")
		}
		if i > 0 && selection.Time-ordered[i-1].Time < policy.MinGapSeconds {
			return PlaybackPlan{}, fmt.Errorf("ad breaks must be at least %.0f seconds apart", policy.MinGapSeconds)
		}
		if selection.Source != "ai" && selection.Source != "manual" {
			return PlaybackPlan{}, errors.New("each placement must be AI-selected or explicitly human-reviewed")
		}
		brand, exists := findBrand(brands, selection.BrandID)
		if !exists {
			return PlaybackPlan{}, errors.New("selected brand does not exist in the synthetic catalogue")
		}
		creative, exists := findCreative(brand, selection.CreativeID)
		if !exists {
			return PlaybackPlan{}, errors.New("selected creative does not belong to the selected brand")
		}
		scene := sceneBeforeMarker(job.Transcript, selection.Time)
		if scene == nil {
			return PlaybackPlan{}, errors.New("scene context is unavailable at a selected ad break")
		}
		match, exists := sceneBrandMatch(*scene, selection.BrandID)
		if !exists {
			return PlaybackPlan{}, errors.New("scene-level brand safety evidence is unavailable; fail closed")
		}
		if match.Blocked {
			return PlaybackPlan{}, fmt.Errorf("%s is blocked by preceding scene context: %s", brand.DisplayName, strings.Join(match.BlockedContexts, ", "))
		}
		for _, candidate := range job.Transcript.BreakCandidates {
			if math.Abs(candidate.Time-selection.Time) < 0.5 && selection.Source == "ai" && !candidate.Potential {
				return PlaybackPlan{}, errors.New("AI-selected placement is not an AI-qualified potential break")
			}
		}
		if selection.Source == "ai" {
			qualified := false
			for _, candidate := range job.Transcript.BreakCandidates {
				if math.Abs(candidate.Time-selection.Time) < 0.5 && candidate.Potential {
					qualified = true
					break
				}
			}
			if !qualified {
				return PlaybackPlan{}, errors.New("AI-selected placement is not in the AI potential list")
			}
		}
		previousMood, previousContext := scene.Tone, scene.Summary
		breakID := fmt.Sprintf("break-%03d", i+1)
		plan.Breaks = append(plan.Breaks, PlaybackBreak{
			BreakID: breakID, Time: selection.Time, BrandID: brand.BrandID, BrandName: brand.DisplayName,
			CreativeID: creative.ID, SourceFilename: filepath.Base(creative.SourceURL),
			CreativeTitle: brand.DisplayName + " · " + filepath.Base(creative.SourceURL),
			DurationSec:   creative.DurationSec, Language: creative.Language,
			CreativeURL: absoluteURL(r, "/ads/"+brand.BrandID+"/"+filepath.Base(creative.SourceURL)),
			Source:      selection.Source, SceneMood: strings.Join(previousMood, ", "), SceneContext: previousContext,
		})
		totalAdSeconds += float64(creative.DurationSec)
	}
	if totalAdSeconds/(job.Media.DurationSeconds+totalAdSeconds) > policy.MaxAdLoadPercent/100 {
		return PlaybackPlan{}, fmt.Errorf("selected creative durations exceed the %.0f%% ad-load limit", policy.MaxAdLoadPercent)
	}
	return plan, nil
}

func findCreative(brand CatalogBrand, id string) (CatalogCreative, bool) {
	for _, creative := range brand.Creatives {
		if creative.ID == id {
			return creative, true
		}
	}
	return CatalogCreative{}, false
}

func sceneBeforeMarker(transcript *Transcript, at float64) *SceneEvidence {
	if transcript == nil {
		return nil
	}
	for _, transition := range transcript.Transitions {
		if transition.Continuity == "new_scene" && math.Abs(transition.Time-at) <= 1.2 {
			var previous *SceneEvidence
			for i := range transcript.Scenes {
				scene := &transcript.Scenes[i]
				if scene.Start < at && scene.End <= at+2 && (previous == nil || scene.End > previous.End) {
					previous = scene
				}
			}
			if previous != nil {
				return previous
			}
		}
	}
	var active *SceneEvidence
	for i := range transcript.Scenes {
		scene := &transcript.Scenes[i]
		if scene.Start <= at && at <= scene.End && (active == nil || scene.Start > active.Start) {
			active = scene
		}
	}
	if active != nil {
		return active
	}
	var previous *SceneEvidence
	for i := range transcript.Scenes {
		scene := &transcript.Scenes[i]
		if scene.Start < at && (previous == nil || scene.End > previous.End) {
			previous = scene
		}
	}
	return previous
}

func sceneBrandMatch(scene SceneEvidence, brandID string) (SceneBrandMatch, bool) {
	for _, match := range scene.BrandMatches {
		if match.BrandID == brandID {
			return match, true
		}
	}
	return SceneBrandMatch{}, false
}

func findPlaybackBreak(job Job, id string) (PlaybackBreak, bool) {
	if job.PlaybackPlan == nil {
		return PlaybackBreak{}, false
	}
	for _, item := range job.PlaybackPlan.Breaks {
		if item.BreakID == id {
			return item, true
		}
	}
	return PlaybackBreak{}, false
}

func (s *server) getVMAP(w http.ResponseWriter, r *http.Request) {
	job, ok := s.getJobSnapshot(r.PathValue("id"))
	if !ok || job.PlaybackPlan == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/vmap+xml; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="scenesense-vmap-`+job.ID+`.xml"`)
	_, _ = io.WriteString(w, renderVMAP(job, r))
}

func renderVMAP(job Job, r *http.Request) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<vmap:VMAP xmlns:vmap="http://www.iab.net/videosuite/vmap" version="1.0">` + "\n")
	if job.PlaybackPlan != nil {
		for _, item := range job.PlaybackPlan.Breaks {
			b.WriteString(fmt.Sprintf(`<vmap:AdBreak timeOffset="%s" breakType="linear" breakId="%s">`, formatVMAPTime(item.Time), xmlEscape(item.BreakID)))
			b.WriteString(`<vmap:AdSource id="1" allowMultipleAds="false" followRedirects="true"><vmap:AdTagURI templateType="vast3"><![CDATA[`)
			b.WriteString(absoluteURL(r, "/api/jobs/"+job.ID+"/vast/"+item.BreakID))
			b.WriteString(`]]></vmap:AdTagURI></vmap:AdSource>`)
			b.WriteString(`</vmap:AdBreak>` + "\n")
		}
	}
	b.WriteString(`</vmap:VMAP>` + "\n")
	return b.String()
}

func formatVMAPTime(seconds float64) string {
	whole := int64(seconds)
	millis := int64(math.Round((seconds - float64(whole)) * 1000))
	if millis == 1000 {
		whole++
		millis = 0
	}
	hours := whole / 3600
	minutes := (whole % 3600) / 60
	secs := whole % 60
	return fmt.Sprintf("%02d:%02d:%02d.%03d", hours, minutes, secs, millis)
}

func xmlEscape(value string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(value))
	return b.String()
}

func absoluteURL(r *http.Request, path string) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host + path
}

func (s *server) getVAST(w http.ResponseWriter, r *http.Request) {
	job, ok := s.getJobSnapshot(r.PathValue("id"))
	item, found := findPlaybackBreak(job, r.PathValue("breakID"))
	if !ok || !found {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	_, _ = io.WriteString(w, renderVAST(item))
}

func renderVAST(item PlaybackBreak) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<VAST version="3.0"><Ad id="` + xmlEscape(item.BreakID) + `"><InLine>`)
	b.WriteString(`<AdSystem version="1.0">SceneSense Demo</AdSystem><AdTitle>` + xmlEscape(item.SourceFilename) + `</AdTitle>`)
	b.WriteString(`<Description>Demo creative for ` + xmlEscape(item.BrandName) + ` at ` + formatVMAPTime(item.Time) + `; annotated preceding-scene mood: ` + xmlEscape(item.SceneMood) + `.</Description>`)
	b.WriteString(`<Creatives><Creative sequence="1"><Linear><Duration>` + formatVMAPTime(float64(item.DurationSec)) + `</Duration><MediaFiles>`)
	b.WriteString(`<MediaFile delivery="progressive" type="video/mp4" width="640" height="360" scalable="true" maintainAspectRatio="true"><![CDATA[` + item.CreativeURL + `]]></MediaFile>`)
	b.WriteString(`</MediaFiles></Linear></Creative></Creatives></InLine></Ad></VAST>`)
	return b.String()
}

func (s *server) getPlaybackDebug(w http.ResponseWriter, r *http.Request) {
	job, ok := s.getJobSnapshot(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="scenesense-debug-`+job.ID+`.json"`)
	writeJSON(w, http.StatusOK, map[string]any{
		"job_id": job.ID, "status": job.Status, "media": job.Media,
		"transcript": job.Transcript, "playback_plan": job.PlaybackPlan, "playback_events": job.PlaybackEvents,
	})
}

func (s *server) recordPlaybackEvent(w http.ResponseWriter, r *http.Request) {
	var event PlaybackEvent
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&event); err != nil {
		writeError(w, http.StatusBadRequest, "Playback event is invalid.")
		return
	}
	validEvents := map[string]bool{"break_start": true, "ad_start": true, "ad_complete": true, "resume": true, "error": true, "skip": true}
	if !validEvents[event.Event] || math.IsNaN(event.ProgrammeAt) || math.IsInf(event.ProgrammeAt, 0) {
		writeError(w, http.StatusBadRequest, "Playback event type or time is invalid.")
		return
	}
	s.jobsMu.Lock()
	job, exists := s.jobs[r.PathValue("id")]
	if !exists {
		s.jobsMu.Unlock()
		writeError(w, http.StatusNotFound, "Analysis job not found.")
		return
	}
	if _, exists := findPlaybackBreak(job, event.BreakID); !exists {
		s.jobsMu.Unlock()
		writeError(w, http.StatusUnprocessableEntity, "Playback event references an unknown ad break.")
		return
	}
	if event.ProgrammeAt < 0 || job.Media.DurationSeconds > 0 && event.ProgrammeAt > job.Media.DurationSeconds+1 {
		s.jobsMu.Unlock()
		writeError(w, http.StatusBadRequest, "Playback event time is outside the programme.")
		return
	}
	if len(job.PlaybackEvents) >= maxPlaybackEvents {
		s.jobsMu.Unlock()
		writeError(w, http.StatusTooManyRequests, "Playback event log is full for this demo job.")
		return
	}
	if len(event.Detail) > 240 {
		event.Detail = event.Detail[:240]
	}
	event.RecordedAt = time.Now().UTC()
	job.PlaybackEvents = append(job.PlaybackEvents, event)
	s.jobs[job.ID] = job
	if err := s.persistJob(job); err != nil {
		s.logger.Warn("persist playback event", "job_id", job.ID, "event", event.Event, "error", err)
	}
	s.jobsMu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) serveCatalogCreative(w http.ResponseWriter, r *http.Request) {
	brands, err := loadBrandCatalog(s.brandsPath)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	brandID, filename := r.PathValue("brandID"), r.PathValue("file")
	brand, exists := findBrand(brands, brandID)
	if !exists {
		http.NotFound(w, r)
		return
	}
	var sourceURL string
	for _, creative := range brand.Creatives {
		if filepath.Base(creative.SourceURL) == filename && strings.HasPrefix(creative.SourceURL, "ads/"+brandID+"/") {
			sourceURL = creative.SourceURL
			break
		}
	}
	if sourceURL == "" || filepath.Base(filename) != filename {
		http.NotFound(w, r)
		return
	}
	relative := filepath.Clean(filepath.FromSlash(sourceURL))
	if filepath.IsAbs(relative) || relative == "." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		http.NotFound(w, r)
		return
	}
	path := filepath.Join(filepath.Dir(s.brandsPath), relative)
	if filepath.Ext(path) != ".mp4" {
		http.NotFound(w, r)
		return
	}
	if _, err := os.Stat(path); err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	http.ServeFile(w, r, path)
}

func validatePlaybackJob(job Job) error {
	if job.Transcript == nil || job.PlaybackPlan == nil {
		return errors.New("playback plan or analysis is missing")
	}
	return nil
}

func decodePlaybackPlan(data []byte, job Job, brands []CatalogBrand, r *http.Request) (PlaybackPlan, error) {
	var request playbackPlanRequest
	if err := json.Unmarshal(data, &request); err != nil {
		return PlaybackPlan{}, err
	}
	return buildPlaybackPlan(job, request.Breaks, brands, r)
}
