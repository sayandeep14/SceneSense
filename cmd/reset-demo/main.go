package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var jobArtifact = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}(\.mp4|\.job\.json)$`)

func isDemoArtifact(name string, includeAds bool) bool {
	return jobArtifact.MatchString(name) || strings.HasPrefix(name, "analysis-") && strings.HasSuffix(name, ".json") ||
		strings.HasPrefix(name, "transcript-") && strings.HasSuffix(name, ".json") ||
		name == "observability.json" || name == "observability.json.tmp" || includeAds && name == "ads"
}

func reset(dir string, execute, includeAds bool, output io.Writer) (string, error) {
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if filepath.Base(absolute) != "uploads" {
		return "", errors.New("reset target must be a directory named uploads")
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("reset target must be a real uploads directory, not a symlink")
	}
	entries, err := os.ReadDir(absolute)
	if err != nil {
		return "", err
	}
	names := make([]string, 0)
	for _, entry := range entries {
		if !isDemoArtifact(entry.Name(), includeAds) || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		if entry.Name() == "ads" && !entry.IsDir() {
			continue
		}
		if entry.Name() != "ads" && !entry.Type().IsRegular() {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	if len(names) == 0 {
		fmt.Fprintln(output, "No SceneSense demo artifacts found.")
		return "", nil
	}
	for _, name := range names {
		fmt.Fprintln(output, name)
	}
	if !execute {
		fmt.Fprintf(output, "Dry run: %d artifact(s). Stop the app, then rerun with --execute to archive them.\n", len(names))
		return "", nil
	}
	archive := filepath.Join(absolute, ".reset-backup-"+time.Now().UTC().Format("20060102T150405.000000000Z"))
	if err := os.Mkdir(archive, 0o700); err != nil {
		return "", err
	}
	for _, name := range names {
		if err := os.Rename(filepath.Join(absolute, name), filepath.Join(archive, name)); err != nil {
			return archive, fmt.Errorf("archive %s: %w; earlier files remain recoverable in %s", name, err, archive)
		}
	}
	fmt.Fprintf(output, "Archived %d artifact(s) to %s. Restart the app for a fresh demo.\n", len(names), archive)
	return archive, nil
}

func main() {
	dir := flag.String("dir", "data/uploads", "SceneSense runtime uploads directory")
	execute := flag.Bool("execute", false, "archive the listed artifacts; default is a dry run")
	includeAds := flag.Bool("include-ads", false, "also archive uploaded ad catalogue and creatives")
	flag.Parse()
	if _, err := reset(*dir, *execute, *includeAds, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
