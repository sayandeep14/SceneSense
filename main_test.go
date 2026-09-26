package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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

func TestAnalysisCacheKeyIncludesConfiguredPipelineVersions(t *testing.T) {
	t.Setenv("ASR_PROVIDER", "groq")
	t.Setenv("GROQ_ASR_MODEL", "whisper-large-v3-turbo")
	t.Setenv("SARVAM_ASR_MODEL", "")
	t.Setenv("OPENAI_VISION_MODEL", "gpt-4o-mini")
	t.Setenv("OPENAI_BREAK_MODEL", "")
	t.Setenv("AI_BRANDS_PATH", filepath.Join("assets", "brands.json"))
	brandCatalogBytes, err := os.ReadFile(filepath.Join("assets", "brands.json"))
	if err != nil {
		t.Fatal(err)
	}
	brandCatalogSum := sha256.Sum256(brandCatalogBytes)
	brandCatalogHash := hex.EncodeToString(brandCatalogSum[:])
	contentHash := "fixture-content-hash"
	parts := strings.Join([]string{
		contentHash, "groq", "whisper-large-v3-turbo", "gpt-4o-mini", "scene-describe-v3-emotion",
		"gpt-4o-mini", "scene-boundary-judge-v4-paired-frames", "scene-fusion-pipeline-v1",
		"pyscenedetect-adaptive-threshold+twin-dissolve-v1", "clip-vit-b32-onnx-int8-d15189d", "yamnet-onnx-qaihub-0.63.0",
		"scene-fusion-v1", "pacing-v1", "text-embedding-3-small", "silencedetect:-32dB:0.45s", brandCatalogHash,
	}, "|")
	expected := sha256.Sum256([]byte(parts))
	first := currentAnalysisCacheKey(contentHash)
	if first != hex.EncodeToString(expected[:]) || first != currentAnalysisCacheKey(contentHash) {
		t.Fatalf("cache key = %q; expected stable key %q", first, hex.EncodeToString(expected[:]))
	}
	changedCatalog := filepath.Join(t.TempDir(), "brands.json")
	if err := os.WriteFile(changedCatalog, append(brandCatalogBytes, ' '), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AI_BRANDS_PATH", changedCatalog)
	if currentAnalysisCacheKey(contentHash) == first {
		t.Fatal("cache key did not change after the brand catalogue changed")
	}
	t.Setenv("AI_BRANDS_PATH", filepath.Join("assets", "brands.json"))
	t.Setenv("OPENAI_VISION_MODEL", "different-model")
	if currentAnalysisCacheKey(contentHash) == first {
		t.Fatal("cache key did not change after the vision model changed")
	}
	t.Setenv("OPENAI_VISION_MODEL", "gpt-4o-mini")
	t.Setenv("ASR_PROVIDER", "sarvam")
	t.Setenv("SARVAM_ASR_MODEL", "saaras:v4")
	sarvamKey := currentAnalysisCacheKey(contentHash)
	if sarvamKey == first {
		t.Fatal("cache key did not change after the ASR provider changed")
	}
	parts = strings.Join([]string{
		contentHash, "sarvam", "saaras:v4", "gpt-4o-mini", "scene-describe-v3-emotion",
		"gpt-4o-mini", "scene-boundary-judge-v4-paired-frames", "scene-fusion-pipeline-v1",
		"pyscenedetect-adaptive-threshold+twin-dissolve-v1", "clip-vit-b32-onnx-int8-d15189d", "yamnet-onnx-qaihub-0.63.0",
		"scene-fusion-v1", "pacing-v1", "text-embedding-3-small", "silencedetect:-32dB:0.45s", brandCatalogHash,
	}, "|")
	expected = sha256.Sum256([]byte(parts))
	if sarvamKey != hex.EncodeToString(expected[:]) {
		t.Fatalf("Sarvam cache key = %q; expected Python-compatible key %q", sarvamKey, hex.EncodeToString(expected[:]))
	}
	t.Setenv("OPENAI_BREAK_MODEL", "another-break-model")
	if currentAnalysisCacheKey(contentHash) == sarvamKey {
		t.Fatal("cache key did not change after the break model changed")
	}
}

func TestGoCacheKeyMatchesPythonWorker(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	t.Setenv("ASR_PROVIDER", "sarvam")
	t.Setenv("SARVAM_ASR_MODEL", "saaras:v4")
	t.Setenv("OPENAI_VISION_MODEL", "gpt-4o-mini")
	t.Setenv("OPENAI_BREAK_MODEL", "gpt-4o-mini")
	command := exec.Command(python, "-c", "import sys; sys.path.insert(0, 'ai'); import worker; print(worker._cache_key('fixture-hash'))")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("python cache key failed: %v: %s", err, output)
	}
	if got, want := currentAnalysisCacheKey("fixture-hash"), strings.TrimSpace(string(output)); got != want {
		t.Fatalf("Go cache key %q differs from Python key %q", got, want)
	}
}

func TestSelectedASRProviderControlsAvailability(t *testing.T) {
	t.Setenv("GROQ_API_KEY", "")
	t.Setenv("SARVAM_API_KEY", "sarvam-test-key")
	t.Setenv("ASR_PROVIDER", "sarvam")
	if !asrConfigured() {
		t.Fatal("Sarvam-only configuration was disabled")
	}
	t.Setenv("ASR_PROVIDER", "groq")
	if asrConfigured() {
		t.Fatal("Groq was enabled without its key")
	}
}

func testServer(t *testing.T) (*server, string) {
	t.Helper()
	dir := t.TempDir()
	app := newServer(slog.New(slog.NewTextHandler(io.Discard, nil)), dir)
	app.adLibraryDir = filepath.Join(dir, "ads")
	return app, dir
}

func TestHealthEndpoint(t *testing.T) {
	app, _ := testServer(t)
	response := httptest.NewRecorder()
	app.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("health status = %d, want %d", response.Code, http.StatusOK)
	}
	var payload map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	if payload["status"] != "ok" {
		t.Fatalf("health status = %q, want ok", payload["status"])
	}
}

func TestDemoAccessGateProtectsUIAndAPIButAllowsHealth(t *testing.T) {
	t.Setenv("DEMO_ACCESS_PASSWORD", "unit-test-demo-password")
	app, _ := testServer(t)
	handler := app.routes()

	for _, path := range []string{"/", "/api/jobs"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusUnauthorized {
			t.Errorf("unauthenticated %s status = %d, want 401", path, response.Code)
		}
		if response.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("unauthenticated %s response lacks Basic Auth challenge", path)
		}
	}

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.SetBasicAuth("demo", "wrong-password")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Errorf("wrong-password status = %d, want 401", response.Code)
	}

	request = httptest.NewRequest(http.MethodGet, "/", nil)
	request.SetBasicAuth("demo", "unit-test-demo-password")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Make every break") {
		t.Errorf("valid demo credentials did not serve UI: status=%d", response.Code)
	}

	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if response.Code != http.StatusOK {
		t.Errorf("unauthenticated healthcheck status = %d, want 200", response.Code)
	}
}

func TestJobArtifactsRestoreAcrossServerRestart(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	job := Job{
		ID: "fixture-job", FileName: "episode.mp4", Status: "completed", Stage: "scene_evidence_complete",
		ContentHash: "fixture-hash", VideoURL: "/media/fixture-job",
		Media: MediaInfo{DurationSeconds: 12, VideoCodec: "h264", AudioCodec: "aac", HasAudio: true},
		Transcript: &Transcript{
			Language: "bengali", Duration: 12, Model: "whisper-fixture", SceneAnalysisStatus: "complete",
			SceneModel: "vision-fixture", ScenePromptVersion: "fixture-v1",
			Scenes:           []SceneEvidence{{SceneID: "scene-01", Start: 0, End: 10, Summary: "A quiet conversation", Confidence: 0.9}},
			SilenceIntervals: []SilenceInterval{{Start: 4, End: 5, Duration: 1}},
		},
	}
	if err := os.WriteFile(filepath.Join(dir, job.ID+".mp4"), []byte("media"), 0o600); err != nil {
		t.Fatal(err)
	}
	writer := newServer(logger, dir)
	if err := writer.persistJob(job); err != nil {
		t.Fatalf("persist job: %v", err)
	}

	restored := newServer(logger, dir)
	got, exists := restored.jobs[job.ID]
	if !exists || restored.byHash[job.ContentHash] != job.ID {
		t.Fatal("job or content-hash index was not restored")
	}
	if got.Transcript == nil || len(got.Transcript.Scenes) != 1 || got.Transcript.SceneModel != "vision-fixture" || len(got.Transcript.SilenceIntervals) != 1 {
		t.Fatalf("AI evidence did not survive restart: %+v", got.Transcript)
	}
}

func TestInterruptedJobRestoresAsRetryableFailure(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	job := Job{ID: "interrupted-job", FileName: "episode.mp4", Status: "processing", ContentHash: "interrupted-hash"}
	if err := os.WriteFile(filepath.Join(dir, job.ID+".mp4"), []byte("media"), 0o600); err != nil {
		t.Fatal(err)
	}
	writer := newServer(logger, dir)
	if err := writer.persistJob(job); err != nil {
		t.Fatalf("persist job: %v", err)
	}

	restored := newServer(logger, dir)
	got := restored.jobs[job.ID]
	if got.Status != "failed" || got.Stage != "analysis_interrupted" || got.Message == "" {
		t.Fatalf("interrupted job state = %+v, want retryable failure", got)
	}
}

func TestUploadRejectsNonMP4(t *testing.T) {
	app, _ := testServer(t)
	body, contentType := multipartBody(t, "story.mov", []byte("not a video"))
	request := httptest.NewRequest(http.MethodPost, "/api/jobs", body)
	request.Header.Set("Content-Type", contentType)
	response := httptest.NewRecorder()
	app.routes().ServeHTTP(response, request)
	if response.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("upload status = %d, want %d; body=%s", response.Code, http.StatusUnsupportedMediaType, response.Body.String())
	}
}

func TestUploadVideoIntakeAndDeduplication(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe is not installed")
	}
	app, storage := testServer(t)
	videoPath := filepath.Join(t.TempDir(), "fixture.mp4")
	command := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "color=c=black:s=640x360:d=2:r=25", "-f", "lavfi", "-i", "sine=frequency=440:duration=2", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", videoPath)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create MP4 fixture: %v: %s", err, output)
	}
	video, err := os.ReadFile(videoPath)
	if err != nil {
		t.Fatal(err)
	}

	first := uploadRequest(t, app, "episode.mp4", video)
	if first.Code != http.StatusCreated {
		t.Fatalf("first upload status = %d, want %d; body=%s", first.Code, http.StatusCreated, first.Body.String())
	}
	var job Job
	if err := json.Unmarshal(first.Body.Bytes(), &job); err != nil {
		t.Fatalf("decode job: %v", err)
	}
	if job.Status != "ready" || job.Stage != "media_intake_complete" {
		t.Fatalf("job state = %s/%s, want ready/media_intake_complete", job.Status, job.Stage)
	}
	if job.Media.Width != 640 || job.Media.Height != 360 || !job.Media.HasAudio {
		t.Fatalf("unexpected inspected media: %+v", job.Media)
	}
	if job.ContentHash == "" || job.FileSize != int64(len(video)) {
		t.Fatalf("missing content hash or incorrect file size: %+v", job)
	}
	if _, err := os.Stat(filepath.Join(storage, job.ID+".mp4")); err != nil {
		t.Fatalf("stored upload not found: %v", err)
	}
	mediaResponse := httptest.NewRecorder()
	app.routes().ServeHTTP(mediaResponse, httptest.NewRequest(http.MethodGet, job.VideoURL, nil))
	if mediaResponse.Code != http.StatusOK || mediaResponse.Body.Len() != len(video) {
		t.Fatalf("source playback response = status %d, bytes %d; want status 200 and %d bytes", mediaResponse.Code, mediaResponse.Body.Len(), len(video))
	}
	jobResponse := httptest.NewRecorder()
	app.routes().ServeHTTP(jobResponse, httptest.NewRequest(http.MethodGet, "/api/jobs/"+job.ID, nil))
	if jobResponse.Code != http.StatusOK {
		t.Fatalf("job detail status = %d, want %d", jobResponse.Code, http.StatusOK)
	}

	second := uploadRequest(t, app, "episode.mp4", video)
	if second.Code != http.StatusOK {
		t.Fatalf("duplicate upload status = %d, want %d", second.Code, http.StatusOK)
	}
	var duplicate Job
	if err := json.Unmarshal(second.Body.Bytes(), &duplicate); err != nil {
		t.Fatalf("decode duplicate job: %v", err)
	}
	if duplicate.ID != job.ID {
		t.Fatalf("duplicate job id = %s, want original %s", duplicate.ID, job.ID)
	}
}

func TestStudioPageIsServed(t *testing.T) {
	app, _ := testServer(t)
	response := httptest.NewRecorder()
	app.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("studio page status = %d, want %d", response.Code, http.StatusOK)
	}
	if !strings.Contains(response.Body.String(), "Make every break") {
		t.Fatal("studio page does not contain the expected UI")
	}
}

func TestUploadRejectsInvalidVideo(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe is not installed")
	}
	app, _ := testServer(t)
	body, contentType := multipartBody(t, "broken.mp4", []byte("not an mp4"))
	request := httptest.NewRequest(http.MethodPost, "/api/jobs", body)
	request.Header.Set("Content-Type", contentType)
	response := httptest.NewRecorder()
	app.routes().ServeHTTP(response, request)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid video status = %d, want %d; body=%s", response.Code, http.StatusUnprocessableEntity, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "valid video") {
		t.Fatalf("invalid video response lacks useful error: %s", response.Body.String())
	}
}

func TestUploadRejectsVideoWithoutAudio(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe is not installed")
	}
	app, _ := testServer(t)
	videoPath := filepath.Join(t.TempDir(), "silent.mp4")
	command := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "color=c=black:s=320x180:d=1:r=24", "-c:v", "libx264", "-pix_fmt", "yuv420p", videoPath)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("create silent MP4 fixture: %v: %s", err, output)
	}
	video, err := os.ReadFile(videoPath)
	if err != nil {
		t.Fatal(err)
	}
	response := uploadRequest(t, app, "silent.mp4", video)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("silent video status = %d, want %d; body=%s", response.Code, http.StatusUnprocessableEntity, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "no audio track") {
		t.Fatalf("silent video response lacks useful error: %s", response.Body.String())
	}
}

func uploadRequest(t *testing.T, app *server, filename string, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	body, contentType := multipartBody(t, filename, data)
	request := httptest.NewRequest(http.MethodPost, "/api/jobs", body)
	request.Header.Set("Content-Type", contentType)
	response := httptest.NewRecorder()
	app.routes().ServeHTTP(response, request)
	return response
}

func multipartBody(t *testing.T, filename string, data []byte) (*bytes.Buffer, string) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("video", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return &body, writer.FormDataContentType()
}
