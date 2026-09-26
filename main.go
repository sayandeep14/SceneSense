package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web/*
var webFiles embed.FS

const (
	defaultAddr       = ":8080"
	defaultUploadRoot = "data/uploads"
	maxUploadBytes    = 500 << 20
	scenePromptVer    = "scene-evidence-v2"
	analysisVersion   = "phase2-sarvam-asr-v1"
)

type MediaInfo struct {
	DurationSeconds float64 `json:"durationSeconds"`
	Width           int     `json:"width"`
	Height          int     `json:"height"`
	VideoCodec      string  `json:"videoCodec"`
	AudioCodec      string  `json:"audioCodec"`
	HasAudio        bool    `json:"hasAudio"`
	Format          string  `json:"format"`
}

type Job struct {
	ID          string      `json:"id"`
	FileName    string      `json:"fileName"`
	FileSize    int64       `json:"fileSize"`
	Status      string      `json:"status"`
	Stage       string      `json:"stage"`
	Progress    int         `json:"progress"`
	CreatedAt   time.Time   `json:"createdAt"`
	ContentHash string      `json:"contentHash"`
	Media       MediaInfo   `json:"media"`
	VideoURL    string      `json:"videoUrl"`
	Message     string      `json:"message"`
	Transcript  *Transcript `json:"transcript,omitempty"`
}

type server struct {
	logger        *slog.Logger
	uploadDir     string
	jobsMu        sync.RWMutex
	jobs          map[string]Job
	byHash        map[string]string
	aiEnabled     bool
	pythonBin     string
	workerPath    string
	brandsPath    string
	demoPassword  string
	analysisSlots chan struct{}
}

func newServer(logger *slog.Logger, uploadDir string) *server {
	pythonBin := strings.TrimSpace(os.Getenv("PYTHON_BIN"))
	if pythonBin == "" {
		pythonBin = "python3"
	}
	workerPath := strings.TrimSpace(os.Getenv("AI_WORKER_PATH"))
	if workerPath == "" {
		workerPath = filepath.Join("ai", "worker.py")
	}
	brandsPath := strings.TrimSpace(os.Getenv("AI_BRANDS_PATH"))
	if brandsPath == "" {
		brandsPath = filepath.Join("assets", "brands.json")
	}
	app := &server{
		logger: logger, uploadDir: uploadDir, jobs: make(map[string]Job), byHash: make(map[string]string),
		pythonBin: pythonBin, workerPath: workerPath, brandsPath: brandsPath,
		demoPassword: strings.TrimSpace(os.Getenv("DEMO_ACCESS_PASSWORD")), analysisSlots: make(chan struct{}, 1),
	}
	app.restoreJobs()
	return app
}

func (s *server) jobArtifactPath(id string) string {
	return filepath.Join(s.uploadDir, id+".job.json")
}

func (s *server) persistJob(job Job) error {
	file, err := os.CreateTemp(s.uploadDir, ".job-*.tmp")
	if err != nil {
		return err
	}
	tempPath := file.Name()
	defer os.Remove(tempPath)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	if err := json.NewEncoder(file).Encode(job); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, s.jobArtifactPath(job.ID))
}

func (s *server) restoreJobs() {
	artifacts, err := filepath.Glob(filepath.Join(s.uploadDir, "*.job.json"))
	if err != nil {
		s.logger.Warn("could not scan saved job artifacts", "error", err)
		return
	}
	for _, artifact := range artifacts {
		data, err := os.ReadFile(artifact)
		if err != nil {
			s.logger.Warn("could not read saved job artifact", "path", filepath.Base(artifact), "error", err)
			continue
		}
		var job Job
		if err := json.Unmarshal(data, &job); err != nil || job.ID == "" || filepath.Base(artifact) != job.ID+".job.json" {
			s.logger.Warn("skipping invalid saved job artifact", "path", filepath.Base(artifact))
			continue
		}
		if _, err := os.Stat(s.uploadPath(job.ID)); err != nil {
			continue
		}
		if job.Status == "queued" || job.Status == "processing" {
			job.Status, job.Stage = "failed", "analysis_interrupted"
			job.Message = "Analysis was interrupted by a service restart. Retry it to continue."
			if err := s.persistJob(job); err != nil {
				s.logger.Warn("could not update interrupted job artifact", "job_id", job.ID, "error", err)
			}
		}
		s.jobs[job.ID] = job
		if job.ContentHash != "" {
			s.byHash[job.ContentHash] = job.ID
		}
	}
	if len(s.jobs) > 0 {
		s.logger.Info("restored saved jobs", "count", len(s.jobs))
	}
}

func currentAnalysisCacheKey(contentHash string) string {
	provider := strings.ToLower(strings.TrimSpace(os.Getenv("ASR_PROVIDER")))
	if provider == "" {
		provider = "groq"
	}
	asrModel := strings.TrimSpace(os.Getenv("GROQ_ASR_MODEL"))
	if provider == "sarvam" {
		asrModel = strings.TrimSpace(os.Getenv("SARVAM_ASR_MODEL"))
		if asrModel == "" {
			asrModel = "saaras:v4"
		}
	} else if asrModel == "" {
		asrModel = "whisper-large-v3-turbo"
	}
	sceneModel := strings.TrimSpace(os.Getenv("OPENAI_VISION_MODEL"))
	if sceneModel == "" {
		sceneModel = "gpt-4o-mini"
	}
	parts := strings.Join([]string{
		contentHash, provider, asrModel, sceneModel, scenePromptVer, analysisVersion,
		"16", "0.30", "300", "silencedetect:-32dB:0.45s",
	}, "|")
	sum := sha256.Sum256([]byte(parts))
	return hex.EncodeToString(sum[:])
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	webRoot, err := fs.Sub(webFiles, "web")
	if err != nil {
		panic(err)
	}
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /api/jobs", s.listJobs)
	mux.HandleFunc("POST /api/jobs", s.createJob)
	mux.HandleFunc("POST /api/jobs/{id}/transcribe", s.retryTranscription)
	mux.HandleFunc("GET /api/jobs/{id}", s.getJob)
	mux.HandleFunc("GET /media/{id}", s.getMedia)
	mux.Handle("GET /", http.FileServer(http.FS(webRoot)))
	return s.withLogging(s.withDemoAccess(mux))
}

func (s *server) withDemoAccess(next http.Handler) http.Handler {
	if s.demoPassword == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Railway needs this endpoint unauthenticated to determine deployment readiness.
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		username, password, ok := r.BasicAuth()
		passwordMatches := subtle.ConstantTimeCompare([]byte(password), []byte(s.demoPassword)) == 1
		if !ok || username != "demo" || !passwordMatches {
			w.Header().Set("WWW-Authenticate", `Basic realm="SceneSense Demo", charset="UTF-8"`)
			http.Error(w, "Demo access required.", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) retryTranscription(w http.ResponseWriter, r *http.Request) {
	if !s.aiEnabled {
		writeError(w, http.StatusServiceUnavailable, "Bengali transcription is not configured on this service.")
		return
	}
	id := r.PathValue("id")
	s.jobsMu.Lock()
	job, exists := s.jobs[id]
	if !exists {
		s.jobsMu.Unlock()
		writeError(w, http.StatusNotFound, "Analysis job not found.")
		return
	}
	if job.Status == "queued" || job.Status == "processing" {
		s.jobsMu.Unlock()
		writeJSON(w, http.StatusConflict, job)
		return
	}
	job.Status, job.Stage, job.Progress = "queued", "transcription_queued", 20
	job.Message = "Bengali speech transcription is queued."
	job.Transcript = nil
	s.jobs[id] = job
	if err := s.persistJob(job); err != nil {
		s.logger.Error("persist retried job state", "job_id", id, "error", err)
	}
	s.jobsMu.Unlock()
	go s.transcribeJob(id)
	writeJSON(w, http.StatusAccepted, job)
}

func (s *server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		s.logger.Info("http request", "method", r.Method, "path", r.URL.Path, "duration_ms", time.Since(started).Milliseconds())
	})
}

func (s *server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": "contextual-ad-lab"})
}

func (s *server) listJobs(w http.ResponseWriter, _ *http.Request) {
	s.jobsMu.RLock()
	jobs := make([]Job, 0, len(s.jobs))
	for _, job := range s.jobs {
		jobs = append(jobs, job)
	}
	s.jobsMu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs})
}

func (s *server) createJob(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes+(1<<20))
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "Choose an MP4 file smaller than 500 MB.")
		return
	}
	file, header, err := r.FormFile("video")
	if err != nil {
		writeError(w, http.StatusBadRequest, "A video file is required.")
		return
	}
	defer file.Close()
	if header.Size > maxUploadBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "This file is larger than the 500 MB upload limit.")
		return
	}
	if !strings.EqualFold(filepath.Ext(header.Filename), ".mp4") {
		writeError(w, http.StatusUnsupportedMediaType, "For this first release, upload an MP4 video.")
		return
	}

	if err := os.MkdirAll(s.uploadDir, 0o750); err != nil {
		s.logger.Error("create upload directory", "error", err)
		writeError(w, http.StatusInternalServerError, "Upload storage is unavailable.")
		return
	}
	temp, err := os.CreateTemp(s.uploadDir, "incoming-*.mp4")
	if err != nil {
		s.logger.Error("create upload temp file", "error", err)
		writeError(w, http.StatusInternalServerError, "Could not start the upload.")
		return
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	hasher := sha256.New()
	written, err := io.Copy(io.MultiWriter(temp, hasher), io.LimitReader(file, maxUploadBytes+1))
	if err != nil {
		_ = temp.Close()
		writeError(w, http.StatusBadRequest, "The upload could not be read completely.")
		return
	}
	if written > maxUploadBytes {
		_ = temp.Close()
		writeError(w, http.StatusRequestEntityTooLarge, "This file is larger than the 500 MB upload limit.")
		return
	}
	if err := temp.Close(); err != nil {
		writeError(w, http.StatusInternalServerError, "Could not save the uploaded video.")
		return
	}
	contentHash := hex.EncodeToString(hasher.Sum(nil))

	s.jobsMu.RLock()
	existingID, exists := s.byHash[contentHash]
	existing := s.jobs[existingID]
	s.jobsMu.RUnlock()
	if exists {
		if !s.aiEnabled || existing.Status == "queued" || existing.Status == "processing" ||
			(existing.Transcript != nil && existing.Transcript.CacheKey == currentAnalysisCacheKey(contentHash)) {
			writeJSON(w, http.StatusOK, existing)
			return
		}
		s.jobsMu.Lock()
		existing = s.jobs[existingID]
		if existing.Status != "queued" && existing.Status != "processing" &&
			(existing.Transcript == nil || existing.Transcript.CacheKey != currentAnalysisCacheKey(contentHash)) {
			existing.Status, existing.Stage, existing.Progress = "queued", "transcription_queued", 20
			existing.Message = "Analysis version changed. Reusing the video and checking the matching AI cache."
			existing.Transcript = nil
			s.jobs[existingID] = existing
			if err := s.persistJob(existing); err != nil {
				s.logger.Error("persist version-invalidated job", "job_id", existingID, "error", err)
			}
			go s.transcribeJob(existingID)
		}
		s.jobsMu.Unlock()
		writeJSON(w, http.StatusOK, existing)
		return
	}

	media, err := probeVideo(r.Context(), tempPath)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if !media.HasAudio {
		writeError(w, http.StatusUnprocessableEntity, "This video has no audio track. Audio is needed to find natural dialogue pauses.")
		return
	}
	if media.DurationSeconds <= 0 || media.DurationSeconds > 3*60*60 {
		writeError(w, http.StatusUnprocessableEntity, "The video duration must be between 1 second and 3 hours.")
		return
	}

	id, err := newID()
	if err != nil {
		s.logger.Error("generate upload id", "error", err)
		writeError(w, http.StatusInternalServerError, "Could not create an analysis job.")
		return
	}
	videoPath := filepath.Join(s.uploadDir, id+".mp4")
	if err := os.Rename(tempPath, videoPath); err != nil {
		s.logger.Error("commit uploaded video", "error", err)
		writeError(w, http.StatusInternalServerError, "Could not store the uploaded video.")
		return
	}
	job := Job{
		ID: id, FileName: filepath.Base(header.Filename), FileSize: written, Status: "ready", Stage: "media_intake_complete", Progress: 18,
		CreatedAt: time.Now().UTC(), ContentHash: contentHash, Media: media, VideoURL: "/media/" + id,
		Message: "Video validated and ready for Bengali speech and scene analysis.",
	}
	if s.aiEnabled {
		job.Status, job.Stage, job.Progress = "queued", "transcription_queued", 20
		job.Message = "Video is ready. Bengali speech transcription is queued."
	}
	if err := s.persistJob(job); err != nil {
		s.logger.Error("persist initial job artifact", "job_id", id, "error", err)
		_ = os.Remove(videoPath)
		writeError(w, http.StatusInternalServerError, "Could not save the analysis job.")
		return
	}
	s.jobsMu.Lock()
	s.jobs[id] = job
	s.byHash[contentHash] = id
	s.jobsMu.Unlock()
	if s.aiEnabled {
		go s.transcribeJob(id)
	}
	writeJSON(w, http.StatusCreated, job)
}

func (s *server) getJob(w http.ResponseWriter, r *http.Request) {
	s.jobsMu.RLock()
	job, exists := s.jobs[r.PathValue("id")]
	s.jobsMu.RUnlock()
	if !exists {
		writeError(w, http.StatusNotFound, "Analysis job not found.")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *server) getMedia(w http.ResponseWriter, r *http.Request) {
	s.jobsMu.RLock()
	_, exists := s.jobs[r.PathValue("id")]
	s.jobsMu.RUnlock()
	if !exists {
		http.NotFound(w, r)
		return
	}
	path := filepath.Join(s.uploadDir, r.PathValue("id")+".mp4")
	http.ServeFile(w, r, path)
}

func probeVideo(ctx context.Context, path string) (MediaInfo, error) {
	// Kept separate from the request handler so probe output can be tested and replaced cleanly.
	commandCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(commandCtx, "ffprobe", "-v", "error", "-show_entries", "format=duration,format_name:stream=codec_type,codec_name,width,height", "-of", "json", path)
	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return MediaInfo{}, fmt.Errorf("This file could not be read as a valid video. Check that it is a playable MP4.")
		}
		return MediaInfo{}, fmt.Errorf("Video inspection failed. Make sure ffprobe is installed.")
	}
	var raw struct {
		Format struct {
			Duration string `json:"duration"`
			Name     string `json:"format_name"`
		} `json:"format"`
		Streams []struct {
			Type   string `json:"codec_type"`
			Codec  string `json:"codec_name"`
			Width  int    `json:"width"`
			Height int    `json:"height"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(output, &raw); err != nil {
		return MediaInfo{}, fmt.Errorf("Video inspection returned invalid metadata.")
	}
	duration, _ := strconv.ParseFloat(raw.Format.Duration, 64)
	info := MediaInfo{DurationSeconds: duration, Format: raw.Format.Name}
	for _, stream := range raw.Streams {
		switch stream.Type {
		case "video":
			if info.VideoCodec == "" {
				info.VideoCodec, info.Width, info.Height = stream.Codec, stream.Width, stream.Height
			}
		case "audio":
			if !info.HasAudio {
				info.HasAudio, info.AudioCodec = true, stream.Codec
			}
		}
	}
	if info.VideoCodec == "" {
		return MediaInfo{}, fmt.Errorf("No video track was found in this file.")
	}
	return info, nil
}

func newID() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", bytes[0:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:16]), nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	demoPassword := strings.TrimSpace(os.Getenv("DEMO_ACCESS_PASSWORD"))
	if strings.TrimSpace(os.Getenv("RAILWAY_ENVIRONMENT")) != "" && demoPassword == "" {
		logger.Error("DEMO_ACCESS_PASSWORD must be configured for Railway deployments")
		os.Exit(1)
	}
	if demoPassword != "" {
		logger.Info("demo access gate enabled", "username", "demo")
	}
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = defaultAddr
	}
	uploadDir := os.Getenv("UPLOAD_DIR")
	if uploadDir == "" {
		uploadDir = defaultUploadRoot
	}
	if err := os.MkdirAll(uploadDir, 0o750); err != nil {
		logger.Error("create upload directory", "error", err)
		os.Exit(1)
	}
	app := newServer(logger, uploadDir)
	app.aiEnabled = strings.TrimSpace(os.Getenv("GROQ_API_KEY")) != ""
	if !app.aiEnabled {
		logger.Warn("GROQ_API_KEY is not set; uploads will stop after media intake")
	}
	httpServer := &http.Server{Addr: addr, Handler: app.routes(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Minute, WriteTimeout: 10 * time.Minute, IdleTimeout: 60 * time.Second}
	logger.Info("server starting", "addr", addr, "upload_dir", uploadDir)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}
