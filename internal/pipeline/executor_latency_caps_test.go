package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func stepResultByName(t *testing.T, database *db.DB, runID string, name types.StepName) *db.StepResult {
	t.Helper()
	steps, err := database.GetStepsByRun(runID)
	if err != nil {
		t.Fatalf("get steps: %v", err)
	}
	for _, sr := range steps {
		if sr.StepName == name {
			return sr
		}
	}
	t.Fatalf("step %s not found", name)
	return nil
}

func TestExecutor_NoOpAutoFixStopsSpendingAttempts(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()
	cfg := &config.Config{AutoFix: config.AutoFix{Test: 3}}

	calls := 0
	step := &adaptiveCallStep{
		name: types.StepTest,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			calls++
			// The fix round leaves HEAD where it was: nothing was committed.
			return &StepOutcome{
				NeedsApproval: true,
				AutoFixable:   true,
				Findings:      `{"findings":[{"severity":"error","description":"TestFoo fails","action":"auto-fix"}],"summary":"1 failure"}`,
			}, nil
		},
	}

	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
	done, _ := startExecutor(t, exec, run, repo, workDir)
	waitForStepStatus(t, database, run.ID, types.StepTest, types.StepStatusFixReview)

	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (initial + one no-op auto-fix, then the gate)", calls)
	}
	logData, err := os.ReadFile(filepath.Join(p.RunLogDir(run.ID), string(types.StepTest)+".log"))
	if err != nil {
		t.Fatalf("read step log: %v", err)
	}
	if !strings.Contains(string(logData), "auto-fix made no changes") {
		t.Fatalf("step log does not say the auto-fix made no changes:\n%s", logData)
	}

	if err := exec.Respond(types.StepTest, types.ActionApprove, nil); err != nil {
		t.Fatalf("approve: %v", err)
	}
	waitExecutorDone(t, done)
}

func TestExecutor_AutoFixThatCommitsKeepsItsBudget(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()
	cfg := &config.Config{AutoFix: config.AutoFix{Lint: 3}}

	calls := 0
	step := &adaptiveCallStep{
		name: types.StepLint,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			calls++
			if sctx.Fixing {
				sctx.Run.HeadSHA = "fixed-" + string(rune('0'+calls))
			}
			if calls < 3 {
				return &StepOutcome{
					NeedsApproval: true,
					AutoFixable:   true,
					Findings:      `{"findings":[{"severity":"error","description":"lint","action":"auto-fix"}],"summary":"1 issue"}`,
				}, nil
			}
			return &StepOutcome{}, nil
		},
	}

	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
	if err := exec.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3 (two committing auto-fixes)", calls)
	}
}

func TestExecutor_ReviewRoundCapCarriesNonBlockingFindingsAsNotes(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()
	cfg := &config.Config{Review: config.Review{MaxRounds: 2}}

	calls := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			calls++
			if calls == 1 {
				return &StepOutcome{
					NeedsApproval: true,
					AutoFixable:   true,
					Findings:      `{"findings":[{"id":"r1","severity":"error","description":"real bug","action":"auto-fix"}],"summary":"1 issue","risk_level":"medium","risk_rationale":"r"}`,
				}, nil
			}
			sctx.Run.HeadSHA = "after-fix"
			// The rereview found two fresh nits: neither blocks a merge.
			return &StepOutcome{
				NeedsApproval: true,
				AutoFixable:   true,
				Findings:      `{"findings":[{"id":"r2","severity":"warning","description":"nit","action":"ask-user"},{"id":"r3","severity":"info","description":"style","action":"auto-fix"}],"summary":"2 issues","risk_level":"low","risk_rationale":"r"}`,
			}, nil
		},
	}

	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
	done, _ := startExecutor(t, exec, run, repo, workDir)
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if err := exec.Respond(types.StepReview, types.ActionFix, []string{"r1"}); err != nil {
		t.Fatalf("fix below the cap: %v", err)
	}
	waitExecutorDone(t, done)

	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (no third round after the cap)", calls)
	}
	sr := stepResultByName(t, database, run.ID, types.StepReview)
	if sr.Status != types.StepStatusCompleted {
		t.Fatalf("review status = %s, want completed", sr.Status)
	}
	if sr.FindingsJSON == nil {
		t.Fatal("review findings missing")
	}
	findings, err := types.ParseFindingsJSON(*sr.FindingsJSON)
	if err != nil {
		t.Fatalf("parse findings: %v", err)
	}
	if len(findings.Items) != 0 || len(findings.UnresolvedNotes) != 2 || findings.RoundCap != 2 {
		t.Fatalf("findings = %+v, want 0 items, 2 unresolved notes, round cap 2", findings)
	}
	updated, _ := database.GetRun(run.ID)
	if updated.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed", updated.Status)
	}
}

func TestExecutor_ReviewRoundCapStillGatesBlockingFindingsAndRefusesFix(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()
	cfg := &config.Config{Review: config.Review{MaxRounds: 1}}

	calls := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			calls++
			return &StepOutcome{
				NeedsApproval: true,
				AutoFixable:   true,
				Findings:      `{"findings":[{"id":"r1","severity":"error","description":"real bug","action":"auto-fix"},{"id":"r2","severity":"warning","description":"nit","action":"auto-fix"}],"summary":"2 issues","risk_level":"high","risk_rationale":"r"}`,
			}, nil
		},
	}

	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
	done, _ := startExecutor(t, exec, run, repo, workDir)
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)

	sr := stepResultByName(t, database, run.ID, types.StepReview)
	findings, err := types.ParseFindingsJSON(*sr.FindingsJSON)
	if err != nil {
		t.Fatalf("parse findings: %v", err)
	}
	if len(findings.Items) != 1 || findings.Items[0].ID != "r1" || len(findings.UnresolvedNotes) != 1 || findings.Items[0].Severity != "error" {
		t.Fatalf("gate findings = %+v, want only the blocking error with the warning as a note", findings)
	}

	err = exec.Respond(types.StepReview, types.ActionFix, []string{"r1"})
	if err == nil || !strings.Contains(err.Error(), "review.max_rounds") {
		t.Fatalf("fix at a capped gate = %v, want a review.max_rounds refusal", err)
	}
	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatalf("approve at a capped gate: %v", err)
	}
	waitExecutorDone(t, done)
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestReviewRoundCapOf(t *testing.T) {
	if got := reviewRoundCapOf(`{"findings":[],"round_cap":3}`); got != 3 {
		t.Fatalf("reviewRoundCapOf = %d, want 3", got)
	}
	if got := reviewRoundCapOf(`{"findings":[]}`); got != 0 {
		t.Fatalf("reviewRoundCapOf(uncapped) = %d, want 0", got)
	}
}
