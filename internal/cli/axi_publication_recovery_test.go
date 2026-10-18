package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

func TestAxiRecoverPublicationRequiresExplicitRun(t *testing.T) {
	cmd := newAxiRecoverPublicationCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.Execute(); err == nil || !strings.Contains(out.String(), "--run is required") {
		t.Fatalf("error=%v output=%s", err, &out)
	}
}

func TestAxiRecoverPublicationOldDaemonHasNoFallback(t *testing.T) {
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{})
	lookedUpActive := false
	fx.setGetActive(func(context.Context) (*ipc.RunInfo, error) { lookedUpActive = true; return nil, nil })
	cmd := newAxiRecoverPublicationCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--run", "terminal-run"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(out.String(), "recover_publication") {
		t.Fatalf("error=%v output=%s", err, &out)
	}
	if lookedUpActive {
		t.Fatal("recovery fell back to ordinary run discovery")
	}
	if got := cliGit(t, ".", "rev-parse", "HEAD"); got != fx.head {
		t.Fatal("recovery changed caller head")
	}
}
