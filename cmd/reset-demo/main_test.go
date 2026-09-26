package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResetDryRunAndRecoverableArchive(t *testing.T) {
	uploads := filepath.Join(t.TempDir(), "uploads")
	if err := os.Mkdir(uploads, 0o700); err != nil {
		t.Fatal(err)
	}
	jobID := "12345678-1234-1234-1234-123456789abc"
	for _, name := range []string{jobID + ".mp4", jobID + ".job.json", "analysis-one.json", "notes.txt", ".env"} {
		if err := os.WriteFile(filepath.Join(uploads, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if archive, err := reset(uploads, false, false, &out); err != nil || archive != "" {
		t.Fatalf("dry run: %q, %v", archive, err)
	}
	if !strings.Contains(out.String(), "Dry run") {
		t.Fatalf("missing dry-run warning: %s", out.String())
	}
	archive, err := reset(uploads, true, false, &out)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{jobID + ".mp4", jobID + ".job.json", "analysis-one.json"} {
		if _, err := os.Stat(filepath.Join(archive, name)); err != nil {
			t.Fatalf("artifact not archived: %s: %v", name, err)
		}
	}
	for _, name := range []string{"notes.txt", ".env"} {
		if _, err := os.Stat(filepath.Join(uploads, name)); err != nil {
			t.Fatalf("unrelated file was moved: %s: %v", name, err)
		}
	}
}

func TestResetRejectsBroadAndSymlinkTargets(t *testing.T) {
	root := t.TempDir()
	var out bytes.Buffer
	if _, err := reset(root, true, false, &out); err == nil {
		t.Fatal("broad directory accepted")
	}
	if err := os.Symlink(root, filepath.Join(root, "uploads")); err != nil {
		t.Fatal(err)
	}
	if _, err := reset(filepath.Join(root, "uploads"), true, false, &out); err == nil {
		t.Fatal("symlink target accepted")
	}
}
