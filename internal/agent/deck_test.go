package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestDeckEventsRequireSuccessfulTerminalAnswer(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","required":["ok"],"properties":{"ok":{"type":"boolean"}}}`)
	for _, tc := range []struct {
		name, stream      string
		wantErr, rejected bool
	}{
		{"success", `{"type":"run_started","model":"provider/model"}
{"type":"text_delta","text":"progress"}
{"type":"tool_finished","output":{"stdout":"ok","exit_code":0}}
{"type":"usage","input_tokens":2,"output_tokens":1}
{"type":"run_finished","output":"{\"ok\":true}","input_tokens":9,"output_tokens":3}`, false, false},
		{"null terminal", `{"type":"run_finished","output":null}`, true, false},
		{"missing output", `{"type":"run_finished"}`, true, false},
		{"duplicate terminal", `{"type":"run_finished","output":"{\"ok\":true}"}
{"type":"run_finished","output":"{\"ok\":true}"}`, true, false},
		{"failed with zero exit", `{"type":"run_failed","error":"quota unavailable"}`, true, false},
		{"progress is not verdict", `{"type":"text_delta","text":"{\"ok\":true}"}`, true, false},
		{"malformed stream", `{`, true, false},
		{"invalid verdict", `{"type":"run_finished","output":"{}"}`, true, true},
		{"empty verdict", `{"type":"run_finished","output":""}`, true, true},
		{"failure after finish", `{"type":"run_finished","output":"{\"ok\":true}"}
{"type":"run_failed","error":"hook failed"}`, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var chunks strings.Builder
			result, err := parseDeckEvents(strings.NewReader(tc.stream), RunOpts{JSONSchema: schema, OnChunk: func(s string) { chunks.WriteString(s) }})
			if (err != nil) != tc.wantErr || IsStructuredOutputRejected(err) != tc.rejected {
				t.Fatalf("result=%+v error=%v", result, err)
			}
			if !tc.wantErr && (string(result.Output) != `{"ok":true}` || result.Model != "provider/model" || !result.UsageReported || result.Usage.InputTokens != 9 || result.Usage.OutputTokens != 3 || chunks.String() != "progress") {
				t.Fatalf("result=%+v chunks=%q", result, chunks.String())
			}
		})
	}
}

func TestDeckRunUsesNativeLifecycleAndStdin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "deck")
	script := `#!/bin/sh
printf '%s\n' "$@" > args
cat > prompt
printf '%s\n' "$NO_MISTAKES_GATE/$DECK_TEST_ENV" > env
printf '%s\n' '{"type":"run_finished","output":"{\"ok\":true}"}'
`
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	a, err := New(types.AgentDeck, bin, []string{"--model", "provider/model"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if SupportsSessionResume(a) || NeutralizesGateInstructions(a) {
		t.Fatal("unverified capability")
	}
	var phases []string
	result, err := a.Run(context.Background(), RunOpts{CWD: dir, Prompt: "review this change", Env: []string{"DECK_TEST_ENV=present"}, JSONSchema: json.RawMessage(`{"type":"object","required":["ok"]}`), Session: &SessionRef{ID: "do-not-resume"}, OnLifecycle: func(e LifecycleEvent) {
		if e.Phase != LifecyclePhaseActivity {
			phases = append(phases, e.Phase)
		}
	}})
	if err != nil || result == nil {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	for path, want := range map[string]string{"args": "run\n--model\nprovider/model\n--ephemeral\n-\n", "env": "1/present\n"} {
		b, e := os.ReadFile(filepath.Join(dir, path))
		if e != nil || string(b) != want {
			t.Fatalf("%s=%q err=%v", path, b, e)
		}
	}
	prompt, _ := os.ReadFile(filepath.Join(dir, "prompt"))
	if !strings.Contains(string(prompt), "review this change") || !strings.Contains(string(prompt), `"required":["ok"]`) {
		t.Fatalf("prompt=%s", prompt)
	}
	if len(phases) != 2 {
		t.Fatalf("phases=%v", phases)
	}
}

func TestDeckRunCancellationAndExitFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	for _, body := range []string{"cat >/dev/null; sleep 30", "cat >/dev/null; echo refused >&2; exit 7"} {
		dir := t.TempDir()
		bin := filepath.Join(dir, "deck")
		if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
		a, _ := New(types.AgentDeck, bin, nil)
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		_, err := a.Run(ctx, RunOpts{CWD: dir, Prompt: "test"})
		cancel()
		if err == nil {
			t.Fatal("failed process accepted")
		}
		if strings.Contains(body, "refused") && !strings.Contains(err.Error(), "refused") {
			t.Fatalf("lost stderr: %v", err)
		}
	}
}

// Mirrors Deck's startup credential check without reading a real credential or
// contacting a gateway. Closing stdout before the diagnostic makes the old
// parse-error cleanup race deterministic.
func TestDeckRunStartupDiagnosticAndTerminalContract(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	t.Setenv("PROXAI_API_KEY", "")
	t.Setenv("PROXAI_API_KEY_FILE", "")
	for _, tc := range []struct {
		name, body, keyPath, want string
	}{
		{"no credential path", `cat >/dev/null
if [ -z "$PROXAI_API_KEY_FILE" ]; then
 exec 1>&-
 sleep 0.05
 echo 'resolve gateway client key: no gateway client key: set PROXAI_API_KEY_FILE' >&2
 exit 1
fi`, "", "exit status 1"},
		{"configured credential path", `cat >/dev/null
[ "$PROXAI_API_KEY_FILE" = fixture-only ] || exit 3
printf '%s\n' '{"type":"run_started","model":"provider/model"}' '{"type":"text_delta","text":"partial"}' '{"type":"run_finished","output":"{\"ok\":true}"}'`, "fixture-only", ""},
		{"missing terminal", `cat >/dev/null; echo diagnostic >&2`, "", "without run_finished"},
		{"malformed terminal", `cat >/dev/null; echo diagnostic >&2; echo '{"type":"run_finished","output":{}}'`, "", "terminal output"},
		{"malformed stream", `cat >/dev/null; echo diagnostic >&2; echo '{'`, "", "deck event"},
		{"failed event", `cat >/dev/null; echo diagnostic >&2; echo '{"type":"run_failed","error":"provider refused"}'`, "", "provider refused"},
		{"nonzero after terminal", `cat >/dev/null; echo diagnostic >&2; echo '{"type":"run_finished","output":"{\"ok\":true}"}'; exit 7`, "", "exit status 7"},
		{"stderr beyond excerpt", `cat >/dev/null; exec 1>&-; sleep 0.05; echo diagnostic >&2; head -c 131072 /dev/zero >&2; exit 9`, "", "exit status 9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "deck")
			if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+tc.body+"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			a, err := New(types.AgentDeck, bin, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, err := a.Run(ctx, RunOpts{CWD: dir, Prompt: "review", Env: []string{"PROXAI_API_KEY_FILE=" + tc.keyPath}, JSONSchema: json.RawMessage(`{"type":"object","required":["ok"],"properties":{"ok":{"type":"boolean"}}}`)})
			if tc.want == "" {
				if err != nil || result == nil || string(result.Output) != `{"ok":true}` {
					t.Fatalf("result=%+v err=%v", result, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
			diagnostic := "diagnostic"
			if tc.name == "no credential path" {
				diagnostic = "resolve gateway client key: no gateway client key: set PROXAI_API_KEY_FILE"
			}
			if !strings.Contains(err.Error(), diagnostic) {
				t.Fatalf("lost stderr: %v", err)
			}
			if ctx.Err() != nil {
				t.Fatalf("failed to drain stderr before deadline: %v", err)
			}
		})
	}
}

// fakeDeck writes a Deck stand-in whose `run --help` lists the given flags
// (counting each probe in ./probes) and whose runs record their argv in ./args.
func fakeDeck(t *testing.T, helpFlags string) (bin, dir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	dir = t.TempDir()
	bin = filepath.Join(dir, "deck")
	script := `#!/bin/sh
cd "$(dirname "$0")"
if [ "$1" = run ] && [ "$2" = --help ]; then
  echo probe >> probes
  printf 'Usage: deck run [OPTIONS] <PROMPT>\n      --ephemeral\n` + helpFlags + `'
  exit 0
fi
printf '%s\n' "$@" > args
cat >/dev/null
printf '%s\n' '{"type":"run_finished","output":"{\"ok\":true}"}'
`
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return bin, dir
}

func deckRunArgs(t *testing.T, a Agent, dir string) []string {
	t.Helper()
	if _, err := a.Run(context.Background(), RunOpts{CWD: t.TempDir(), Prompt: "review", JSONSchema: json.RawMessage(`{"type":"object","required":["ok"]}`)}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "args"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(raw))
}

func countProbes(t *testing.T, dir string) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "probes"))
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(raw), "probe")
}

func TestDeckOptOutPassesNoProjectContextAndNeutralizes(t *testing.T) {
	bin, dir := fakeDeck(t, `      --no-project-context\n`)
	a, err := NewWithOptions(types.AgentDeck, bin, []string{"--effort", "high"}, Options{DisableProjectSettings: true})
	if err != nil {
		t.Fatal(err)
	}
	if !NeutralizesGateInstructions(a) {
		t.Fatal("a Deck build listing --no-project-context must neutralize under the opt-out")
	}
	if err := EnsureGateNeutralized(a); err != nil {
		t.Fatalf("gate refused a neutralizing Deck: %v", err)
	}
	got := strings.Join(deckRunArgs(t, a, dir), " ")
	if got != "run --no-project-context --effort high --ephemeral -" {
		t.Fatalf("argv = %q", got)
	}
	// A second agent for the same unchanged binary reuses the probe.
	if _, err := NewWithOptions(types.AgentDeck, bin, nil, Options{DisableProjectSettings: true}); err != nil {
		t.Fatal(err)
	}
	if n := countProbes(t, dir); n != 1 {
		t.Fatalf("probes = %d, want 1 (cached per binary identity)", n)
	}
}

func TestDeckOptOutKeepsAnOperatorPinnedFlagOnce(t *testing.T) {
	bin, dir := fakeDeck(t, `      --no-project-context\n`)
	a, err := NewWithOptions(types.AgentDeck, bin, []string{"--no-project-context"}, Options{DisableProjectSettings: true})
	if err != nil {
		t.Fatal(err)
	}
	if !NeutralizesGateInstructions(a) {
		t.Fatal("an operator-pinned --no-project-context must stay neutralized")
	}
	if got := strings.Join(deckRunArgs(t, a, dir), " "); got != "run --no-project-context --ephemeral -" {
		t.Fatalf("argv = %q", got)
	}
}

func TestDeckWithoutTheFlagFailsTheGateClosed(t *testing.T) {
	bin, dir := fakeDeck(t, "")
	a, err := NewWithOptions(types.AgentDeck, bin, nil, Options{DisableProjectSettings: true})
	if err != nil {
		t.Fatal(err)
	}
	if NeutralizesGateInstructions(a) {
		t.Fatal("a Deck build without --no-project-context must not report neutralized")
	}
	if err := EnsureGateNeutralized(a); err == nil || !strings.Contains(err.Error(), "deck") || !strings.Contains(err.Error(), "--no-project-context") {
		t.Fatalf("gate error = %v", err)
	}
	if countProbes(t, dir) != 1 {
		t.Fatal("the opt-out must probe the binary")
	}
	missing, err := NewWithOptions(types.AgentDeck, filepath.Join(t.TempDir(), "deck"), nil, Options{DisableProjectSettings: true})
	if err != nil {
		t.Fatal(err)
	}
	if NeutralizesGateInstructions(missing) {
		t.Fatal("a missing Deck binary must not report neutralized")
	}
}

func TestDeckWithoutOptOutIsUnchanged(t *testing.T) {
	bin, dir := fakeDeck(t, `      --no-project-context\n`)
	a, err := NewWithOptions(types.AgentDeck, bin, []string{"--effort", "high"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if NeutralizesGateInstructions(a) {
		t.Fatal("without the opt-out Deck must not claim neutralization")
	}
	if got := strings.Join(deckRunArgs(t, a, dir), " "); got != "run --effort high --ephemeral -" {
		t.Fatalf("argv = %q", got)
	}
	if countProbes(t, dir) != 0 {
		t.Fatal("without the opt-out the binary must not be probed")
	}
}
