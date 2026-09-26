package main

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const observerAddress = "127.0.0.1:9917"

// telemetrySink never blocks a request or analysis worker. A missing Rust observer
// drops measurements; it cannot affect upload, inference, or playback.
type telemetrySink struct {
	queue chan string
}

func newTelemetrySink() *telemetrySink {
	sink := &telemetrySink{queue: make(chan string, 128)}
	go func() {
		for packet := range sink.queue {
			connection, err := net.DialTimeout("udp", observerAddress, 5*time.Millisecond)
			if err != nil {
				continue
			}
			_ = connection.SetWriteDeadline(time.Now().Add(5 * time.Millisecond))
			_, _ = connection.Write([]byte(packet))
			_ = connection.Close()
		}
	}()
	return sink
}

func (s *server) observe(packet string) {
	if s.telemetry == nil || len(packet) > 256 {
		return
	}
	select {
	case s.telemetry.queue <- packet:
	default:
	}
}

func (s *server) observeStage(id, stage string) {
	s.observe("S|" + id + "|" + stage)
}

func (s *server) observeFinish(id, status string) {
	s.observe("F|" + id + "|" + status)
}

func (s *server) observeDuration(id, name, status string, elapsed time.Duration) {
	s.observe("M|" + id + "|" + name + "|" + status + "|" + strconv.FormatInt(max(0, elapsed.Milliseconds()), 10))
}

func (s *server) observeEvent(id, name string) {
	s.observe("E|" + id + "|" + name)
}

func (s *server) getObservability(w http.ResponseWriter, _ *http.Request) {
	path := filepath.Join(s.uploadDir, "observability.json")
	data, err := os.ReadFile(path)
	if err != nil || len(data) > 128<<10 || !json.Valid(data) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "unavailable"})
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(data)
}
