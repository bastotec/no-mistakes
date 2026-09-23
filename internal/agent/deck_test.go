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
