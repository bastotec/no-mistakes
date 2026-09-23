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
{"type":"usage","input_tokens":2,"output_tokens":1}
{"type":"run_finished","output":"{\"ok\":true}","input_tokens":9,"output_tokens":3}`, false, false},
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
