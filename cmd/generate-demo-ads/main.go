// Command generate-demo-ads creates local-only synthetic slate MP4s for the
// creative URLs listed in assets/brands.json. The application never runs this.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type catalogueBrand struct {
	Creatives []catalogueCreative `json:"creatives"`
}

type catalogueCreative struct {
	Duration int    `json:"duration_sec"`
	URL      string `json:"url"`
}

func main() {
	brandsFile := flag.String("brands", "assets/brands.json", "creative catalogue JSON")
	timeLabel := flag.String("time", "00:00:25.515", "sample insertion timestamp shown on each slate")
	mood := flag.String("mood", "reflective, curious", "sample preceding-scene mood shown on each slate")
	force := flag.Bool("force", false, "replace existing local demo MP4s")
	flag.Parse()
	if err := generate(*brandsFile, *timeLabel, *mood, *force); err != nil {
		fmt.Fprintln(os.Stderr, "generate-demo-ads:", err)
		os.Exit(1)
	}
}

func generate(brandsFile, timeLabel, mood string, force bool) error {
	if strings.TrimSpace(timeLabel) == "" || len(timeLabel) > 80 || strings.TrimSpace(mood) == "" || len(mood) > 120 {
		return errors.New("time and mood labels must be non-empty and short enough for a demo slate")
	}
	data, err := os.ReadFile(brandsFile)
	if err != nil {
		return fmt.Errorf("read catalogue: %w", err)
	}
	var brands []catalogueBrand
	if err := json.Unmarshal(data, &brands); err != nil || len(brands) == 0 {
		return errors.New("catalogue JSON is invalid or empty")
	}
	root := filepath.Dir(brandsFile)
	type output struct {
		path     string
		duration int
	}
	var outputs []output
	for _, brand := range brands {
		for _, creative := range brand.Creatives {
			if creative.Duration != 15 && creative.Duration != 20 && creative.Duration != 30 {
				return fmt.Errorf("%q has unsupported duration %d; sample creatives must be 15, 20, or 30 seconds", creative.URL, creative.Duration)
			}
			if !strings.HasPrefix(creative.URL, "ads/") || filepath.IsAbs(creative.URL) || filepath.Clean(creative.URL) != creative.URL || strings.Contains(creative.URL, "..") {
				return fmt.Errorf("unsafe creative URL in catalogue: %q", creative.URL)
			}
			outputs = append(outputs, output{path: filepath.Join(root, filepath.FromSlash(creative.URL)), duration: creative.Duration})
		}
	}
	if len(outputs) == 0 {
		return errors.New("catalogue contains no creatives")
	}
	if !force {
		for _, item := range outputs {
			if _, err := os.Stat(item.path); err == nil {
				return fmt.Errorf("%s already exists; pass -force to regenerate the local demo files", item.path)
			} else if !os.IsNotExist(err) {
				return err
			}
		}
	}

	for _, item := range outputs {
		if err := os.MkdirAll(filepath.Dir(item.path), 0o755); err != nil {
			return fmt.Errorf("create %s: %w", filepath.Dir(item.path), err)
		}
		if err := renderSlate(item.path, filepath.Base(item.path), timeLabel, mood, item.duration); err != nil {
			return fmt.Errorf("render %s: %w", item.path, err)
		}
		fmt.Printf("created %s (%ds)\n", item.path, item.duration)
	}
	fmt.Printf("Generated %d local synthetic demo ads. The web server only serves these files.\n", len(outputs))
	return nil
}

func renderSlate(output, filename, timeLabel, mood string, duration int) error {
	tempDir, err := os.MkdirTemp("", "scenesense-demo-ad-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tempDir)
	svgPath := filepath.Join(tempDir, "slate.svg")
	pngPath := filepath.Join(tempDir, "slate.png")
	tempMP4, err := os.CreateTemp(filepath.Dir(output), ".demo-ad-*.mp4")
	if err != nil {
		return err
	}
	tempOutput := tempMP4.Name()
	if err := tempMP4.Close(); err != nil {
		return err
	}
	defer os.Remove(tempOutput)

	svg := fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="1280" height="720" viewBox="0 0 1280 720">
<rect width="1280" height="720" fill="#050505"/>
<text x="640" y="205" text-anchor="middle" fill="#e7c96e" font-family="Arial, sans-serif" font-size="36" letter-spacing="7">SYNTHETIC DEMO AD</text>
<text x="640" y="300" text-anchor="middle" fill="#ffffff" font-family="Arial, sans-serif" font-size="32">Creative file: %s</text>
<text x="640" y="370" text-anchor="middle" fill="#ffffff" font-family="Arial, sans-serif" font-size="32">Sample insertion time: %s</text>
<text x="640" y="440" text-anchor="middle" fill="#ffffff" font-family="Arial, sans-serif" font-size="32">Sample annotated mood: %s</text>
<text x="640" y="535" text-anchor="middle" fill="#8d8d8d" font-family="Arial, sans-serif" font-size="24">Placeholder creative · %d seconds</text>
</svg>`, html.EscapeString(filename), html.EscapeString(timeLabel), html.EscapeString(mood), duration)
	if err := os.WriteFile(svgPath, []byte(svg), 0o600); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	convert := exec.CommandContext(ctx, "rsvg-convert", "--output", pngPath, svgPath)
	if output, err := convert.CombinedOutput(); err != nil {
		return fmt.Errorf("rsvg-convert (install librsvg): %w: %s", err, strings.TrimSpace(string(output)))
	}
	encode := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-loop", "1", "-framerate", "15", "-i", pngPath,
		"-t", fmt.Sprint(duration), "-an", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-movflags", "+faststart", tempOutput)
	if output, err := encode.CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg (install FFmpeg): %w: %s", err, strings.TrimSpace(string(output)))
	}
	if err := os.Rename(tempOutput, output); err != nil {
		return err
	}
	return nil
}
