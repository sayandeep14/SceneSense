package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
)

// checkAgainstSchema walks the subset of JSON Schema used by web/schema/ad-manifest-v1.json:
// required keys, additionalProperties:false, nested properties, $ref, array items, and patterns.
func checkAgainstSchema(t *testing.T, path string, value any, schema map[string]any, defs map[string]any) {
	t.Helper()
	if ref, ok := schema["$ref"].(string); ok {
		schema = defs[strings.TrimPrefix(ref, "#/$defs/")].(map[string]any)
	}
	if pattern, ok := schema["pattern"].(string); ok {
		if text, _ := value.(string); !regexp.MustCompile(pattern).MatchString(text) {
			t.Errorf("%s = %q does not match %s", path, text, pattern)
		}
	}
	if enum, ok := schema["enum"].([]any); ok {
		found := false
		for _, option := range enum {
			found = found || option == value
		}
		if !found {
			t.Errorf("%s = %v is not one of %v", path, value, enum)
		}
	}
	switch typed := value.(type) {
	case map[string]any:
		properties, _ := schema["properties"].(map[string]any)
		required, _ := schema["required"].([]any)
		for _, key := range required {
			if _, ok := typed[key.(string)]; !ok {
				t.Errorf("%s is missing required %q", path, key)
			}
		}
		for key, child := range typed {
			childSchema, ok := properties[key].(map[string]any)
			if !ok {
				if schema["additionalProperties"] == false {
					t.Errorf("%s has %q, which the schema does not allow", path, key)
				}
				continue
			}
			checkAgainstSchema(t, path+"."+key, child, childSchema, defs)
		}
	case []any:
		if items, ok := schema["items"].(map[string]any); ok {
			for _, child := range typed {
				checkAgainstSchema(t, path+"[]", child, items, defs)
			}
		}
	}
}

func TestReviewOptimizeFinalizeAndManifestContract(t *testing.T) {
	app, _ := testServer(t)
	job, brands := playbackFixture(t, 1800)
	job.VideoURL, job.FileName, job.Media.FrameRate = "/media/fixture-job", "episode.mp4", 25
	candidate := &job.Transcript.BreakCandidates[0]
	candidate.SceneChange, candidate.Tier, candidate.AdScore, candidate.DialogueComplete = true, "High", 0.82, true
	candidate.Rationale, candidate.ShotTransition = "Setting change from kitchen to courtyard (fade) + 1.8-second silence.", "fade"
	candidate.Signals, candidate.AIReason, candidate.AIModel, candidate.AIPromptVer = []string{"fade"}, "x", "m", "p"
	app.jobs[job.ID] = job
	app.brandsPath = "assets/brands.json"
	handler := app.routes()
	send := func(method, path, body string) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Host = "demo.example"
		handler.ServeHTTP(response, request)
		return response
	}

	optimized := send(http.MethodPost, "/api/jobs/fixture-job/optimize", `{"k":1}`)
	var plan OptimizeResult
	if optimized.Code != http.StatusOK || json.Unmarshal(optimized.Body.Bytes(), &plan) != nil || plan.K != 1 || plan.MaxK != 4 {
		t.Fatalf("optimize status=%d body=%s", optimized.Code, optimized.Body.String())
	}

	selection := `{"time":60,"source":"ai","brand_id":"` + brands[0].BrandID + `","creative_id":"` + brands[0].Creatives[0].ID +
		`","allow_skip":true,"skip_after_sec":5,"click_through_url":"https://brand-a.example/app","cta_label":"Install app"}`
	if saved := send(http.MethodPut, "/api/jobs/fixture-job/review", `{"breaks":[`+selection+`],"target":1}`); saved.Code != http.StatusOK {
		t.Fatalf("save review status=%d body=%s", saved.Code, saved.Body.String())
	}
	if restored, _ := app.getJobSnapshot("fixture-job"); restored.Review == nil || len(restored.Review.Selected) != 1 {
		t.Fatalf("review was not stored: %+v", restored.Review)
	}

	finalized := send(http.MethodPost, "/api/jobs/fixture-job/finalize", `{"breaks":[`+selection+`]}`)
	if finalized.Code != http.StatusCreated {
		t.Fatalf("finalize status=%d body=%s", finalized.Code, finalized.Body.String())
	}
	manifestResponse := send(http.MethodGet, "/api/jobs/fixture-job/manifest.json", "")
	var manifest map[string]any
	if manifestResponse.Code != http.StatusOK || json.Unmarshal(manifestResponse.Body.Bytes(), &manifest) != nil {
		t.Fatalf("manifest status=%d body=%s", manifestResponse.Code, manifestResponse.Body.String())
	}
	schemaBytes, err := os.ReadFile("web/schema/ad-manifest-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(schemaBytes, &schema); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	checkAgainstSchema(t, "manifest", manifest, schema, schema["$defs"].(map[string]any))

	breakEntry := manifest["breaks"].([]any)[0].(map[string]any)
	ad := breakEntry["ads"].([]any)[0].(map[string]any)
	placement := breakEntry["placement"].(map[string]any)
	if breakEntry["timecode"] != "00:01:00:00" || breakEntry["frame"].(float64) != 1500 || ad["skip_offset_sec"].(float64) != 5 ||
		ad["click_through_url"] != "https://brand-a.example/app" || placement["confidence_tier"] != "High" ||
		!strings.HasPrefix(placement["rationale"].(string), "Setting change") || manifest["revision"].(float64) != 1 {
		t.Fatalf("manifest break = %+v", breakEntry)
	}

	if again := send(http.MethodPost, "/api/jobs/fixture-job/finalize", `{"breaks":[`+selection+`]}`); again.Code != http.StatusCreated {
		t.Fatalf("second finalize status=%d", again.Code)
	}
	if first := send(http.MethodGet, "/api/jobs/fixture-job/manifest.json?revision=1", ""); first.Code != http.StatusOK ||
		!strings.Contains(first.Body.String(), `"revision":1`) {
		t.Fatalf("revision 1 is not retrievable: %d", first.Code)
	}
	if missing := send(http.MethodGet, "/api/jobs/fixture-job/manifest.json?revision=9", ""); missing.Code != http.StatusNotFound {
		t.Fatalf("unknown revision status=%d", missing.Code)
	}
	if schemaFile := send(http.MethodGet, "/schema/ad-manifest-v1.json", ""); schemaFile.Code != http.StatusOK {
		t.Fatalf("schema is not served: %d", schemaFile.Code)
	}
	if empty := send(http.MethodPost, "/api/jobs/fixture-job/finalize", `{"breaks":[]}`); empty.Code != http.StatusUnprocessableEntity {
		t.Fatalf("finalizing with no breaks status=%d", empty.Code)
	}
}

func TestSMPTETimecodeUsesFrameRate(t *testing.T) {
	for _, tc := range []struct {
		seconds, rate float64
		want          string
		frame         int
	}{{560.72, 25, "00:09:20:18", 14018}, {3725.5, 30, "01:02:05:15", 111765}, {10, 0, "00:00:10:00", 250}} {
		if got, frame := smpteTimecode(tc.seconds, tc.rate); got != tc.want || frame != tc.frame {
			t.Errorf("smpteTimecode(%v, %v) = %s/%d, want %s/%d", tc.seconds, tc.rate, got, frame, tc.want, tc.frame)
		}
	}
	if parseFrameRate("30000/1001") != 29.97 || parseFrameRate("0/0") != 0 || parseFrameRate("25") != 25 {
		t.Fatal("frame rate parsing is wrong")
	}
}
