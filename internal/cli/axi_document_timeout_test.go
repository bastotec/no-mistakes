package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/spf13/cobra"
)

func TestAxiDocumentTimeoutRefusesOldDaemonWithoutFallback(t *testing.T) {
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{})
	ordinary := false
	fx.setGetActive(func(context.Context) (*ipc.RunInfo, error) { ordinary = true; return nil, nil })
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := runAxiDocumentTimeout(cmd, false, "original task", "", time.Minute, 90*time.Minute)
	if err == nil || !strings.Contains(out.String(), "start_document_run") || !strings.Contains(out.String(), "no ordinary fallback") {
		t.Fatalf("%v\n%s", err, &out)
	}
	if ordinary {
		t.Fatal("old daemon fell back to ordinary launch")
	}
	if got := cliGit(t, ".", "rev-parse", "HEAD"); got != fx.head {
		t.Fatal("caller head moved")
	}
}

func TestAxiDocumentTimeoutLaunchHonorsWaitAndCancellation(t *testing.T) {
	for _, cancelParent := range []bool{false, true} {
		t.Run(map[bool]string{false: "wait", true: "cancel"}[cancelParent], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			release := make(chan struct{})
			var entered atomic.Bool
			fx := newAxiTimeoutFixture(t, axiTimeoutOpts{documentRun: func(ctx context.Context, _ json.RawMessage) (interface{}, error) {
				entered.Store(true)
				if cancelParent {
					cancel()
				}
				select {
				case <-release:
				case <-ctx.Done():
				}
				return &ipc.StartFreshRunResult{}, nil
			}})
			defer close(release)
			cmd := &cobra.Command{}
			cmd.SetContext(ctx)
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			wait := 300 * time.Millisecond
			if cancelParent {
				wait = time.Minute
			}
			started := time.Now()
			err := runAxiDocumentTimeout(cmd, false, "original task", "", wait, 90*time.Minute)
			if elapsed := time.Since(started); elapsed > 2*time.Second {
				t.Fatalf("launch ignored bounded wait/cancellation for %s", elapsed)
			}
			if !entered.Load() || err == nil {
				t.Fatalf("launch not interrupted: entered=%v err=%v\n%s", entered.Load(), err, &out)
			}
			if cancelParent {
				if !strings.Contains(out.String(), "context canceled") || strings.Contains(out.String(), "wait of") {
					t.Fatalf("cancellation misreported:\n%s", &out)
				}
			} else {
				assertWaitElapsed(t, err, out.String(), wait.String())
				if strings.Contains(out.String(), "Both CLI and daemon") {
					t.Fatalf("wait misreported as capability failure:\n%s", &out)
				}
			}
			if got := cliGit(t, ".", "rev-parse", "HEAD"); got != fx.head {
				t.Fatal("caller head moved")
			}
		})
	}
}
