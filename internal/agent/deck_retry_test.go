package agent

import (
	"context"
	"encoding/json"
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

// Diagnostics can quote the gateway signature without being gateway failures.
// Neither rejected model output nor invalid configuration may spend retries.
func TestDeckDoesNotRetryGatewayTextInNonGatewayDiagnostics(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	defer withFastBackoff(t)()
	for _, tc := range []struct {
		name   string
		script string
		schema []byte
	}{
		{
			name: "rejected model output",
			script: `printf '%s\n' '{"type":"run_finished","output":"{\"value\":\"model gateway request failed\"}"}'
`,
			schema: []byte(`{"type":"object","required":["value"],"properties":{"value":{"type":"string","enum":["ok"]}}}`),
		},
		{
			name: "invalid configuration",
			script: `printf '%s\n' "error: invalid value 'model gateway request failed' for '--effort <LEVEL>'" >&2
exit 2
`,
		},
		{
			name: "gateway signature only on stderr",
			script: `printf '%s\n' 'model gateway request failed (HTTP 503)' >&2
exit 2
`,
		},
		{
			name: "gateway signature in hook failure",
			script: `printf '%s\n' '{"type":"run_failed","error":"hook failed: model gateway request failed (HTTP 503)"}'
exit 1
`,
		},
		{
			name: "refused request quoting transient failure",
			script: `printf '%s\n' '{"type":"run_failed","error":"model gateway request failed (HTTP 400): model gateway request failed (HTTP 503)"}'
exit 1
`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "deck")
			if err := os.WriteFile(bin, []byte("#!/bin/sh\ncat >/dev/null\n"+tc.script), 0o700); err != nil {
				t.Fatal(err)
			}
			a, err := New(types.AgentDeck, bin, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			attempts := 0
			_, err = a.Run(context.Background(), RunOpts{
				CWD:        dir,
				Prompt:     "review",
				JSONSchema: tc.schema,
				OnAttempt:  func(Attempt) { attempts++ },
			})
			if err == nil {
				t.Fatal("a non-gateway failure must remain an error")
			}
			if tc.schema != nil && !IsStructuredOutputRejected(err) {
				t.Fatalf("expected a structured-output rejection, got %v", err)
			}
			if attempts != 1 {
				t.Fatalf("deck made %d attempts for a non-gateway failure, want 1: %v", attempts, err)
			}
		})
	}
}

func TestClassifyDeckTransient(t *testing.T) {
	for _, tc := range []struct {
		message string
		want    bool
	}{
		{"model gateway request failed", true},
		{"model gateway request failed: codex_stream_failed", true},
		{"model gateway request failed (HTTP 503)", true},
		{"model gateway request failed (HTTP 429)", true},
		{"model gateway request failed (HTTP 400)", false},
		{"model gateway request failed (HTTP 401)", false},
		{"tool budget exhausted", false},
		{"insufficient_quota", false},
		{"ended its turn with prose instead of the required JSON object", false},
		{"connection reset by peer", false},
	} {
		wire, err := json.Marshal(map[string]string{"type": "run_failed", "error": tc.message})
		if err != nil {
			t.Fatal(err)
		}
		_, err = parseDeckEvents(strings.NewReader(string(wire)), RunOpts{})
		// Process stderr stays diagnostic even when it names a transient failure.
		err = errors.Join(errors.New("model gateway request failed (HTTP 502)"), err)
		if _, got := classifyDeckTransient(err); got != tc.want {
			t.Errorf("classifyDeckTransient(%q) = %v, want %v", tc.message, got, tc.want)
		}
	}
	for _, err := range []error{context.Canceled, errors.New("deck run failed: model gateway request failed")} {
		if _, got := classifyDeckTransient(err); got {
			t.Errorf("non-native failure must never be retried: %v", err)
		}
	}
}

// Deck's own deadline is a typed error, never retried: a rerun would only
// spend the same budget again.
func TestDeckDeadlineIsTypedAndNotRetried(t *testing.T) {
	defer withFastBackoff(t)()
	a, count := fakeDeckFailingFirst(t, 9, "run exceeded 1500s deadline")
	_, err := a.Run(context.Background(), RunOpts{CWD: t.TempDir(), Prompt: "review"})
	if !errors.Is(err, ErrRunDeadline) {
		t.Fatalf("Run() error = %v, want ErrRunDeadline", err)
	}
	if got := runCount(t, count); got != "1" {
		t.Fatalf("deck ran %s times, want 1", got)
	}
	_, err = func() (*Result, error) {
		b, _ := fakeDeckFailingFirst(t, 9, "model gateway request failed (HTTP 400)")
		return b.Run(context.Background(), RunOpts{CWD: t.TempDir(), Prompt: "review"})
	}()
	if errors.Is(err, ErrRunDeadline) {
		t.Fatalf("a gateway refusal read as a deadline: %v", err)
	}
}
