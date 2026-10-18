package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

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

func TestAxiRecoverPublicationWaitsBeyondDefaultRPCBudget(t *testing.T) {
	newAxiTimeoutFixture(t, axiTimeoutOpts{recoverPublication: func(ctx context.Context, raw json.RawMessage) (interface{}, error) {
		var params ipc.RecoverPublicationParams
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
		if err := sleepOrDone(ctx, 31*time.Second); err != nil {
			return nil, err
		}
		return &ipc.RecoverPublicationResult{RunID: params.RunID, PRURL: "https://github.com/test/repo/pull/99"}, nil
	}})
	cmd := newAxiRecoverPublicationCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--run", "terminal-run"})
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	if err := cmd.ExecuteContext(ctx); err != nil || !strings.Contains(out.String(), "https://github.com/test/repo/pull/99") {
		t.Fatalf("slow recovery response lost: %v output=%s", err, &out)
	}
}

func TestAxiRecoverPublicationWaitHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handled := make(chan struct{})
	newAxiTimeoutFixture(t, axiTimeoutOpts{recoverPublication: func(serverCtx context.Context, _ json.RawMessage) (interface{}, error) {
		close(handled)
		cancel()
		<-serverCtx.Done()
		return nil, serverCtx.Err()
	}})
	cmd := newAxiRecoverPublicationCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--run", "terminal-run"})
	started := time.Now()
	if err := cmd.ExecuteContext(ctx); err == nil || !strings.Contains(out.String(), "context canceled") {
		t.Fatalf("canceled wait: %v output=%s", err, &out)
	}
	select {
	case <-handled:
	default:
		t.Fatal("request never reached recovery handler")
	}
	if time.Since(started) > 5*time.Second {
		t.Fatal("cancellation did not interrupt recovery wait")
	}
}
