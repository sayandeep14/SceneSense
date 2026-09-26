package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestCancelAnalysisStopsWorkerAndPersistsRetryableState(t *testing.T) {
	app := newServer(slog.New(slog.NewTextHandler(io.Discard, nil)), t.TempDir())
	app.jobs["job-1"] = Job{ID: "job-1", Status: "processing", Stage: "detecting_shots"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	app.analysisCancels["job-1"] = cancel
	response := httptest.NewRecorder()
	app.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/jobs/job-1/cancel", nil))
	if response.Code != http.StatusOK || app.jobs["job-1"].Status != "cancelled" {
		t.Fatalf("cancel response: %d %s", response.Code, response.Body.String())
	}
	if ctx.Err() != context.Canceled {
		t.Fatal("worker context was not cancelled")
	}
	data, err := os.ReadFile(app.jobArtifactPath("job-1"))
	if err != nil {
		t.Fatal(err)
	}
	var persisted Job
	if err := json.Unmarshal(data, &persisted); err != nil || persisted.Status != "cancelled" {
		t.Fatalf("cancel state not persisted: %s (%v)", data, err)
	}
	response = httptest.NewRecorder()
	app.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/jobs/job-1/cancel", nil))
	if response.Code != http.StatusConflict {
		t.Fatalf("repeat cancel should conflict: %d", response.Code)
	}
}

func TestCancelQueuedAnalysisBeforeItStarts(t *testing.T) {
	app := newServer(slog.New(slog.NewTextHandler(io.Discard, nil)), t.TempDir())
	app.jobs["job-2"] = Job{ID: "job-2", Status: "queued"}
	response := httptest.NewRecorder()
	app.routes().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/jobs/job-2/cancel", nil))
	if response.Code != http.StatusOK || app.jobs["job-2"].Status != "cancelled" {
		t.Fatalf("queued cancellation failed: %d %s", response.Code, response.Body.String())
	}
}
