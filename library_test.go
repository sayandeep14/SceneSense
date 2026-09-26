package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRetryFromSceneAnalysisReusesTheSavedTranscript(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	app, dir := testServer(t)
	// A stand-in worker that reports which phase it was asked to redo and which transcript it was given.
	worker := filepath.Join(t.TempDir(), "worker.py")
	script := `import json,sys
r=json.load(sys.stdin)
t=r.get("transcript") or {"segments":[{"start":0,"end":1,"text":"নতুন"}]}
json.dump({"language":"bn","duration":10,"text":r.get("from_phase",""),"segments":t["segments"],"model":"m"},sys.stdout)`
	if err := os.WriteFile(worker, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	app.aiEnabled, app.pythonBin, app.workerPath = true, python, worker
	job := Job{ID: "retry-job", Status: "completed", ContentHash: "hash", Media: MediaInfo{DurationSeconds: 10},
		Transcript: &Transcript{Duration: 10, Model: "m", Text: "old", SceneAnalysisStatus: "unavailable",
			Segments: []TranscriptSegment{{Text: "পুরনো", Start: 0, End: 1}}, Scenes: []SceneEvidence{{SceneID: "stale"}}}}
	app.jobs[job.ID] = job
	if err := os.WriteFile(filepath.Join(dir, job.ID+".mp4"), []byte("video"), 0o600); err != nil {
		t.Fatal(err)
	}
	handler := app.routes()
	send := func(method, path, body string) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(method, path, strings.NewReader(body)))
		return response
	}
	waitDone := func() Job {
		for i := 0; i < 200; i++ {
			if current, _ := app.getJobSnapshot(job.ID); current.Status != "queued" && current.Status != "processing" {
				return current
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("retry did not finish")
		return Job{}
	}

	if bad := send(http.MethodPost, "/api/jobs/retry-job/retry", `{"from":"somewhere"}`); bad.Code != http.StatusBadRequest {
		t.Fatalf("unknown phase status=%d", bad.Code)
	}
	accepted := send(http.MethodPost, "/api/jobs/retry-job/retry", `{"from":"scene_analysis"}`)
	var queued Job
	if accepted.Code != http.StatusAccepted || json.Unmarshal(accepted.Body.Bytes(), &queued) != nil ||
		queued.Transcript == nil || len(queued.Transcript.Segments) != 1 || len(queued.Transcript.Scenes) != 0 {
		t.Fatalf("scene retry should keep only the transcript while it runs: %d %s", accepted.Code, accepted.Body.String())
	}
	done := waitDone()
	if done.Transcript == nil || done.Transcript.Text != "scene_analysis" || done.Transcript.Segments[0].Text != "পুরনো" {
		t.Fatalf("the worker did not receive the saved transcript for a scene retry: %+v", done.Transcript)
	}

	if again := send(http.MethodPost, "/api/jobs/retry-job/retry", `{"from":"transcription"}`); again.Code != http.StatusAccepted {
		t.Fatalf("transcription retry status=%d", again.Code)
	}
	done = waitDone()
	if done.Transcript.Text != "transcription" || done.Transcript.Segments[0].Text != "নতুন" {
		t.Fatalf("a transcription retry must not reuse the old transcript: %+v", done.Transcript)
	}

	// A programme with no dialogue still has a valid (empty) transcript to reuse.
	app.jobsMu.Lock()
	current := app.jobs[job.ID]
	current.Transcript = &Transcript{Duration: 10, Model: "m", Segments: []TranscriptSegment{}}
	app.jobs[job.ID] = current
	app.jobsMu.Unlock()
	if silent := send(http.MethodPost, "/api/jobs/retry-job/retry", `{"from":"scene_analysis"}`); silent.Code != http.StatusAccepted {
		t.Fatalf("scene retry with an empty transcript status=%d", silent.Code)
	}
	waitDone()
	app.jobsMu.Lock()
	current = app.jobs[job.ID]
	current.Transcript = nil
	app.jobs[job.ID] = current
	app.jobsMu.Unlock()
	if conflict := send(http.MethodPost, "/api/jobs/retry-job/retry", `{"from":"scene_analysis"}`); conflict.Code != http.StatusConflict {
		t.Fatalf("scene retry without a transcript status=%d", conflict.Code)
	}
}

func TestDeleteJobRemovesVideoSidecarAndItsCachesOnly(t *testing.T) {
	app, dir := testServer(t)
	job := Job{ID: "gone", Status: "completed", ContentHash: "hash-a"}
	app.jobs[job.ID], app.byHash[job.ContentHash] = job, job.ID
	if err := app.persistJob(job); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"gone.mp4":              "video",
		"analysis-one.json":     `{"content_hash":"hash-a"}`,
		"transcript-abc.json":   `{"content_hash":"hash-a"}`,
		"analysis-other.json":   `{"content_hash":"hash-b"}`,
		"transcript-other.json": `{"content_hash":"hash-b"}`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	app.jobs["busy"] = Job{ID: "busy", Status: "processing"}
	handler := app.routes()
	busy := httptest.NewRecorder()
	handler.ServeHTTP(busy, httptest.NewRequest(http.MethodDelete, "/api/jobs/busy", nil))
	if busy.Code != http.StatusConflict {
		t.Fatalf("deleting a job under analysis status=%d", busy.Code)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/api/jobs/gone", nil))
	if response.Code != http.StatusNoContent {
		t.Fatalf("delete status=%d body=%s", response.Code, response.Body.String())
	}
	for _, name := range []string{"gone.mp4", "gone.job.json", "analysis-one.json", "transcript-abc.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s should be deleted", name)
		}
	}
	for _, name := range []string{"analysis-other.json", "transcript-other.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s belongs to another video and must stay: %v", name, err)
		}
	}
	if _, ok := app.getJobSnapshot("gone"); ok || app.byHash["hash-a"] != "" {
		t.Fatal("the job is still listed")
	}
	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, httptest.NewRequest(http.MethodDelete, "/api/jobs/gone", nil))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("second delete status=%d", missing.Code)
	}
}

func TestRemoveUploadedAdsButNeverBuiltInBrands(t *testing.T) {
	app, _ := testServer(t)
	app.brandsPath = "assets/brands.json"
	brandDir := filepath.Join(app.adLibraryDir, "ads", "custom_tea_1")
	if err := os.MkdirAll(brandDir, 0o750); err != nil {
		t.Fatal(err)
	}
	custom := []CatalogBrand{{BrandID: "custom_tea_1", DisplayName: "Brand Tea", Category: "tea",
		TargetContexts: []string{"tea"}, Creatives: []CatalogCreative{
			{ID: "tea_15s", DurationSec: 15, Language: "bn", SourceURL: "ads/custom_tea_1/tea_15s.mp4"},
			{ID: "tea_20s", DurationSec: 20, Language: "bn", SourceURL: "ads/custom_tea_1/tea_20s.mp4"},
		}}}
	for _, creative := range custom[0].Creatives {
		if err := os.WriteFile(filepath.Join(app.adLibraryDir, creative.SourceURL), []byte("mp4"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.writeCustomCatalog(custom); err != nil {
		t.Fatal(err)
	}
	handler := app.routes()
	send := func(path string) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, path, nil))
		return response
	}
	if builtin := send("/api/ads/brand_a"); builtin.Code != http.StatusForbidden {
		t.Fatalf("built-in brand removal status=%d", builtin.Code)
	}
	if one := send("/api/ads/custom_tea_1/tea_15s"); one.Code != http.StatusOK || !strings.Contains(one.Body.String(), `"brand_removed":false`) {
		t.Fatalf("remove one creative: %d %s", one.Code, one.Body.String())
	}
	if _, err := os.Stat(filepath.Join(brandDir, "tea_15s.mp4")); !os.IsNotExist(err) {
		t.Fatal("the removed creative file still exists")
	}
	catalog, _ := app.loadCatalog()
	if brand, ok := findBrand(catalog, "custom_tea_1"); !ok || len(brand.Creatives) != 1 {
		t.Fatalf("catalogue after removing one creative: %+v", brand)
	}
	if last := send("/api/ads/custom_tea_1/tea_20s"); last.Code != http.StatusOK || !strings.Contains(last.Body.String(), `"brand_removed":true`) {
		t.Fatalf("removing the last creative should remove the brand: %d %s", last.Code, last.Body.String())
	}
	if _, err := os.Stat(brandDir); !os.IsNotExist(err) {
		t.Fatal("the empty brand folder still exists")
	}
	if gone := send("/api/ads/custom_tea_1"); gone.Code != http.StatusNotFound {
		t.Fatalf("removing a missing brand status=%d", gone.Code)
	}
}
