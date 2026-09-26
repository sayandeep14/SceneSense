package main

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
)

type balancedAd struct {
	Time       float64 `json:"time"`
	BrandID    string  `json:"brand_id,omitempty"`
	CreativeID string  `json:"creative_id,omitempty"`
	Category   string  `json:"category,omitempty"`
	FitScore   float64 `json:"fit_score,omitempty"`
	Reason     string  `json:"reason"`
}

type brandChoice struct {
	brandID     string
	creativeID  string
	category    string
	fit         float64
	recommended bool
}

// balanceAdSequence uses the already-judged scene context and hard brand blocks.
// The same brand cannot appear within two breaks; adjacent categories get a soft penalty.
func balanceAdSequence(times []float64, options [][]brandChoice) []balancedAd {
	type solution struct {
		score float64
		items []balancedAd
	}
	memo := make(map[string]solution)
	var solve func(int, string, string, string) solution
	solve = func(index int, lastBrand, olderBrand, lastCategory string) solution {
		if index == len(times) {
			return solution{}
		}
		key := strconv.Itoa(index) + "|" + lastBrand + "|" + olderBrand + "|" + lastCategory
		if found, ok := memo[key]; ok {
			return found
		}
		next := solve(index+1, "", lastBrand, "")
		best := solution{score: next.score - 2, items: append([]balancedAd{{Time: times[index], Reason: "No safe, diverse brand could be assigned here."}}, next.items...)}
		for _, option := range options[index] {
			if option.brandID == lastBrand || option.brandID == olderBrand {
				continue
			}
			next = solve(index+1, option.brandID, lastBrand, option.category)
			score := next.score + option.fit + 0.15
			if option.recommended {
				score += 0.1
			}
			if option.category == lastCategory {
				score -= 0.35
			}
			if score > best.score {
				reason := "Context-safe match; balanced against nearby brands."
				if option.category == lastCategory {
					reason = "Context-safe match; category repeats because it has the best overall sequence fit."
				}
				best = solution{score: score, items: append([]balancedAd{{Time: times[index], BrandID: option.brandID,
					CreativeID: option.creativeID, Category: option.category, FitScore: option.fit, Reason: reason}}, next.items...)}
			}
		}
		memo[key] = best
		return best
	}
	return solve(0, "", "", "").items
}

func (s *server) balanceAdPlan(w http.ResponseWriter, r *http.Request) {
	job, ok := s.getJobSnapshot(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "Analysis job not found.")
		return
	}
	if job.Status != "completed" || job.Transcript == nil {
		writeError(w, http.StatusConflict, "Brand balancing needs completed scene evidence.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2<<10)
	defer r.Body.Close()
	var request struct {
		Times []float64 `json:"times"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil && err != io.EOF {
		writeError(w, http.StatusBadRequest, "Invalid break times.")
		return
	}
	if len(request.Times) == 0 || len(request.Times) > 32 {
		writeError(w, http.StatusBadRequest, "Choose 1–32 break times.")
		return
	}
	for index, at := range request.Times {
		if math.IsNaN(at) || math.IsInf(at, 0) || at < minLeadSeconds || at > job.Transcript.Duration-minTailSeconds ||
			(index > 0 && at-request.Times[index-1] < minBreakGap) {
			writeError(w, http.StatusBadRequest, "Break times must be sorted, within the programme, and at least five minutes apart.")
			return
		}
	}
	brands, err := s.loadCatalog()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "Ad catalogue is unavailable.")
		return
	}
	options := make([][]brandChoice, len(request.Times))
	for index, at := range request.Times {
		scene := sceneBeforeMarker(job.Transcript, at)
		if scene == nil {
			continue
		}
		for _, brand := range brands {
			match := evaluateBrandForScene(*scene, brand)
			if match.Blocked || !match.Recommended || len(brand.Creatives) == 0 {
				continue
			}
			creatives := append([]CatalogCreative(nil), brand.Creatives...)
			sort.Slice(creatives, func(i, j int) bool { return creatives[i].DurationSec < creatives[j].DurationSec })
			options[index] = append(options[index], brandChoice{brandID: brand.BrandID, creativeID: creatives[0].ID,
				category: brand.Category, fit: match.FitScore, recommended: match.Recommended})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"assignments": balanceAdSequence(request.Times, options),
		"note": "Preview only. Hard scene-context blocks remain enforced; finalizing rechecks actual creative durations and placement policy."})
}
