package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPipelineDeckDefaultDrivesEveryDuty(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "deck")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' '{\"type\":\"run_finished\",\"output\":\"{\\\"ok\\\":true}\"}'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	global, err := config.LoadGlobalFromBytes([]byte("agent: deck\n"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Merge(global, &config.RepoConfig{})
	cfg.AgentPathOverride = map[string]string{"deck": bin}
	ag, err := newPipelineAgent(context.Background(), cfg, t.TempDir(), fakeLookPath, runenv.Overlay{})
	if err != nil {
		t.Fatal(err)
	}
	defer ag.Close()
	for _, purpose := range []string{"review", "review-fix", "test-evidence"} {
		var starts []string
		res, err := ag.Run(context.Background(), agent.RunOpts{Purpose: purpose, CWD: dir, Prompt: purpose, JSONSchema: json.RawMessage(`{"type":"object","required":["ok"]}`), OnLifecycle: func(e agent.LifecycleEvent) {
			if e.Phase == agent.LifecyclePhaseStart {
				starts = append(starts, e.Agent)
			}
		}})
		if err != nil || res == nil || string(res.Output) != `{"ok":true}` {
			t.Fatalf("%s result=%+v err=%v", purpose, res, err)
		}
		if len(starts) != 1 || starts[0] != "deck" {
			t.Fatalf("%s starts=%v", purpose, starts)
		}
	}
	cfg.DisableProjectSettings = true
	if _, err := newPipelineAgent(context.Background(), cfg, t.TempDir(), fakeLookPath, runenv.Overlay{}); err == nil {
		t.Fatal("Deck must refuse unverified settings suppression")
	}
	if cfg.Agent != types.AgentDeck {
		t.Fatal("agent changed")
	}
}
