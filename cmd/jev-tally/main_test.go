package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The measurement harness must refuse a Gemini model exactly like the
// pipeline step, and must resolve the gateway key by reference only, so a
// missing key is a fail-safe stop before any quota is spent.
func TestRunRefusesGeminiModel(t *testing.T) {
	err := run("kunchenguid/no-mistakes", 1, "", filepath.Join(t.TempDir(), "state.json"),
		"AI_GATEWAY_API_KEY", "/nonexistent/secrets", "google/gemini-2.5-pro", time.Second)
	if err == nil || !strings.Contains(err.Error(), "Gemini") {
		t.Fatalf("run() with a Gemini model = %v, want a refusal naming Gemini", err)
	}
}

func TestRunStopsWithoutKeyBeforeSpendingQuota(t *testing.T) {
	err := run("kunchenguid/no-mistakes", 1, "", filepath.Join(t.TempDir(), "state.json"),
		"NM_JEV_TALLY_TEST_KEY", "/nonexistent/secrets", "typesafe-ai/jev", time.Second)
	if err == nil || !strings.Contains(err.Error(), "gateway key not found") {
		t.Fatalf("run() without a key = %v, want a fail-safe stop", err)
	}
	if strings.Contains(err.Error(), "sk-") {
		t.Fatalf("error must never embed a key value: %v", err)
	}
}
