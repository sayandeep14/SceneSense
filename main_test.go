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
	t.Setenv("OPENAI_VISION_MODEL", "gpt-4o-mini")
	contentHash := "fixture-content-hash"
	parts := strings.Join([]string{
		contentHash, "whisper-large-v3-turbo", "gpt-4o-mini", "scene-evidence-v2",
		"phase2-cut-aware-v1", "16", "0.30", "300", "silencedetect:-32dB:0.45s",
	}, "|")
	expected := sha256.Sum256([]byte(parts))
	first := currentAnalysisCacheKey(contentHash)
	if first != hex.EncodeToString(expected[:]) || first != currentAnalysisCacheKey(contentHash) {
		t.Fatalf("cache key = %q; expected stable key %q", first, hex.EncodeToString(expected[:]))
	}
	t.Setenv("OPENAI_VISION_MODEL", "different-model")
	if currentAnalysisCacheKey(contentHash) == first {
		t.Fatal("cache key did not change after the vision model changed")
	}
}

func testServer(t *testing.T) (*server, string) {
	t.Helper()
	dir := t.TempDir()
	return newServer(slog.New(slog.NewTextHandler(io.Discard, nil)), dir), dir
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
