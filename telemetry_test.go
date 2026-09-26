package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestObservabilityEndpointIsReadOnlyAndOptional(t *testing.T) {
	dir := t.TempDir()
	app := newServer(slog.New(slog.NewTextHandler(io.Discard, nil)), dir)
	request := httptest.NewRequest(http.MethodGet, "/api/observability", nil)
	response := httptest.NewRecorder()
	app.routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"unavailable"`) {
		t.Fatalf("missing observer: %d %s", response.Code, response.Body.String())
	}
	path := filepath.Join(dir, "observability.json")
	if err := os.WriteFile(path, []byte(`{"events_received":3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	app.routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"events_received":3`) {
		t.Fatalf("observer response: %d %s", response.Code, response.Body.String())
	}
	if err := os.WriteFile(path, []byte(`{broken`), 0o600); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	app.routes().ServeHTTP(response, request)
	if !strings.Contains(response.Body.String(), `"unavailable"`) {
		t.Fatalf("malformed snapshot should be unavailable: %s", response.Body.String())
	}
}

func TestTelemetryQueueDoesNotRequireCollector(t *testing.T) {
	app := &server{telemetry: &telemetrySink{queue: make(chan string, 1)}}
	app.observeStage("job", "upload")
	app.observeFinish("job", "ok")
	if got := len(app.telemetry.queue); got != 1 {
		t.Fatalf("expected bounded queue, got %d", got)
	}
}
