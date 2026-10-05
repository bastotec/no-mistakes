package config

import (
	"strings"
	"testing"
)

func TestReviewMaxRounds_GlobalDefaultAndTrustedRepoOverride(t *testing.T) {
	global, err := LoadGlobalFromBytes([]byte("review:\n  max_rounds: 2\n"))
	if err != nil {
		t.Fatalf("load global: %v", err)
	}
	if got := Merge(global, &RepoConfig{}).Review.MaxRounds; got != 2 {
		t.Fatalf("global max_rounds = %d, want 2", got)
	}

	trusted, err := LoadRepoFromBytes([]byte("review:\n  max_rounds: 4\n"))
	if err != nil {
		t.Fatalf("load repo: %v", err)
	}
	pushed, err := LoadRepoFromBytes([]byte("review:\n  max_rounds: 1\n"))
	if err != nil {
		t.Fatalf("load pushed repo: %v", err)
	}
	// The trusted default-branch value wins; a pushed branch cannot shorten
	// its own review, with or without allow_repo_commands.
	for _, allow := range []bool{false, true} {
		if got := Merge(global, EffectiveRepoConfig(pushed, trusted, allow)).Review.MaxRounds; got != 4 {
			t.Fatalf("allow_repo_commands=%v: max_rounds = %d, want the trusted 4", allow, got)
		}
	}
	if got := Merge(global, EffectiveRepoConfig(pushed, nil, false)).Review.MaxRounds; got != 2 {
		t.Fatalf("without a trusted copy max_rounds = %d, want the global 2", got)
	}

	// An explicit 0 in the trusted repo config restores the unlimited loop.
	unlimited, err := LoadRepoFromBytes([]byte("review:\n  max_rounds: 0\n"))
	if err != nil {
		t.Fatalf("load repo: %v", err)
	}
	if got := Merge(global, EffectiveRepoConfig(nil, unlimited, false)).Review.MaxRounds; got != 0 {
		t.Fatalf("trusted max_rounds: 0 resolved to %d, want 0", got)
	}

	if got := Merge(&GlobalConfig{}, &RepoConfig{}).Review.MaxRounds; got != 0 {
		t.Fatalf("absent max_rounds = %d, want 0 (unlimited)", got)
	}
}

func TestReviewMaxRounds_NegativeIsRejected(t *testing.T) {
	if _, err := LoadGlobalFromBytes([]byte("review:\n  max_rounds: -1\n")); err == nil || !strings.Contains(err.Error(), "review.max_rounds") {
		t.Fatalf("global negative max_rounds error = %v", err)
	}
	if _, err := LoadRepoFromBytes([]byte("review:\n  max_rounds: -1\n")); err == nil || !strings.Contains(err.Error(), "review.max_rounds") {
		t.Fatalf("repo negative max_rounds error = %v", err)
	}
}

func TestTestSkip_TrustedRepoOnly(t *testing.T) {
	trusted, err := LoadRepoFromBytes([]byte("test:\n  skip: true\n  skip_reason: \"  GitHub CI runs the full suite  \"\n"))
	if err != nil {
		t.Fatalf("load repo: %v", err)
	}
	got := Merge(&GlobalConfig{}, EffectiveRepoConfig(&RepoConfig{}, trusted, false)).Test
	if !got.Skip || got.SkipReason != "GitHub CI runs the full suite" {
		t.Fatalf("trusted skip = %v %q, want true with the trimmed reason", got.Skip, got.SkipReason)
	}

	pushed := &RepoConfig{Test: TestRaw{Skip: true, SkipReason: "skip my own tests"}}
	for _, allow := range []bool{false, true} {
		if got := Merge(&GlobalConfig{}, EffectiveRepoConfig(pushed, &RepoConfig{}, allow)).Test; got.Skip {
			t.Fatalf("allow_repo_commands=%v: a pushed test.skip was honored", allow)
		}
	}
	if got := Merge(&GlobalConfig{}, EffectiveRepoConfig(pushed, nil, false)).Test; got.Skip {
		t.Fatal("a pushed test.skip was honored without a trusted copy")
	}

	if got := Merge(&GlobalConfig{Test: TestRaw{Skip: true}}, &RepoConfig{}).Test; got.Skip {
		t.Fatal("a global test.skip leaked into the resolved config")
	}

	noReason := Merge(&GlobalConfig{}, &RepoConfig{Test: TestRaw{Skip: true}}).Test
	if !noReason.Skip || noReason.SkipReason == "" {
		t.Fatalf("skip without a reason = %v %q, want a default reason", noReason.Skip, noReason.SkipReason)
	}
}
