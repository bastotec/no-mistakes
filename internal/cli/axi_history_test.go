package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/spf13/cobra"
)

func TestAxiPreserveHistory_RejectsUnsupportedDaemonWithoutFallback(t *testing.T) {
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{}) // models the old RPC surface
	ordinaryLookup := false
	fx.setGetActive(func(context.Context) (*ipc.RunInfo, error) {
		ordinaryLookup = true
		return nil, nil
	})
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := runAxiPreserveHistory(cmd, false, nil, "preserve history", "main", fx.head, "", "", time.Minute)
	if err == nil || !strings.Contains(out.String(), "start_history_run") || !strings.Contains(out.String(), "no unconstrained fallback") {
		t.Fatalf("unsupported daemon did not fail closed: %v\n%s", err, &out)
	}
	if ordinaryLookup {
		t.Fatal("unsupported constrained launch fell back to ordinary launch")
	}
	if got := cliGit(t, ".", "rev-parse", "HEAD"); got != fx.head {
		t.Fatal("unsupported launch changed the caller head")
	}
}

func TestAxiPreserveHistory_RejectsInvalidControlsBeforeOpeningEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name, base, sha, nonce, generation, want string
		skip                                     []types.StepName
	}{
		{name: "empty", base: "main", want: "full nonzero"},
		{name: "moving-ref", base: "main", sha: "origin/main", want: "full nonzero"},
		{name: "missing-branch", sha: strings.Repeat("a", 40), want: "--base-branch"},
		{name: "skip", base: "main", sha: strings.Repeat("a", 40), skip: []types.StepName{types.StepRebase}, want: "--skip"},
		{name: "incomplete-proof", base: "main", sha: strings.Repeat("a", 40), nonce: "n", want: "supplied together"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &cobra.Command{}
			var out bytes.Buffer
			cmd.SetOut(&out)
			err := runAxiPreserveHistory(cmd, false, tc.skip, "intent", tc.base, tc.sha, tc.nonce, tc.generation, time.Minute)
			if err == nil || !strings.Contains(out.String(), tc.want) {
				t.Fatalf("want %q: %v\n%s", tc.want, err, &out)
			}
		})
	}
}
