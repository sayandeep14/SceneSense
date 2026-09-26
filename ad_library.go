package main

import (
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	brandSourceBuiltin = "builtin"
	brandSourceCustom  = "custom"
	maxAdUploadBytes   = 200 << 20
	maxAdSeconds       = 120.0
	maxContextsPerList = 30
	maxContextLength   = 60
	aiFitThreshold     = 0.55
	contextFitMinimum  = 0.30
	sceneConfidenceMin = 0.65
	defaultCTALabel    = "Visit website"
	maxCTALabelLength  = 24
)

//go:embed ai/context_taxonomy.json
var contextTaxonomyJSON []byte

// contextTaxonomy maps canonical context groups to their aliases; the Python worker reads the same file.
var contextTaxonomy = func() map[string][]string {
	var groups map[string][]string
	if err := json.Unmarshal(contextTaxonomyJSON, &groups); err != nil || len(groups) == 0 {
		panic("ai/context_taxonomy.json is invalid")
	}
	return groups
}()

func (s *server) customCatalogPath() string {
	return filepath.Join(s.adLibraryDir, "catalog.json")
}

func readCustomCatalog(path string) ([]CatalogBrand, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("uploaded ad library is unavailable")
	}
	var brands []CatalogBrand
	if err := json.Unmarshal(data, &brands); err != nil {
		return nil, errors.New("uploaded ad library is invalid")
	}
	if err := validateCatalogEntries(brands); err != nil {
		return nil, err
	}
	return brands, nil
}

// loadCatalog merges the read-only built-in catalogue with ads uploaded through the UI.
func (s *server) loadCatalog() ([]CatalogBrand, error) {
	builtin, err := loadBrandCatalog(s.brandsPath)
	if err != nil {
		return nil, err
	}
	custom, err := readCustomCatalog(s.customCatalogPath())
	if err != nil {
		return nil, err
	}
	brands := make([]CatalogBrand, 0, len(builtin)+len(custom))
	seen := make(map[string]bool, len(builtin)+len(custom))
	for _, brand := range builtin {
		brand.Source = brandSourceBuiltin
		seen[brand.BrandID] = true
		brands = append(brands, brand)
	}
	for _, brand := range custom {
		if seen[brand.BrandID] {
			s.logger.Warn("skipping uploaded brand that collides with a built-in brand", "brand_id", brand.BrandID)
			continue
		}
		brand.Source = brandSourceCustom
		seen[brand.BrandID] = true
		brands = append(brands, brand)
	}
	return brands, nil
}

func (s *server) creativeFilePath(brand CatalogBrand, sourceURL string) string {
	root := filepath.Dir(s.brandsPath)
	if brand.Source == brandSourceCustom {
		root = s.adLibraryDir
	}
	return filepath.Join(root, filepath.FromSlash(sourceURL))
}

func parseContextList(raw string) ([]string, error) {
	fields := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ';' || r == '\n' || r == '\r' })
	values := make([]string, 0, len(fields))
	seen := make(map[string]bool, len(fields))
	for _, field := range fields {
		value := strings.ToLower(strings.Join(strings.Fields(field), " "))
		if value == "" || seen[value] {
			continue
		}
		if utf8.RuneCountInString(value) > maxContextLength {
			return nil, fmt.Errorf("each context must be at most %d characters", maxContextLength)
		}
		seen[value] = true
		values = append(values, value)
	}
	if len(values) > maxContextsPerList {
		return nil, fmt.Errorf("add at most %d contexts per list", maxContextsPerList)
	}
	return values, nil
}

// normalizeClickURL accepts an empty link or an absolute http(s) URL that is safe to place in VAST CDATA.
func normalizeClickURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || len(raw) > 500 || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" ||
		parsed.User != nil || strings.ContainsAny(raw, " \t\r\n") || strings.Contains(raw, "]]>") {
		return "", errors.New("the website link must be a full http:// or https:// address")
	}
	return parsed.String(), nil
}

func normalizeCTALabel(raw string) (string, error) {
	label := strings.Join(strings.Fields(raw), " ")
	if utf8.RuneCountInString(label) > maxCTALabelLength {
		return "", fmt.Errorf("the button label must be at most %d characters", maxCTALabelLength)
	}
	for _, r := range label {
		if unicode.IsControl(r) {
			return "", errors.New("the button label contains invalid characters")
		}
	}
	return label, nil
}

func brandSlug(name string) string {
	var b strings.Builder
	lastUnderscore := false
	for _, r := range strings.ToLower(name) {
		if r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			b.WriteRune(r)
			lastUnderscore = false
		} else if !lastUnderscore && b.Len() > 0 {
			b.WriteByte('_')
			lastUnderscore = true
		}
	}
	slug := strings.Trim(b.String(), "_")
	if len(slug) > 32 {
		slug = strings.Trim(slug[:32], "_")
	}
	return slug
}

func randomHex(n int) (string, error) {
	bytes := make([]byte, n)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

func (s *server) writeCustomCatalog(brands []CatalogBrand) error {
	for i := range brands {
		brands[i].Source = ""
	}
	data, err := json.MarshalIndent(brands, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(s.adLibraryDir, ".catalog-*.tmp")
	if err != nil {
		return err
	}
	tempPath := file.Name()
	defer os.Remove(tempPath)
	if _, err := file.Write(append(data, '\n')); err != nil {
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
	return os.Rename(tempPath, s.customCatalogPath())
}

func (s *server) uploadAd(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxAdUploadBytes+(1<<20))
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "Choose an MP4 ad smaller than 200 MB.")
		return
	}
	defer r.MultipartForm.RemoveAll()
	name := strings.Join(strings.Fields(r.FormValue("brand_name")), " ")
	if n := utf8.RuneCountInString(name); n < 2 || n > 60 {
		writeError(w, http.StatusBadRequest, "Brand name must be 2–60 characters.")
		return
	}
	category := strings.Join(strings.Fields(r.FormValue("category")), " ")
	if category == "" {
		category = "uncategorised"
	}
	if utf8.RuneCountInString(category) > 60 {
		writeError(w, http.StatusBadRequest, "Category must be at most 60 characters.")
		return
	}
	language := strings.ToLower(strings.TrimSpace(r.FormValue("language")))
	if language == "" {
		language = "bn"
	}
	if !languageCode.MatchString(language) {
		writeError(w, http.StatusBadRequest, "Language must be a 2–3 letter code such as bn.")
		return
	}
	targets, err := parseContextList(r.FormValue("target_contexts"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "Target contexts: "+err.Error()+".")
		return
	}
	if len(targets) == 0 {
		writeError(w, http.StatusBadRequest, "Add at least one target context so the ad can be matched to scenes.")
		return
	}
	negatives, err := parseContextList(r.FormValue("negative_contexts"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "Negative contexts: "+err.Error()+".")
		return
	}
	clickURL, err := normalizeClickURL(r.FormValue("click_through_url"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "Website link: "+err.Error()+".")
		return
	}
	ctaLabel, err := normalizeCTALabel(r.FormValue("cta_label"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "Button label: "+err.Error()+".")
		return
	}
	if clickURL == "" {
		ctaLabel = ""
	}
	file, header, err := r.FormFile("video")
	if err != nil {
		writeError(w, http.StatusBadRequest, "An ad video file is required.")
		return
	}
	defer file.Close()
	if !strings.EqualFold(filepath.Ext(header.Filename), ".mp4") {
		writeError(w, http.StatusUnsupportedMediaType, "Upload the ad as an MP4 video.")
		return
	}

	s.adLibraryMu.Lock()
	defer s.adLibraryMu.Unlock()
	all, err := s.loadCatalog()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	custom, err := readCustomCatalog(s.customCatalogPath())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	brandIndex := -1
	for _, brand := range all {
		if strings.EqualFold(brand.DisplayName, name) && brand.Source == brandSourceBuiltin {
			writeError(w, http.StatusConflict, "That name belongs to a built-in catalogue brand. Choose a different brand name.")
			return
		}
	}
	for i, brand := range custom {
		if strings.EqualFold(brand.DisplayName, name) {
			brandIndex = i
			break
		}
	}
	if brandIndex < 0 {
		suffix, err := randomHex(3)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Could not create the brand.")
			return
		}
		brandID := "custom_" + suffix
		if slug := brandSlug(name); slug != "" {
			brandID = "custom_" + slug + "_" + suffix
		}
		custom = append(custom, CatalogBrand{BrandID: brandID, DisplayName: name})
		brandIndex = len(custom) - 1
	}
	brand := &custom[brandIndex]
	brand.Category, brand.TargetContexts, brand.NegativeContexts = category, targets, negatives
	brand.ClickThroughURL, brand.CTALabel = clickURL, ctaLabel

	brandDir := filepath.Join(s.adLibraryDir, "ads", brand.BrandID)
	if err := os.MkdirAll(brandDir, 0o750); err != nil {
		s.logger.Error("create ad library directory", "error", err)
		writeError(w, http.StatusInternalServerError, "Ad storage is unavailable.")
		return
	}
	temp, err := os.CreateTemp(brandDir, ".incoming-*.mp4")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Could not start the ad upload.")
		return
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	written, err := io.Copy(temp, io.LimitReader(file, maxAdUploadBytes+1))
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "The ad upload could not be read completely.")
		return
	}
	if written > maxAdUploadBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "This ad is larger than the 200 MB limit.")
		return
	}
	media, err := probeVideo(r.Context(), tempPath)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if media.DurationSeconds < 0.5 || media.DurationSeconds > maxAdSeconds+0.5 {
		writeError(w, http.StatusUnprocessableEntity, "Ads must be between 1 and 120 seconds long.")
		return
	}
	seconds := int(math.Max(1, math.Round(media.DurationSeconds)))
	suffix, err := randomHex(3)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Could not name the uploaded ad.")
		return
	}
	creativeID := fmt.Sprintf("%s_%ds_%s_%s", strings.TrimPrefix(brand.BrandID, "custom_"), seconds, language, suffix)
	fileName := creativeID + ".mp4"
	if err := os.Rename(tempPath, filepath.Join(brandDir, fileName)); err != nil {
		writeError(w, http.StatusInternalServerError, "Could not store the uploaded ad.")
		return
	}
	brand.Creatives = append(brand.Creatives, CatalogCreative{
		ID: creativeID, DurationSec: seconds, Language: language, SourceURL: "ads/" + brand.BrandID + "/" + fileName,
	})
	saved := *brand
	if err := validateCatalogEntries(custom); err != nil {
		_ = os.Remove(filepath.Join(brandDir, fileName))
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err := s.writeCustomCatalog(custom); err != nil {
		s.logger.Error("persist ad library", "error", err)
		_ = os.Remove(filepath.Join(brandDir, fileName))
		writeError(w, http.StatusInternalServerError, "Could not save the ad library.")
		return
	}
	saved.Source = brandSourceCustom
	s.logger.Info("ad uploaded", "brand_id", saved.BrandID, "creative_id", creativeID, "duration_sec", seconds)
	writeJSON(w, http.StatusCreated, map[string]any{"brand": saved, "creative_id": creativeID})
}

func (s *server) deleteAdBrand(w http.ResponseWriter, r *http.Request) {
	s.removeUploadedAds(w, r.PathValue("brandID"), "")
}

func (s *server) deleteAdCreative(w http.ResponseWriter, r *http.Request) {
	s.removeUploadedAds(w, r.PathValue("brandID"), r.PathValue("creativeID"))
}

// removeUploadedAds deletes one uploaded creative, or a whole uploaded brand when creativeID is empty.
// A brand whose last creative is removed disappears too. Built-in brands cannot be removed.
func (s *server) removeUploadedAds(w http.ResponseWriter, brandID, creativeID string) {
	s.adLibraryMu.Lock()
	defer s.adLibraryMu.Unlock()
	builtin, err := loadBrandCatalog(s.brandsPath)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	if _, isBuiltin := findBrand(builtin, brandID); isBuiltin {
		writeError(w, http.StatusForbidden, "Built-in catalogue brands cannot be removed.")
		return
	}
	custom, err := readCustomCatalog(s.customCatalogPath())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	index := -1
	for i, brand := range custom {
		if brand.BrandID == brandID {
			index = i
		}
	}
	if index < 0 {
		writeError(w, http.StatusNotFound, "That uploaded brand does not exist.")
		return
	}
	brand := custom[index]
	brand.Source = brandSourceCustom
	var removed []CatalogCreative
	if creativeID == "" {
		removed = brand.Creatives
	} else {
		kept := []CatalogCreative{}
		for _, creative := range brand.Creatives {
			if creative.ID == creativeID {
				removed = append(removed, creative)
			} else {
				kept = append(kept, creative)
			}
		}
		if len(removed) == 0 {
			writeError(w, http.StatusNotFound, "That ad does not exist for this brand.")
			return
		}
		custom[index].Creatives = kept
	}
	brandRemoved := creativeID == "" || len(custom[index].Creatives) == 0
	if brandRemoved {
		custom = append(custom[:index], custom[index+1:]...)
	}
	if err := s.writeCustomCatalog(custom); err != nil {
		s.logger.Error("persist ad library after removal", "error", err)
		writeError(w, http.StatusInternalServerError, "Could not update the ad library.")
		return
	}
	for _, creative := range removed {
		if err := os.Remove(s.creativeFilePath(brand, creative.SourceURL)); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.logger.Warn("delete uploaded ad file", "creative_id", creative.ID, "error", err)
		}
	}
	if brandRemoved {
		_ = os.Remove(filepath.Join(s.adLibraryDir, "ads", brandID))
	}
	s.logger.Info("uploaded ads removed", "brand_id", brandID, "creatives", len(removed), "brand_removed", brandRemoved)
	writeJSON(w, http.StatusOK, map[string]any{"removed_creatives": len(removed), "brand_removed": brandRemoved})
}

// Scene/brand matching. AI fit scores are used when the scene model scored the brand; brands added
// after analysis get a deterministic context match. Negative contexts are always enforced here.

var (
	nonWord      = regexp.MustCompile(`[^\p{L}\p{N}]+`)
	languageCode = regexp.MustCompile(`^[a-z]{2,3}$`)
)

func normalizeContext(value string) string {
	return strings.TrimSpace(nonWord.ReplaceAllString(strings.ToLower(strings.ReplaceAll(value, "_", " ")), " "))
}

// containsPhrase reports whether phrase appears in text as whole words, allowing a plural suffix.
func containsPhrase(text, phrase string) bool {
	text, phrase = " "+normalizeContext(text)+" ", normalizeContext(phrase)
	if phrase == "" {
		return false
	}
	for _, variant := range []string{phrase, phrase + "s", phrase + "es"} {
		if strings.Contains(text, " "+variant+" ") {
			return true
		}
	}
	return strings.HasSuffix(phrase, "s") && strings.Contains(text, " "+strings.TrimSuffix(phrase, "s")+" ")
}

func canonicalContexts(values []string) map[string]bool {
	tags := make(map[string]bool)
	for canonical, aliases := range contextTaxonomy {
		for _, value := range values {
			matched := false
			for _, alias := range aliases {
				if containsPhrase(value, alias) {
					matched = true
					break
				}
			}
			if matched {
				tags[canonical] = true
				break
			}
		}
	}
	return tags
}

func sceneTerms(scene SceneEvidence) []string {
	terms := []string{scene.Summary}
	terms = append(terms, scene.Activities...)
	terms = append(terms, scene.Tone...)
	terms = append(terms, scene.Evidence...)
	return append(terms, scene.SensitiveContexts...)
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func negativeContextHits(scene SceneEvidence, negatives []string) []string {
	terms := sceneTerms(scene)
	sceneTags := canonicalContexts(terms)
	var hits []string
	for tag := range canonicalContexts(negatives) {
		if sceneTags[tag] {
			hits = appendUnique(hits, tag)
		}
	}
	joined := strings.Join(terms, " | ")
	for _, negative := range negatives {
		if containsPhrase(joined, negative) {
			hits = appendUnique(hits, normalizeContext(negative))
		}
	}
	sort.Strings(hits)
	return hits
}

func contextFit(scene SceneEvidence, targets []string) (float64, []string, string) {
	activities := strings.Join(scene.Activities, " | ")
	other := strings.Join(append(append([]string{scene.Summary}, scene.Tone...), scene.Evidence...), " | ")
	sceneTags := canonicalContexts(sceneTerms(scene))
	var weight float64
	var matched, notes []string
	for _, target := range targets {
		switch {
		case containsPhrase(activities, target):
			weight += 1
			notes = append(notes, target+" (activity)")
		case containsPhrase(other, target):
			weight += 0.6
			notes = append(notes, target+" (scene description)")
		default:
			related := false
			for tag := range canonicalContexts([]string{target}) {
				related = related || sceneTags[tag]
			}
			if !related {
				continue
			}
			weight += 0.5
			notes = append(notes, target+" (related context)")
		}
		matched = append(matched, target)
	}
	if len(matched) == 0 {
		return 0, nil, "None of this brand's target contexts appear in the scene evidence."
	}
	return math.Min(1, math.Round(weight*0.35*100)/100), matched, "Context match: " + strings.Join(notes, ", ") + "."
}

// evaluateBrandForScene returns the fit and hard-block verdict for one brand after a scene.
func evaluateBrandForScene(scene SceneEvidence, brand CatalogBrand) SceneBrandMatch {
	match := SceneBrandMatch{BrandID: brand.BrandID, DisplayName: brand.DisplayName, Category: brand.Category}
	threshold := contextFitMinimum
	if ai, ok := sceneBrandMatch(scene, brand.BrandID); ok {
		match.FitScore, match.MatchedContexts, match.Reason, match.FitSource = ai.FitScore, ai.MatchedContexts, ai.Reason, "ai"
		match.BlockedContexts = append([]string(nil), ai.BlockedContexts...)
		if ai.Blocked && len(match.BlockedContexts) == 0 {
			match.BlockedContexts = []string{"ai_blocked"}
		}
		threshold = aiFitThreshold
	} else {
		match.FitScore, match.MatchedContexts, match.Reason = contextFit(scene, brand.TargetContexts)
		match.FitSource = "context"
	}
	for _, hit := range negativeContextHits(scene, brand.NegativeContexts) {
		match.BlockedContexts = appendUnique(match.BlockedContexts, hit)
	}
	// A reviewer may place a manual marker anywhere, but cannot override these
	// culture-sensitive adjacency blocks by choosing a brand with empty negatives.
	for context := range canonicalContexts(sceneTerms(scene)) {
		if universalSensitiveContexts[context] {
			match.BlockedContexts = appendUnique(match.BlockedContexts, context)
		}
	}
	if scene.Confidence < sceneConfidenceMin || scene.DialogueState == "unclear" {
		match.BlockedContexts = appendUnique(match.BlockedContexts, "uncertain_scene")
	}
	match.Blocked = len(match.BlockedContexts) > 0
	if match.Blocked && len(match.BlockedContexts) > 0 {
		match.Reason = "Scene-context safety shield: an ad must wait until this story moment has passed."
	}
	match.Recommended = !match.Blocked && match.FitScore >= threshold
	return match
}

type suggestionCreative struct {
	CreativeID  string `json:"creative_id"`
	DurationSec int    `json:"duration_sec"`
	Language    string `json:"language"`
}

type adSuggestion struct {
	SceneBrandMatch
	Source         string               `json:"source"`
	TargetContexts []string             `json:"target_contexts"`
	Creatives      []suggestionCreative `json:"creatives"`
	NextSafeTime   *float64             `json:"next_safe_time,omitempty"`
}

var universalSensitiveContexts = map[string]bool{
	"grief": true, "medical": true, "violence": true, "injury": true,
	"religious_ritual": true, "children_at_risk": true, "financial_distress": true,
}

func nextSafeBrandMoment(transcript *Transcript, at float64, brand CatalogBrand) *float64 {
	for _, candidate := range transcript.BreakCandidates {
		if !candidate.Potential || candidate.Time <= at+0.5 {
			continue
		}
		if scene := sceneBeforeMarker(transcript, candidate.Time); scene != nil && evaluateBrandForScene(*scene, brand).Recommended {
			next := candidate.Time
			return &next
		}
	}
	return nil
}

func sceneAfterMarker(transcript *Transcript, at float64) *SceneEvidence {
	var next *SceneEvidence
	for i := range transcript.Scenes {
		scene := &transcript.Scenes[i]
		if scene.End > at+0.4 && (next == nil || scene.Start > next.Start) && scene.Start <= at+0.4 {
			next = scene
		}
	}
	if next != nil {
		return next
	}
	for i := range transcript.Scenes {
		scene := &transcript.Scenes[i]
		if scene.Start > at && (next == nil || scene.Start < next.Start) {
			next = scene
		}
	}
	return next
}

func (s *server) adSuggestions(w http.ResponseWriter, r *http.Request) {
	job, ok := s.getJobSnapshot(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "Analysis job not found.")
		return
	}
	if job.Transcript == nil || job.Status != "completed" {
		writeError(w, http.StatusConflict, "Ad suggestions need a completed analysis.")
		return
	}
	at, err := strconv.ParseFloat(r.URL.Query().Get("time"), 64)
	if err != nil || math.IsNaN(at) || math.IsInf(at, 0) || at < 0 || at > job.Media.DurationSeconds {
		writeError(w, http.StatusBadRequest, "A time within the programme is required.")
		return
	}
	brands, err := s.loadCatalog()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	before := sceneBeforeMarker(job.Transcript, at)
	suggestions := make([]adSuggestion, 0, len(brands))
	for _, brand := range brands {
		match := SceneBrandMatch{BrandID: brand.BrandID, DisplayName: brand.DisplayName, Category: brand.Category,
			Blocked: true, BlockedContexts: []string{"no_scene_evidence"}, Reason: "No scene evidence covers this moment."}
		if before != nil {
			match = evaluateBrandForScene(*before, brand)
		}
		creatives := make([]suggestionCreative, 0, len(brand.Creatives))
		for _, creative := range brand.Creatives {
			creatives = append(creatives, suggestionCreative{CreativeID: creative.ID, DurationSec: creative.DurationSec, Language: creative.Language})
		}
		sort.SliceStable(creatives, func(i, j int) bool { return creatives[i].DurationSec < creatives[j].DurationSec })
		var nextSafe *float64
		if match.Blocked {
			nextSafe = nextSafeBrandMoment(job.Transcript, at, brand)
		}
		suggestions = append(suggestions, adSuggestion{SceneBrandMatch: match, Source: brand.Source,
			TargetContexts: brand.TargetContexts, Creatives: creatives, NextSafeTime: nextSafe})
	}
	rank := func(item adSuggestion) int {
		switch {
		case item.Recommended:
			return 0
		case !item.Blocked:
			return 1
		}
		return 2
	}
	sort.SliceStable(suggestions, func(i, j int) bool {
		if rank(suggestions[i]) != rank(suggestions[j]) {
			return rank(suggestions[i]) < rank(suggestions[j])
		}
		return suggestions[i].FitScore > suggestions[j].FitScore
	})
	var candidate *BreakCandidate
	for i := range job.Transcript.BreakCandidates {
		item := &job.Transcript.BreakCandidates[i]
		if math.Abs(item.Time-at) < 0.5 && (candidate == nil || math.Abs(item.Time-at) < math.Abs(candidate.Time-at)) {
			candidate = item
		}
	}
	var transition *TransitionEvidence
	for i := range job.Transcript.Transitions {
		item := &job.Transcript.Transitions[i]
		if math.Abs(item.Time-at) <= 1.2 {
			transition = item
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"time": at, "scene_before": before, "scene_after": sceneAfterMarker(job.Transcript, at),
		"break_candidate": candidate, "transition": transition, "suggestions": suggestions,
	})
}
