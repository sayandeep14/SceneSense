package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestEvaluateBrandForSceneEnforcesUnseenNegativeContexts(t *testing.T) {
	scene := SceneEvidence{
		Summary: "Friends share drinks and alcohol at a rooftop party", Activities: []string{"drinking", "celebration"},
		Tone: []string{"festive"}, Confidence: 0.9, DialogueState: "completed_thought",
	}
	brand := CatalogBrand{BrandID: "custom_juice_1", DisplayName: "Brand Juice", Category: "beverage",
		TargetContexts: []string{"celebration", "party"}, NegativeContexts: []string{"alcohol"}}
	match := evaluateBrandForScene(scene, brand)
	if !match.Blocked || match.Recommended || !strings.Contains(strings.Join(match.BlockedContexts, ","), "alcohol") {
		t.Fatalf("a negative context outside the shared taxonomy must still block: %+v", match)
	}

	brand.NegativeContexts = []string{"funeral"}
	scene.SensitiveContexts = []string{"mourning"}
	if match := evaluateBrandForScene(scene, brand); !match.Blocked || !strings.Contains(strings.Join(match.BlockedContexts, ","), "grief") {
		t.Fatalf("taxonomy aliases must block a funeral-negative brand after a mourning scene: %+v", match)
	}

	scene.SensitiveContexts = nil
	match = evaluateBrandForScene(scene, brand)
	if match.Blocked || !match.Recommended || match.FitSource != "context" || len(match.MatchedContexts) != 2 {
		t.Fatalf("activity and summary matches should recommend an unscored brand: %+v", match)
	}

	scene.Confidence = 0.4
	if match := evaluateBrandForScene(scene, brand); !match.Blocked || match.BlockedContexts[len(match.BlockedContexts)-1] != "uncertain_scene" {
		t.Fatalf("uncertain scenes must block every brand: %+v", match)
	}
}

func TestEvaluateBrandForSceneKeepsAIBlocksAndScores(t *testing.T) {
	scene := SceneEvidence{Summary: "A calm chat", Confidence: 0.9, DialogueState: "completed_thought",
		BrandMatches: []SceneBrandMatch{{BrandID: "brand_a", FitScore: 0.8, Reason: "fits", Blocked: true, BlockedContexts: []string{"food"}}}}
	match := evaluateBrandForScene(scene, CatalogBrand{BrandID: "brand_a", DisplayName: "Brand A"})
	if !match.Blocked || match.FitSource != "ai" || match.FitScore != 0.8 {
		t.Fatalf("AI evidence and its block must be preserved: %+v", match)
	}
}

func adUploadRequest(t *testing.T, fields map[string]string, videoPath string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, value := range fields {
		if err := writer.WriteField(key, value); err != nil {
			t.Fatal(err)
		}
	}
	if videoPath != "" {
		part, err := writer.CreateFormFile("video", filepath.Base(videoPath))
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(videoPath)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = part.Write(data)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/ads", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	return request
}

func TestUploadedAdPersistsAndIsSuggestedForMatchingScene(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe is not installed")
	}
	app, dir := testServer(t)
	videoPath := filepath.Join(t.TempDir(), "tea-ad.mp4")
	command := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "color=c=black:s=320x180:d=6:r=24",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", videoPath)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create ad fixture: %v %s", err, output)
	}

	missing := httptest.NewRecorder()
	app.routes().ServeHTTP(missing, adUploadRequest(t, map[string]string{"brand_name": "Brand Tea"}, videoPath))
	if missing.Code != http.StatusBadRequest {
		t.Fatalf("upload without target contexts status=%d, want 400", missing.Code)
	}
	builtin := httptest.NewRecorder()
	app.routes().ServeHTTP(builtin, adUploadRequest(t, map[string]string{"brand_name": "brand a", "target_contexts": "tea"}, videoPath))
	if builtin.Code != http.StatusConflict {
		t.Fatalf("upload reusing a built-in brand name status=%d, want 409", builtin.Code)
	}

	badLink := httptest.NewRecorder()
	app.routes().ServeHTTP(badLink, adUploadRequest(t, map[string]string{"brand_name": "Brand Tea", "target_contexts": "tea",
		"click_through_url": "ftp://brand-tea.example"}, videoPath))
	if badLink.Code != http.StatusBadRequest {
		t.Fatalf("upload with a non-http link status=%d, want 400", badLink.Code)
	}

	fields := map[string]string{"brand_name": "Brand Tea", "category": "beverage/tea",
		"target_contexts": "Conversation, family\nmorning", "negative_contexts": "funeral; hospital",
		"click_through_url": " https://brand-tea.example/app ", "cta_label": "Install app"}
	response := httptest.NewRecorder()
	app.routes().ServeHTTP(response, adUploadRequest(t, fields, videoPath))
	if response.Code != http.StatusCreated {
		t.Fatalf("upload status=%d body=%s", response.Code, response.Body.String())
	}
	var created struct {
		Brand CatalogBrand `json:"brand"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	brand := created.Brand
	if !strings.HasPrefix(brand.BrandID, "custom_brand_tea_") || len(brand.Creatives) != 1 || brand.Creatives[0].DurationSec != 6 ||
		strings.Join(brand.TargetContexts, ",") != "conversation,family,morning" || strings.Join(brand.NegativeContexts, ",") != "funeral,hospital" {
		t.Fatalf("uploaded brand = %+v", brand)
	}
	second := httptest.NewRecorder()
	app.routes().ServeHTTP(second, adUploadRequest(t, fields, videoPath))
	if second.Code != http.StatusCreated {
		t.Fatalf("second upload status=%d body=%s", second.Code, second.Body.String())
	}

	// A fresh server over the same storage sees the uploaded brand, both creatives, and serves the files.
	restarted := newServer(slog.New(slog.NewTextHandler(io.Discard, nil)), dir)
	restarted.adLibraryDir = app.adLibraryDir
	catalog, err := restarted.loadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	persisted, ok := findBrand(catalog, brand.BrandID)
	if !ok || persisted.Source != brandSourceCustom || len(persisted.Creatives) != 2 ||
		persisted.ClickThroughURL != "https://brand-tea.example/app" || persisted.CTALabel != "Install app" {
		t.Fatalf("uploaded brand did not persist: %+v", persisted)
	}
	file := filepath.Base(persisted.Creatives[1].SourceURL)
	media := httptest.NewRecorder()
	restarted.routes().ServeHTTP(media, httptest.NewRequest(http.MethodGet, "/ads/"+brand.BrandID+"/"+file, nil))
	if media.Code != http.StatusOK || media.Header().Get("Content-Type") != "video/mp4" {
		t.Fatalf("uploaded creative status=%d type=%q", media.Code, media.Header().Get("Content-Type"))
	}

	job, _ := playbackFixture(t, 1800)
	job.Transcript.Scenes[0].Activities = []string{"conversation"}
	restarted.jobs[job.ID] = job
	suggestions := httptest.NewRecorder()
	restarted.routes().ServeHTTP(suggestions, httptest.NewRequest(http.MethodGet, "/api/jobs/fixture-job/ad-suggestions?time=60", nil))
	if suggestions.Code != http.StatusOK {
		t.Fatalf("suggestions status=%d body=%s", suggestions.Code, suggestions.Body.String())
	}
	var payload struct {
		SceneBefore *SceneEvidence `json:"scene_before"`
		Suggestions []adSuggestion `json:"suggestions"`
	}
	if err := json.Unmarshal(suggestions.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	var uploaded *adSuggestion
	for i := range payload.Suggestions {
		if payload.Suggestions[i].BrandID == brand.BrandID {
			uploaded = &payload.Suggestions[i]
		}
	}
	if payload.SceneBefore == nil || uploaded == nil || !uploaded.Recommended || uploaded.FitSource != "context" || len(uploaded.Creatives) != 2 {
		t.Fatalf("uploaded ad was not suggested for a matching scene: %+v", uploaded)
	}

	request := httptest.NewRequest(http.MethodPost, "/", nil)
	if _, err := buildPlaybackPlan(job, []PlaybackSelection{{Time: 60, BrandID: brand.BrandID,
		CreativeID: persisted.Creatives[0].ID, Source: "manual"}}, catalog, request); err != nil {
		t.Fatalf("a safe uploaded brand should be playable without AI scene scores: %v", err)
	}
	job.Transcript.Scenes[0].SensitiveContexts = []string{"funeral"}
	if _, err := buildPlaybackPlan(job, []PlaybackSelection{{Time: 60, BrandID: brand.BrandID,
		CreativeID: persisted.Creatives[0].ID, Source: "manual"}}, catalog, request); err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("an uploaded brand's negative context must block playback, got %v", err)
	}
}
