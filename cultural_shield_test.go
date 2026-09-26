package main

import "testing"

func TestCulturalShieldBlocksAllBrandsAtSensitiveScene(t *testing.T) {
	brand := CatalogBrand{BrandID: "tea", DisplayName: "Tea", Category: "beverage", TargetContexts: []string{"cooking"}}
	for _, scene := range []SceneEvidence{
		{Summary: "পরিবারে শোকের মুহূর্ত", Confidence: .95, DialogueState: "completed_thought"},
		{Summary: "Family gathering", SensitiveContexts: []string{"mourning"}, Confidence: .95, DialogueState: "completed_thought"},
	} {
		match := evaluateBrandForScene(scene, brand)
		if !match.Blocked || match.Recommended {
			t.Fatalf("mourning scene should block even a brand without negative contexts: %+v", match)
		}
	}
	celebration := SceneEvidence{Summary: "A family celebration", SensitiveContexts: []string{"celebration"},
		Activities: []string{"cooking"}, Confidence: .95, DialogueState: "completed_thought"}
	match := evaluateBrandForScene(celebration, brand)
	if match.Blocked || !match.Recommended {
		t.Fatalf("ordinary celebration should not be universally blocked: %+v", match)
	}
}

func TestNextSafeBrandMomentUsesOnlyPotentialCutAndSafeScene(t *testing.T) {
	brand := CatalogBrand{BrandID: "tea", DisplayName: "Tea", Category: "beverage", TargetContexts: []string{"cooking"}}
	transcript := &Transcript{Scenes: []SceneEvidence{
		{SceneID: "mourning", Start: 0, End: 100, Summary: "A family mourns", SensitiveContexts: []string{"mourning"}, Confidence: .95, DialogueState: "completed_thought"},
		{SceneID: "cooking", Start: 100, End: 200, Summary: "The family cooks", Activities: []string{"cooking"}, Confidence: .95, DialogueState: "completed_thought"},
	}, BreakCandidates: []BreakCandidate{{Time: 80, Potential: true}, {Time: 120, Potential: false}, {Time: 150, Potential: true}}}
	next := nextSafeBrandMoment(transcript, 50, brand)
	if next == nil || *next != 150 {
		t.Fatalf("expected next safe potential moment at 150, got %v", next)
	}
}
