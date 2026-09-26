package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
)

const optimizerVersion = "even-spacing-dp-v1"

// OptimizeRequest asks for k breaks. A nil K lets the optimiser choose k; pinned times are reviewer
// placements that must be kept; excluded candidates were removed by the reviewer.
type OptimizeRequest struct {
	K        *int      `json:"k"`
	Pinned   []float64 `json:"pinned"`
	Excluded []string  `json:"excluded"`
}

type OptimizedBreak struct {
	CandidateID string  `json:"candidate_id,omitempty"`
	Time        float64 `json:"time"`
	Source      string  `json:"source"`
	Tier        string  `json:"tier,omitempty"`
	AdScore     float64 `json:"ad_score"`
	Note        string  `json:"note"`
}

type CandidateOutcome struct {
	CandidateID string `json:"candidate_id"`
	Status      string `json:"status"`
	Note        string `json:"note"`
}

type OptimizeResult struct {
	K            int                `json:"k"`
	AutoK        int                `json:"auto_k"`
	MaxK         int                `json:"max_k"`
	IdealSpacing float64            `json:"ideal_spacing_sec"`
	Selected     []OptimizedBreak   `json:"selected"`
	Outcomes     []CandidateOutcome `json:"outcomes"`
	Message      string             `json:"message"`
}

type optItem struct {
	candidate *BreakCandidate
	time      float64
	score     float64
	pinned    bool
}

// solveSpacing picks exactly k items (all pinned ones included) with at least minBreakGap between
// neighbours, maximising total score minus a penalty for gaps that stray from even spacing.
func solveSpacing(items []optItem, k int, duration float64) ([]int, bool) {
	n := len(items)
	if k == 0 {
		for _, item := range items {
			if item.pinned {
				return nil, false
			}
		}
		return []int{}, true
	}
	if n < k {
		return nil, false
	}
	ideal := duration / float64(k+1)
	gapCost := func(gap float64) float64 { return spacingPenalty * math.Abs(gap-ideal) / ideal }
	pinnedBetween := make([][]bool, n+1) // pinnedBetween[i][j]: a pinned item strictly between i and j
	pinnedBefore := make([]bool, n)
	pinnedAfter := make([]bool, n)
	seen := false
	for j := 0; j < n; j++ {
		pinnedBefore[j] = seen
		seen = seen || items[j].pinned
	}
	seen = false
	for j := n - 1; j >= 0; j-- {
		pinnedAfter[j] = seen
		seen = seen || items[j].pinned
	}
	for i := 0; i < n; i++ {
		pinnedBetween[i] = make([]bool, n)
		found := false
		for j := i + 1; j < n; j++ {
			pinnedBetween[i][j] = found
			found = found || items[j].pinned
		}
	}
	negInf := math.Inf(-1)
	best := make([][]float64, k+1)
	parent := make([][]int, k+1)
	for c := range best {
		best[c] = make([]float64, n)
		parent[c] = make([]int, n)
		for j := range best[c] {
			best[c][j], parent[c][j] = negInf, -1
		}
	}
	for j := 0; j < n; j++ {
		if !pinnedBefore[j] {
			best[1][j] = items[j].score - gapCost(items[j].time)
		}
	}
	for c := 2; c <= k; c++ {
		for j := 0; j < n; j++ {
			for i := 0; i < j; i++ {
				if best[c-1][i] == negInf || pinnedBetween[i][j] || items[j].time-items[i].time < minBreakGap {
					continue
				}
				if value := best[c-1][i] + items[j].score - gapCost(items[j].time-items[i].time); value > best[c][j] {
					best[c][j], parent[c][j] = value, i
				}
			}
		}
	}
	last, total := -1, negInf
	for j := 0; j < n; j++ {
		if best[k][j] == negInf || pinnedAfter[j] {
			continue
		}
		if value := best[k][j] - gapCost(duration-items[j].time); value > total {
			last, total = j, value
		}
	}
	if last < 0 {
		return nil, false
	}
	picked := make([]int, 0, k)
	for c, j := k, last; c >= 1; c-- {
		picked = append(picked, j)
		j = parent[c][j]
	}
	sort.Ints(picked)
	return picked, true
}

func optimizeBreaks(transcript *Transcript, request OptimizeRequest) (OptimizeResult, error) {
	duration := transcript.Duration
	maxK := maxBreakCount(duration)
	result := OptimizeResult{MaxK: maxK, Selected: []OptimizedBreak{}, Outcomes: []CandidateOutcome{}}
	pins := append([]float64(nil), request.Pinned...)
	sort.Float64s(pins)
	for i, at := range pins {
		if math.IsNaN(at) || math.IsInf(at, 0) || at < minLeadSeconds || at > duration-minTailSeconds {
			return result, errors.New("each reviewer placement must leave 15 seconds at the start and 10 at the end")
		}
		if i > 0 && at-pins[i-1] < minBreakGap {
			return result, fmt.Errorf("reviewer placements must be at least %.0f seconds apart", minBreakGap)
		}
	}
	if len(pins) > maxK {
		return result, fmt.Errorf("this programme allows at most %d ad breaks", maxK)
	}
	excluded := make(map[string]bool, len(request.Excluded))
	for _, id := range request.Excluded {
		excluded[id] = true
	}
	pool := func(allowLow bool) []optItem {
		items := make([]optItem, 0, len(pins)+len(transcript.BreakCandidates))
		for _, at := range pins {
			items = append(items, optItem{time: at, score: manualPlacementLoad, pinned: true})
		}
		for i := range transcript.BreakCandidates {
			candidate := &transcript.BreakCandidates[i]
			if !candidate.Potential || excluded[candidate.CandidateID] || (candidate.Tier == "Low" && !allowLow) {
				continue
			}
			tooClose := false
			for _, at := range pins {
				tooClose = tooClose || math.Abs(at-candidate.Time) < 0.5
			}
			if !tooClose {
				items = append(items, optItem{candidate: candidate, time: candidate.Time, score: candidate.AdScore})
			}
		}
		sort.Slice(items, func(a, b int) bool { return items[a].time < items[b].time })
		return items
	}
	strong, all := pool(false), pool(true)
	for k := maxK; k >= len(pins); k-- {
		if _, ok := solveSpacing(strong, k, duration); ok {
			result.AutoK = k
			break
		}
	}
	k := result.AutoK
	if request.K != nil {
		k = min(max(*request.K, len(pins)), maxK)
	}
	items, picked, ok := strong, []int(nil), false
	for ; k >= len(pins); k-- {
		if picked, ok = solveSpacing(strong, k, duration); ok {
			items = strong
			break
		}
		if picked, ok = solveSpacing(all, k, duration); ok {
			items = all
			break
		}
	}
	if !ok {
		k, items, picked = len(pins), all, nil
		picked, _ = solveSpacing(all, k, duration)
	}
	result.K = k
	if k > 0 {
		result.IdealSpacing = math.Round(duration / float64(k+1))
	}
	selected := make(map[*BreakCandidate]bool)
	previous := 0.0
	for _, index := range picked {
		item := items[index]
		entry := OptimizedBreak{Time: item.time, Source: "manual", AdScore: item.score, Note: "Placed by a reviewer."}
		gap := "first break"
		if previous > 0 {
			gap = clockLabel(item.time-previous) + " after the previous break"
		}
		if item.candidate != nil {
			selected[item.candidate] = true
			entry.CandidateID, entry.Source, entry.Tier = item.candidate.CandidateID, "ai", item.candidate.Tier
			entry.Note = fmt.Sprintf("Selected: %s ad-friendliness (%.2f); %s.", item.candidate.Tier, item.candidate.AdScore, gap)
		}
		result.Selected = append(result.Selected, entry)
		previous = item.time
	}
	for i := range transcript.BreakCandidates {
		candidate := &transcript.BreakCandidates[i]
		outcome := CandidateOutcome{CandidateID: candidate.CandidateID}
		switch {
		case selected[candidate]:
			outcome.Status = "selected"
			for _, entry := range result.Selected {
				if entry.CandidateID == candidate.CandidateID {
					outcome.Note = entry.Note
				}
			}
		case excluded[candidate.CandidateID]:
			outcome.Status, outcome.Note = "excluded", "Removed by a reviewer."
		case !candidate.Potential:
			outcome.Status = "blocked"
			messages := make([]string, 0, len(candidate.Reasons))
			for _, reason := range candidate.Reasons {
				messages = append(messages, reason.Message)
			}
			outcome.Note = joinSentences(messages)
		default:
			outcome.Status, outcome.Note = "available", unselectedNote(candidate, result.Selected, k)
		}
		result.Outcomes = append(result.Outcomes, outcome)
	}
	switch {
	case k == 0 && result.AutoK == 0:
		result.Message = "No moment is safe and ad-friendly enough for a break; the programme plays without ads."
	case request.K != nil && *request.K > k:
		result.Message = fmt.Sprintf("Only %d break(s) fit the %s minimum gap with the available moments.", k, clockLabel(minBreakGap))
	case k == 1:
		result.Message = fmt.Sprintf("1 break at %s; the system recommends %d of at most %d.",
			clockLabel(result.Selected[0].Time), result.AutoK, maxK)
	default:
		shortest, longest := math.Inf(1), 0.0
		for i := 1; i < len(result.Selected); i++ {
			gap := result.Selected[i].Time - result.Selected[i-1].Time
			shortest, longest = math.Min(shortest, gap), math.Max(longest, gap)
		}
		spacing := clockLabel(shortest) + "–" + clockLabel(longest)
		if clockLabel(shortest) == clockLabel(longest) {
			spacing = clockLabel(shortest)
		}
		result.Message = fmt.Sprintf("%d breaks, %s apart; the system recommends %d of at most %d.", k, spacing, result.AutoK, maxK)
	}
	return result, nil
}

func unselectedNote(candidate *BreakCandidate, selected []OptimizedBreak, k int) string {
	for _, entry := range selected {
		distance := math.Abs(entry.Time - candidate.Time)
		if distance >= minBreakGap {
			continue
		}
		if entry.Source == "manual" {
			return fmt.Sprintf("Within %s of the reviewer's break at %s.", clockLabel(distance), clockLabel(entry.Time))
		}
		if entry.AdScore > candidate.AdScore {
			return fmt.Sprintf("Within %s of the break at %s, which scores higher (%.2f vs %.2f).",
				clockLabel(distance), clockLabel(entry.Time), entry.AdScore, candidate.AdScore)
		}
		return fmt.Sprintf("Within %s of the break at %s, which keeps the breaks evenly spaced.", clockLabel(distance), clockLabel(entry.Time))
	}
	if candidate.Tier == "Low" {
		return fmt.Sprintf("Low ad-friendliness (%.2f); used only if you ask for more breaks.", candidate.AdScore)
	}
	return fmt.Sprintf("Not needed: %d break(s) already cover the programme evenly.", k)
}

func joinSentences(messages []string) string {
	out := ""
	for i, message := range messages {
		if i > 0 {
			out += " "
		}
		out += message
	}
	return out
}

func (s *server) optimizePlacements(w http.ResponseWriter, r *http.Request) {
	job, ok := s.getJobSnapshot(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "Analysis job not found.")
		return
	}
	if job.Transcript == nil || job.Status != "completed" {
		writeError(w, http.StatusConflict, "Break optimisation needs a completed analysis.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var request OptimizeRequest
	if err := decoder.Decode(&request); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "Optimisation request is invalid JSON.")
		return
	}
	if request.K != nil && (*request.K < 0 || *request.K > 64) {
		writeError(w, http.StatusBadRequest, "k must be between 0 and 64.")
		return
	}
	transcript := *job.Transcript
	transcript.BreakCandidates = append([]BreakCandidate(nil), job.Transcript.BreakCandidates...)
	result, err := optimizeBreaks(&transcript, request)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}
