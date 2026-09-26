package main

import "testing"

func TestBalancedAdsAvoidRepeatBrandsAndPreferCategoryVariety(t *testing.T) {
	options := [][]brandChoice{
		{{brandID: "tea", creativeID: "tea-15", category: "food", fit: .95, recommended: true},
			{brandID: "bank", creativeID: "bank-15", category: "finance", fit: .8, recommended: true}},
		{{brandID: "tea", creativeID: "tea-15", category: "food", fit: .99, recommended: true},
			{brandID: "coffee", creativeID: "coffee-15", category: "food", fit: .85, recommended: true},
			{brandID: "travel", creativeID: "travel-15", category: "travel", fit: .8, recommended: true}},
		{{brandID: "tea", creativeID: "tea-15", category: "food", fit: .95, recommended: true},
			{brandID: "bank", creativeID: "bank-15", category: "finance", fit: .85, recommended: true},
			{brandID: "travel", creativeID: "travel-15", category: "travel", fit: .82, recommended: true}},
	}
	result := balanceAdSequence([]float64{300, 700, 1100}, options)
	if len(result) != 3 {
		t.Fatalf("expected 3 assignments, got %d", len(result))
	}
	for i, item := range result {
		if item.BrandID == "" || item.CreativeID == "" {
			t.Fatalf("a safe alternative existed at break %d: %+v", i, result)
		}
		for previous := max(0, i-2); previous < i; previous++ {
			if item.BrandID == result[previous].BrandID {
				t.Fatalf("brand repeated inside two-break window: %+v", result)
			}
		}
	}
}

func TestBalancedAdsLeaveUnsafeOrUnfillableBreaksEmpty(t *testing.T) {
	options := [][]brandChoice{{{brandID: "tea", creativeID: "tea-15", category: "food", fit: .9, recommended: true}},
		{{brandID: "tea", creativeID: "tea-15", category: "food", fit: .9, recommended: true}}, {}}
	result := balanceAdSequence([]float64{300, 700, 1100}, options)
	if result[0].BrandID == result[1].BrandID && result[0].BrandID != "" {
		t.Fatalf("repeated only available brand: %+v", result)
	}
	if result[2].BrandID != "" {
		t.Fatalf("assigned a brand with no safe option: %+v", result)
	}
}
