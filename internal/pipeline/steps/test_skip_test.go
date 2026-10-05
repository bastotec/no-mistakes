package steps

import (
	"context"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
)

func TestTestStep_TrustedSkipRecordsSkippedWithReasonAndRunsNothing(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{
		name: "must-not-run",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			t.Fatal("the test agent ran although test.skip is set")
			return nil, nil
		},
	}
	// commands.test would fail the step if it ran.
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "exit 1"})
	sctx.Config.Test.Skip = true
	sctx.Config.Test.SkipReason = "GitHub CI runs the full suite"

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !outcome.Skipped {
		t.Fatal("outcome is not skipped")
	}
	if outcome.SkipReason != "test.skip: GitHub CI runs the full suite" {
		t.Fatalf("SkipReason = %q", outcome.SkipReason)
	}
	if outcome.NeedsApproval || outcome.Findings != "" {
		t.Fatalf("skipped outcome parks or carries findings: %+v", outcome)
	}
	if len(ag.calls) > 0 {
		t.Fatalf("agent called %d times", len(ag.calls))
	}
}
