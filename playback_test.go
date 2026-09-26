package main

import (
	"encoding/json"
	"encoding/xml"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func playbackFixture(t *testing.T, duration float64) (Job, []CatalogBrand) {
	t.Helper()
	brands, err := loadBrandCatalog("assets/brands.json")
	if err != nil {
		t.Fatal(err)
	}
	matches := make([]SceneBrandMatch, 0, len(brands))
	for _, entry := range brands {
		matches = append(matches, SceneBrandMatch{
			BrandID: entry.BrandID, DisplayName: entry.DisplayName, Category: entry.Category,
			FitScore: 0.7, Reason: "Safe demo fit", Recommended: true,
		})
	}
	job := Job{
		ID: "fixture-job", Status: "completed", Media: MediaInfo{DurationSeconds: duration},
		Transcript: &Transcript{
			Duration: duration,
			Scenes: []SceneEvidence{{SceneID: "scene-1", Start: 0, End: duration, Summary: "A calm conversation",
				Tone: []string{"warm", "reflective"}, Confidence: 0.9, DialogueState: "completed_thought", BrandMatches: matches}},
			BreakCandidates: []BreakCandidate{{CandidateID: "candidate-1", Time: 60, Potential: true}},
			BreakPolicy: BreakPolicyInfo{Version: breakPolicyVersion, MinGapSeconds: minBreakGap,
				MaxBreakCount: 4, MaxAdLoadPercent: 20, PlannedAdSeconds: 15},
		},
	}
	return job, brands
}

func TestBuildPlaybackPlanUsesSelectedCatalogueCreativeAndSceneMood(t *testing.T) {
	job, brands := playbackFixture(t, 1800)
	request := httptest.NewRequest(http.MethodPost, "/api/jobs/fixture-job/playback-plan", nil)
	request.Host = "demo.example"
	request.Header.Set("X-Forwarded-Proto", "https")
	plan, err := buildPlaybackPlan(job, []PlaybackSelection{{
		Time: 60, BrandID: brands[0].BrandID, CreativeID: brands[0].Creatives[0].ID, Source: "manual",
	}}, brands, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Breaks) != 1 {
		t.Fatalf("break count = %d, want 1", len(plan.Breaks))
	}
	item := plan.Breaks[0]
	if item.SourceFilename != "a_15s_bn.mp4" || item.DurationSec != 15 || item.SceneMood != "warm, reflective" {
		t.Fatalf("playback creative metadata = %+v", item)
	}
	if item.CreativeURL != "https://demo.example/ads/brand_a/a_15s_bn.mp4" {
		t.Fatalf("creative URL = %q, expected same-origin HTTPS", item.CreativeURL)
	}
}

func TestBuildPlaybackPlanRejectsSpacingCountAdLoadAndBlockedBrands(t *testing.T) {
	job, brands := playbackFixture(t, 1800)
	selection := func(at float64, brand CatalogBrand, creative CatalogCreative) PlaybackSelection {
		return PlaybackSelection{Time: at, BrandID: brand.BrandID, CreativeID: creative.ID, Source: "manual"}
	}
	request := httptest.NewRequest(http.MethodPost, "/", nil)
	brand := brands[0]
	if _, err := buildPlaybackPlan(job, []PlaybackSelection{
		selection(60, brand, brand.Creatives[0]), selection(200, brand, brand.Creatives[0]),
	}, brands, request); err == nil || !strings.Contains(err.Error(), "at least 300 seconds") {
		t.Fatalf("nearby ad markers should be rejected, got %v", err)
	}
	tooMany := make([]PlaybackSelection, 5)
	for i := range tooMany {
		tooMany[i] = selection(60+float64(i)*310, brand, brand.Creatives[0])
	}
	if _, err := buildPlaybackPlan(job, tooMany, brands, request); err == nil || !strings.Contains(err.Error(), "at most 4") {
		t.Fatalf("fifth ad marker should be rejected, got %v", err)
	}
	short, shortBrands := playbackFixture(t, 60)
	if _, err := buildPlaybackPlan(short, []PlaybackSelection{selection(30, shortBrands[0], shortBrands[0].Creatives[2])}, shortBrands, request); err == nil || !strings.Contains(err.Error(), "ad-load limit") {
		t.Fatalf("30-second creative in a 60-second programme should violate ad load, got %v", err)
	}
	job.Transcript.Scenes[0].BrandMatches[0].Blocked = true
	job.Transcript.Scenes[0].BrandMatches[0].BlockedContexts = []string{"grief"}
	if _, err := buildPlaybackPlan(job, []PlaybackSelection{selection(60, brand, brand.Creatives[0])}, brands, request); err == nil || !strings.Contains(err.Error(), "blocked by preceding scene") {
		t.Fatalf("negative-context brand should be rejected, got %v", err)
	}
}

func TestVMAPAndVASTContractExposeOrderedTimedCreativeAndMood(t *testing.T) {
	job, brands := playbackFixture(t, 1800)
	request := httptest.NewRequest(http.MethodPost, "/", nil)
	request.Host = "demo.example"
	plan, err := buildPlaybackPlan(job, []PlaybackSelection{{
		Time: 25.515, BrandID: brands[0].BrandID, CreativeID: brands[0].Creatives[0].ID, Source: "manual",
	}}, brands, request)
	if err != nil {
		t.Fatal(err)
	}
	job.PlaybackPlan = &plan
	vmap := renderVMAP(job, request)
	var parsed struct {
		XMLName xml.Name `xml:"http://www.iab.net/videosuite/vmap VMAP"`
		Breaks  []struct {
			TimeOffset string `xml:"timeOffset,attr"`
			Source     struct {
				AdTagURI string `xml:"AdTagURI"`
			} `xml:"AdSource"`
		} `xml:"AdBreak"`
	}
	if err := xml.Unmarshal([]byte(vmap), &parsed); err != nil {
		t.Fatalf("VMAP is not well-formed XML: %v\n%s", err, vmap)
	}
	if parsed.XMLName.Local != "VMAP" || len(parsed.Breaks) != 1 || parsed.Breaks[0].TimeOffset != "00:00:25.515" ||
		!strings.Contains(parsed.Breaks[0].Source.AdTagURI, "/api/jobs/fixture-job/vast/break-001") {
		t.Fatalf("VMAP break contract did not preserve the selected time/source: %+v", parsed)
	}
	vast := renderVAST(plan.Breaks[0])
	var vastDoc struct {
		XMLName xml.Name `xml:"VAST"`
		Body    string   `xml:",innerxml"`
	}
	if err := xml.Unmarshal([]byte(vast), &vastDoc); err != nil {
		t.Fatalf("VAST response is not well-formed XML: %v", err)
	}
	if vastDoc.XMLName.Local != "VAST" || !strings.Contains(vastDoc.Body, "a_15s_bn.mp4") ||
		!strings.Contains(vastDoc.Body, "warm, reflective") || !strings.Contains(vastDoc.Body, "video/mp4") {
		t.Fatalf("VAST response omitted filename, annotated mood, or media URL: %s", vast)
	}
}

func TestPlaybackPlanAndEventsEndpointsPersistDecisionGraph(t *testing.T) {
	app, dir := testServer(t)
	job, brands := playbackFixture(t, 1800)
	job.VideoURL = "/media/fixture-job"
	if err := app.persistJob(job); err != nil {
		t.Fatal(err)
	}
	app.jobs[job.ID] = job
	app.brandsPath = "assets/brands.json"
	body, _ := json.Marshal(playbackPlanRequest{Breaks: []PlaybackSelection{{
		Time: 60, BrandID: brands[0].BrandID, CreativeID: brands[0].Creatives[0].ID, Source: "manual",
	}}})
	request := httptest.NewRequest(http.MethodPost, "/api/jobs/fixture-job/playback-plan", strings.NewReader(string(body)))
	request.Host = "demo.example"
	response := httptest.NewRecorder()
	app.routes().ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create plan status=%d body=%s", response.Code, response.Body.String())
	}
	var result struct {
		Plan PlaybackPlan `json:"plan"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || len(result.Plan.Breaks) != 1 {
		t.Fatalf("created plan response = %+v, err=%v", result, err)
	}
	vmapResponse := httptest.NewRecorder()
	app.routes().ServeHTTP(vmapResponse, httptest.NewRequest(http.MethodGet, "/api/jobs/fixture-job/vmap.xml", nil))
	if vmapResponse.Code != http.StatusOK || !strings.Contains(vmapResponse.Body.String(), "00:01:00.000") {
		t.Fatalf("VMAP endpoint status=%d body=%s", vmapResponse.Code, vmapResponse.Body.String())
	}
	vastResponse := httptest.NewRecorder()
	app.routes().ServeHTTP(vastResponse, httptest.NewRequest(http.MethodGet, "/api/jobs/fixture-job/vast/break-001", nil))
	if vastResponse.Code != http.StatusOK || !strings.Contains(vastResponse.Body.String(), "a_15s_bn.mp4") {
		t.Fatalf("VAST endpoint status=%d body=%s", vastResponse.Code, vastResponse.Body.String())
	}
	eventBody := `{"event":"break_start","break_id":"break-001","programme_time_sec":60}`
	eventRequest := httptest.NewRequest(http.MethodPost, "/api/jobs/fixture-job/playback-events", strings.NewReader(eventBody))
	eventResponse := httptest.NewRecorder()
	app.routes().ServeHTTP(eventResponse, eventRequest)
	if eventResponse.Code != http.StatusNoContent {
		t.Fatalf("event status=%d body=%s", eventResponse.Code, eventResponse.Body.String())
	}
	updated, ok := app.getJobSnapshot(job.ID)
	if !ok || updated.PlaybackPlan == nil || len(updated.PlaybackEvents) != 1 || updated.PlaybackEvents[0].Event != "break_start" {
		t.Fatalf("plan/events were not persisted: %+v", updated)
	}
	debug := httptest.NewRecorder()
	app.routes().ServeHTTP(debug, httptest.NewRequest(http.MethodGet, "/api/jobs/fixture-job/debug.json", nil))
	if debug.Code != http.StatusOK || !strings.Contains(debug.Body.String(), "playback_events") || !strings.Contains(debug.Body.String(), "break_start") {
		t.Fatalf("debug JSON omitted event evidence: %s", debug.Body.String())
	}
	_ = dir
}

func TestCatalogueCreativesAreLocalPlayableMP4s(t *testing.T) {
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe is unavailable; local asset generation is verified in CI")
	}
	app, _ := testServer(t)
	brands, err := loadBrandCatalog("assets/brands.json")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, brand := range brands {
		for _, creative := range brand.Creatives {
			path := filepath.Join("assets", filepath.FromSlash(creative.SourceURL))
			if _, err := os.Stat(path); err != nil {
				t.Errorf("local catalogue creative %s is missing: %v", path, err)
				continue
			}
			probe := exec.Command(ffprobe, "-v", "error", "-show_entries", "format=duration", "-of", "default=noprint_wrappers=1:nokey=1", path)
			output, err := probe.Output()
			seconds, parseErr := strconv.ParseFloat(strings.TrimSpace(string(output)), 64)
			if err != nil || parseErr != nil || math.Abs(seconds-float64(creative.DurationSec)) > 0.05 {
				t.Errorf("%s duration=%q probeErr=%v parseErr=%v; want %d seconds", path, strings.TrimSpace(string(output)), err, parseErr, creative.DurationSec)
			}
			count++
		}
	}
	if count != 21 {
		t.Fatalf("checked %d creative files, expected all 21 catalogue files", count)
	}
	request := httptest.NewRequest(http.MethodGet, "/ads/brand_a/a_15s_bn.mp4", nil)
	request.SetPathValue("brandID", "brand_a")
	request.SetPathValue("file", "a_15s_bn.mp4")
	response := httptest.NewRecorder()
	app.serveCatalogCreative(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "video/mp4" || response.Body.Len() == 0 {
		t.Fatalf("static creative response status=%d type=%q bytes=%d", response.Code, response.Header().Get("Content-Type"), response.Body.Len())
	}
}

func TestMissingCreativeReturnsNotFoundWithoutGeneratingIt(t *testing.T) {
	dir := t.TempDir()
	brandsPath := filepath.Join(dir, "brands.json")
	data := `[{"brand_id":"brand_a","display_name":"Brand A","creatives":[{"id":"a_15s_bn","duration_sec":15,"language":"bn","url":"ads/brand_a/missing.mp4"}]}]`
	if err := os.WriteFile(brandsPath, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	app, _ := testServer(t)
	app.brandsPath = brandsPath
	request := httptest.NewRequest(http.MethodGet, "/ads/brand_a/missing.mp4", nil)
	request.SetPathValue("brandID", "brand_a")
	request.SetPathValue("file", "missing.mp4")
	response := httptest.NewRecorder()
	app.serveCatalogCreative(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("missing creative status=%d, want 404", response.Code)
	}
	if _, err := os.Stat(filepath.Join(dir, "ads", "brand_a", "missing.mp4")); !os.IsNotExist(err) {
		t.Fatalf("server unexpectedly created a missing creative: stat error=%v", err)
	}
}

func TestPlaybackPlanCarriesSkipAndClickThroughIntoVAST(t *testing.T) {
	job, brands := playbackFixture(t, 1800)
	brands[0].ClickThroughURL, brands[0].CTALabel = "https://brand-a.example/offer", "Shop now"
	request := httptest.NewRequest(http.MethodPost, "/", nil)
	selection := PlaybackSelection{Time: 60, BrandID: brands[0].BrandID, CreativeID: brands[0].Creatives[0].ID,
		Source: "manual", AllowSkip: true, SkipAfterSec: 5}
	plan, err := buildPlaybackPlan(job, []PlaybackSelection{selection}, brands, request)
	if err != nil {
		t.Fatal(err)
	}
	item := plan.Breaks[0]
	if !item.AllowSkip || item.SkipAfterSec != 5 || item.ClickURL != "https://brand-a.example/offer" || item.CTALabel != "Shop now" {
		t.Fatalf("break should inherit the library link and keep skip settings: %+v", item)
	}
	vast := renderVAST(item)
	var doc struct {
		Linear struct {
			SkipOffset   string `xml:"skipoffset,attr"`
			ClickThrough string `xml:"VideoClicks>ClickThrough"`
		} `xml:"Ad>InLine>Creatives>Creative>Linear"`
	}
	if err := xml.Unmarshal([]byte(vast), &doc); err != nil {
		t.Fatalf("VAST is not well-formed: %v\n%s", err, vast)
	}
	if doc.Linear.SkipOffset != "00:00:05.000" || doc.Linear.ClickThrough != "https://brand-a.example/offer" {
		t.Fatalf("VAST skip/click contract = %+v\n%s", doc.Linear, vast)
	}

	selection.ClickThroughURL, selection.CTALabel, selection.AllowSkip = "https://override.example/app", "", false
	plan, err = buildPlaybackPlan(job, []PlaybackSelection{selection}, brands, request)
	if err != nil {
		t.Fatal(err)
	}
	if item := plan.Breaks[0]; item.ClickURL != "https://override.example/app" || item.CTALabel != "Shop now" || item.SkipAfterSec != 0 ||
		strings.Contains(renderVAST(item), "skipoffset") {
		t.Fatalf("per-break link should override and non-skippable ads must omit skipoffset: %+v", item)
	}

	for _, bad := range []PlaybackSelection{
		{Time: 60, BrandID: brands[0].BrandID, CreativeID: brands[0].Creatives[0].ID, Source: "manual", AllowSkip: true, SkipAfterSec: 15},
		{Time: 60, BrandID: brands[0].BrandID, CreativeID: brands[0].Creatives[0].ID, Source: "manual", AllowSkip: true, SkipAfterSec: -1},
		{Time: 60, BrandID: brands[0].BrandID, CreativeID: brands[0].Creatives[0].ID, Source: "manual", ClickThroughURL: "javascript:alert(1)"},
		{Time: 60, BrandID: brands[0].BrandID, CreativeID: brands[0].Creatives[0].ID, Source: "manual", ClickThroughURL: "https://x.example/a]]>b"},
	} {
		if _, err := buildPlaybackPlan(job, []PlaybackSelection{bad}, brands, request); err == nil {
			t.Fatalf("invalid skip or link settings should be rejected: %+v", bad)
		}
	}
}
