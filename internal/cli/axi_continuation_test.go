package cli

import (
	"bytes"
	"context"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/spf13/cobra"
	"strings"
	"testing"
	"time"
)

func TestAxiContinuationRejectsOldDaemonWithoutFallback(t *testing.T) {
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{})
	ordinary := false
	fx.setGetActive(func(context.Context) (*ipc.RunInfo, error) { ordinary = true; return nil, nil })
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := runAxiContinuation(cmd, false, "continue exact history", "https://github.com/upstream/widgets/pull/168", time.Minute)
	if err == nil || !strings.Contains(out.String(), "start_continuation_run") || ordinary {
		t.Fatalf("fallback=%v err=%v output=%s", ordinary, err, &out)
	}
	if got := cliGit(t, ".", "rev-parse", "HEAD"); got != fx.head {
		t.Fatal("unsupported launch moved source")
	}
}

func TestAxiContinuationDoesNotRelaxExistingFlagGuards(t *testing.T) {
	for _, args := range [][]string{
		{"--continue-pr", "https://github.com/upstream/widgets/pull/168", "--skip", "test"},
		{"--continue-pr", "https://github.com/upstream/widgets/pull/168", "--base-branch", "main"},
		{"--continue-pr", "https://github.com/upstream/widgets/pull/168", "--preserve-history", strings.Repeat("a", 40)},
		{"--continue-pr", "https://github.com/upstream/widgets/pull/168", "--existing-pr", "https://github.com/upstream/widgets/pull/168"},
		{"--continue-pr", "https://github.com/upstream/widgets/pull/168", "--launch-nonce", "nonce", "--validation-generation", "generation"},
		{"--continue-pr", "https://github.com/upstream/widgets/pull/168", "--retire-existing-pr"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			cmd := newAxiRunCmd()
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs(args)
			if err := cmd.Execute(); err == nil || !strings.Contains(out.String(), "cannot be combined") {
				t.Fatalf("err=%v out=%s", err, &out)
			}
		})
	}
}

func TestAxiContinuationStatusShowsExactPinsAndTestExclusion(t *testing.T) {
	raw := `{"version":1,"pr_head":"` + strings.Repeat("a", 40) + `","base_tip":"` + strings.Repeat("b", 40) + `","merge_base":"` + strings.Repeat("c", 40) + `","base_branch":"main","url":"https://github.com/upstream/widgets/pull/168"}`
	rv := runViewFromIPC(&ipc.RunInfo{ID: "run", ContinuationJSON: &raw})
	out := axiDoc(runObjectField(rv))
	for _, want := range []string{"continuation:", "original_pr_head_sha: " + strings.Repeat("a", 40), "base_tip_sha: " + strings.Repeat("b", 40), "merge_base_sha: " + strings.Repeat("c", 40), "local_test: excluded", "publication: fast-forward-only"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %s", want, out)
		}
	}
}
