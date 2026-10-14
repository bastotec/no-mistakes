package config

import (
	"slices"
	"strings"
	"testing"
)

func TestResolveReviewSizingDefaults(t *testing.T) {
	got := ResolveReviewSizing(ReviewSizingRaw{})
	if got.Effort != ReviewEffortAuto || got.SmallMaxLines != DefaultReviewSmallMaxLines || got.LargeMinFiles != DefaultReviewLargeMinFiles || !slices.Equal(got.RiskyPaths, DefaultReviewRiskyPaths) {
		t.Fatalf("defaults = %+v", got)
	}
	empty := []string{}
	if got := ResolveReviewSizing(ReviewSizingRaw{RiskyPaths: &empty}); len(got.RiskyPaths) != 0 {
		t.Fatalf("an empty risky_paths list must turn path risk off, got %v", got.RiskyPaths)
	}
}

func TestLoadRepo_ReviewSizing(t *testing.T) {
	cfg, err := LoadRepoFromBytes([]byte("review:\n  sizing:\n    effort: High\n    small_max_lines: 40\n    risky_paths: [\"internal/gate/**\", \"*auth*\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	sizing := ResolveReviewSizing(cfg.Review.Sizing)
	if sizing.Effort != ReviewEffortHigh || sizing.SmallMaxLines != 40 || sizing.SmallMaxFiles != DefaultReviewSmallMaxFiles || !slices.Equal(sizing.RiskyPaths, []string{"internal/gate/**", "*auth*"}) {
		t.Fatalf("sizing = %+v", sizing)
	}
	for _, bad := range []string{
		"review:\n  sizing:\n    effort: ultra\n",
		"review:\n  sizing:\n    large_min_lines: -1\n",
		"review:\n  sizing:\n    risky_paths: [\"[\"]\n",
	} {
		if _, err := LoadRepoFromBytes([]byte(bad)); err == nil || !strings.Contains(err.Error(), "review.sizing") {
			t.Fatalf("config %q: err = %v, want a review.sizing error", bad, err)
		}
	}
}

// review.sizing rides the trusted-only review block: a pushed branch cannot
// lighten its own review.
func TestEffectiveRepoConfig_ReviewSizingTrustedOnly(t *testing.T) {
	low := ReviewSizingRaw{Effort: ReviewEffortLow}
	high := ReviewSizingRaw{Effort: ReviewEffortHigh}
	pushed := &RepoConfig{Review: ReviewRaw{Sizing: low}}
	if got := EffectiveRepoConfig(pushed, &RepoConfig{Review: ReviewRaw{Sizing: high}}, true); got.Review.Sizing.Effort != ReviewEffortHigh {
		t.Fatalf("sizing effort = %q, want the trusted value", got.Review.Sizing.Effort)
	}
	if got := EffectiveRepoConfig(pushed, nil, false); got.Review.Sizing.Effort != "" {
		t.Fatalf("sizing effort = %q, want none without a trusted copy", got.Review.Sizing.Effort)
	}
}

func TestLoadGlobal_ReviewRoutes(t *testing.T) {
	cfg, err := LoadGlobalFromBytes([]byte("review:\n  routes:\n    finders: [chain/review, claude/sonnet-5-5]\n    light: zai/glm-5.3\n    verify: chain/review\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.Review.Routes.Finders, []string{"chain/review", "claude/sonnet-5-5"}) || cfg.Review.Routes.Light != "zai/glm-5.3" || cfg.Review.Routes.Verify != "chain/review" {
		t.Fatalf("routes = %+v", cfg.Review.Routes)
	}
	if _, err := LoadGlobalFromBytes([]byte("review:\n  routes:\n    finders: [\"\"]\n")); err == nil {
		t.Fatal("an empty finder route must fail the config")
	}
	merged := Merge(cfg, &RepoConfig{})
	if merged.Review.Routes.Light != "zai/glm-5.3" || merged.Review.Sizing.Effort != ReviewEffortAuto {
		t.Fatalf("merged review = %+v", merged.Review)
	}
}
