package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// fakeDeckFailingFirst writes a Deck that fails its first `failures` runs with
// failure, then answers. It counts runs in the returned file.
func fakeDeckFailingFirst(t *testing.T, failures int, failure string) (Agent, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	dir := t.TempDir()
	count := filepath.Join(dir, "count")
	script := `#!/bin/sh
cat >/dev/null
n=$(cat "` + count + `" 2>/dev/null || echo 0)
n=$((n+1))
echo $n > "` + count + `"
if [ $n -le ` + string(rune('0'+failures)) + ` ]; then
  printf '%s\n' '{"type":"run_failed","error":"` + failure + `"}'
  exit 1
fi
printf '%s\n' '{"type":"run_finished","output":"done"}'
`
	bin := filepath.Join(dir, "deck")
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	a, err := New(types.AgentDeck, bin, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a, count
}

func runCount(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// A dropped gateway stream is rerun in place instead of failing the step.
func TestDeckRetriesATransientGatewayDrop(t *testing.T) {
	defer withFastBackoff(t)()
	a, count := fakeDeckFailingFirst(t, 1, "model gateway request failed")
	result, err := a.Run(context.Background(), RunOpts{CWD: t.TempDir(), Prompt: "review"})
	if err != nil || result == nil || result.Text != "done" {
		t.Fatalf("Run() = %+v, %v; want the retried answer", result, err)
	}
	if got := runCount(t, count); got != "2" {
		t.Fatalf("deck ran %s times, want 2", got)
	}
	if !ReportsAgentAttempts(a) {
		t.Fatal("deck reports its own attempts, so wrappers must not record the run again")
	}
}

// The retry budget is bounded: a gateway that keeps dropping still fails.
func TestDeckGivesUpAfterTwoRetries(t *testing.T) {
	defer withFastBackoff(t)()
	a, count := fakeDeckFailingFirst(t, 9, "model gateway request failed (HTTP 502)")
	if _, err := a.Run(context.Background(), RunOpts{CWD: t.TempDir(), Prompt: "review"}); err == nil {
		t.Fatal("a gateway that never recovers must fail the run")
	}
	if got := runCount(t, count); got != "3" {
		t.Fatalf("deck ran %s times, want 3 (one try plus two retries)", got)
	}
}

// A request the gateway refused is not retried: rerunning it cannot succeed.
func TestDeckDoesNotRetryARefusedGatewayRequest(t *testing.T) {
	defer withFastBackoff(t)()
	a, count := fakeDeckFailingFirst(t, 1, "model gateway request failed (HTTP 400)")
	if _, err := a.Run(context.Background(), RunOpts{CWD: t.TempDir(), Prompt: "review"}); err == nil {
		t.Fatal("an HTTP 400 must fail the run")
	}
	if got := runCount(t, count); got != "1" {
		t.Fatalf("deck ran %s times, want 1", got)
	}
}

func TestClassifyDeckTransient(t *testing.T) {
	for msg, want := range map[string]bool{
		"deck run failed: model gateway request failed":                              true,
		"deck run failed: model gateway request failed (HTTP 503)":                   true,
		"deck run failed: model gateway request failed (HTTP 429)":                   true,
		"deck run failed: model gateway request failed (HTTP 400)":                   false,
		"deck run failed: model gateway request failed (HTTP 401)":                   false,
		"deck run failed: tool budget exhausted":                                     false,
		"deck run failed: insufficient_quota":                                        false,
		"deck output: ended its turn with prose instead of the required JSON object": false,
		"deck run failed: connection reset by peer":                                  false,
	} {
		if _, got := classifyDeckTransient(errors.New(msg)); got != want {
			t.Errorf("classifyDeckTransient(%q) = %v, want %v", msg, got, want)
		}
	}
	if _, got := classifyDeckTransient(context.Canceled); got {
		t.Error("cancellation must never be retried")
	}
}
