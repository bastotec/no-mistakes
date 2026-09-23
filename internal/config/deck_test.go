package config

import (
	"context"
	"os/exec"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestDeckForkDefaultAndResolution(t *testing.T) {
	repo, err := LoadRepo("../..")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Merge(DefaultGlobalConfig(), EffectiveRepoConfig(repo, repo, false))
	if cfg.Agent != types.AgentDeck || len(cfg.Agents) != 1 || cfg.Agents[0] != types.AgentDeck {
		t.Fatalf("fork default=%v", cfg.Agents)
	}
	if cfg.AgentPath() != "deck" {
		t.Fatal(cfg.AgentPath())
	}
	err = cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
		if bin != "deck" {
			t.Fatalf("unexpected fallback %s", bin)
		}
		return "/tools/deck", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
		if bin != "deck" {
			t.Fatalf("unexpected fallback %s", bin)
		}
		return "", exec.ErrNotFound
	})
	if err == nil {
		t.Fatal("missing Deck must fail, not select another agent")
	}
}

func TestDeckProfileAndManagedArgs(t *testing.T) {
	if err := agentcfg.Validate(types.AgentDeck, agentcfg.Profile{Model: "provider/model"}); err != nil {
		t.Fatal(err)
	}
	if err := agentcfg.Validate(types.AgentDeck, agentcfg.Profile{Effort: agentcfg.EffortHigh}); err == nil {
		t.Fatal("unsupported effort accepted")
	}
	args := agentcfg.NativeArgs(types.AgentDeck, agentcfg.Profile{Model: "mapped"}, []string{"-m", "raw"})
	if len(args) != 0 {
		t.Fatalf("raw model overridden: %v", args)
	}
	for _, flag := range []string{"run", "--session", "--session=x", "-s", "--ephemeral"} {
		if err := validateAgentArgsOverride(map[string][]string{"deck": {flag}}); err == nil {
			t.Fatalf("managed flag %s accepted", flag)
		}
	}
	if err := validateAgentArgsOverride(map[string][]string{"deck": {"--model", "provider/model"}}); err != nil {
		t.Fatal(err)
	}
}
