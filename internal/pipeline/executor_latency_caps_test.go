package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

	if err := exec.Respond(types.StepTest, types.ActionFix, nil); err == nil {
		t.Fatal("no-op auto-fix gate authorized another repair")
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
	cfg := &config.Config{AutoFix: config.AutoFix{Review: 1}, Review: config.Review{MaxRounds: 2}}

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

// cappedFixStep parks at review.max_rounds 1 with two blocking findings and a
// nit, then plays the fixer on the fix round: it records what it was handed
// and advances HEAD only when commit is true.
type cappedFixStep struct {
	commit        bool
	calls         int
	fixOnly       bool
	fixerFindings string
}

const cappedFixGateFindings = `{"findings":[{"id":"r1","severity":"error","description":"real bug","action":"auto-fix"},{"id":"r4","severity":"error","description":"second bug","action":"auto-fix"},{"id":"r2","severity":"warning","description":"nit","action":"auto-fix"}],"summary":"3 issues","risk_level":"high","risk_rationale":"r"}`

const cappedFixReviewedHead = "1111111111111111111111111111111111111111"

func (c *cappedFixStep) step() Step {
	return &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			c.calls++
			if !sctx.Fixing {
				return &StepOutcome{
					NeedsApproval:         true,
					AutoFixable:           true,
					Findings:              cappedFixGateFindings,
					ReviewApprovedHeadSHA: cappedFixReviewedHead,
				}, nil
			}
			c.fixOnly = sctx.FixWithoutRereview
			c.fixerFindings = sctx.PreviousFindings
			if c.commit {
				sctx.Run.HeadSHA = "2222222222222222222222222222222222222222"
				return &StepOutcome{FixSummary: "changes applied"}, nil
			}
			return &StepOutcome{FixSummary: "no changes applied"}, nil
		},
	}
}

func assertCappedFixCompleted(t *testing.T, database *db.DB, runID string, wantResolved, wantNotes []string) {
	t.Helper()
	sr := stepResultByName(t, database, runID, types.StepReview)
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
	ids := func(items []types.Finding) []string {
		var out []string
		for _, item := range items {
			out = append(out, item.ID)
		}
		return out
	}
	if len(findings.Items) != 0 || findings.RoundCap != 1 ||
		strings.Join(ids(findings.ResolvedByFix), ",") != strings.Join(wantResolved, ",") ||
		strings.Join(ids(findings.UnresolvedNotes), ",") != strings.Join(wantNotes, ",") {
		t.Fatalf("findings = %+v, want resolved %v and notes %v at round cap 1", findings, wantResolved, wantNotes)
	}
	run, err := database.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed", run.Status)
	}
	if run.ReviewApprovedHeadSHA == nil || *run.ReviewApprovedHeadSHA != cappedFixReviewedHead {
		t.Fatalf("review approval = %v, want the head the capped round reviewed", run.ReviewApprovedHeadSHA)
	}
}

func TestExecutor_ReviewRoundCapFixRunsOnceWithoutRereview(t *testing.T) {
	database, p, run, repo := setupTest(t)
	cfg := &config.Config{AutoFix: config.AutoFix{Review: 1}, Review: config.Review{MaxRounds: 1}}
	capped := &cappedFixStep{commit: true}

	exec := NewExecutor(database, p, cfg, nil, []Step{capped.step()}, nil)
	done, _ := startExecutor(t, exec, run, repo, t.TempDir())
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)

	sr := stepResultByName(t, database, run.ID, types.StepReview)
	gate, err := types.ParseFindingsJSON(*sr.FindingsJSON)
	if err != nil {
		t.Fatalf("parse findings: %v", err)
	}
	if len(gate.Items) != 2 || len(gate.UnresolvedNotes) != 1 || gate.RoundCap != 1 {
		t.Fatalf("gate findings = %+v, want the two errors with the warning as a note", gate)
	}

	if err := exec.Respond(types.StepReview, types.ActionFix, []string{"r1"}); err != nil {
		t.Fatalf("fix at a capped gate: %v", err)
	}
	waitExecutorDone(t, done)

	if capped.calls != 2 || !capped.fixOnly {
		t.Fatalf("calls = %d, fix without rereview = %v; want the initial review plus one fix-only round", capped.calls, capped.fixOnly)
	}
	if strings.Contains(capped.fixerFindings, "unresolved_notes") || !strings.Contains(capped.fixerFindings, `"r1"`) || strings.Contains(capped.fixerFindings, `"r4"`) {
		t.Fatalf("fixer was handed %s, want only the accepted r1", capped.fixerFindings)
	}
	assertCappedFixCompleted(t, database, run.ID, []string{"r1"}, []string{"r2", "r4"})

	rounds, err := database.GetRoundsByStep(sr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 2 || rounds[1].ReviewedHeadSHA != nil {
		t.Fatalf("rounds = %d, fix round reviewed head = %v; want a second round that reviewed nothing", len(rounds), rounds[len(rounds)-1].ReviewedHeadSHA)
	}
}

func TestExecutor_ReviewRoundCapFixThatChangesNothingLeavesNotes(t *testing.T) {
	database, p, run, repo := setupTest(t)
	cfg := &config.Config{AutoFix: config.AutoFix{Review: 1}, Review: config.Review{MaxRounds: 1}}
	capped := &cappedFixStep{commit: false}

	exec := NewExecutor(database, p, cfg, nil, []Step{capped.step()}, nil)
	done, _ := startExecutor(t, exec, run, repo, t.TempDir())
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if err := exec.Respond(types.StepReview, types.ActionFix, []string{"r1", "r4"}); err != nil {
		t.Fatalf("fix at a capped gate: %v", err)
	}
	waitExecutorDone(t, done)

	if capped.calls != 2 {
		t.Fatalf("calls = %d, want 2", capped.calls)
	}
	assertCappedFixCompleted(t, database, run.ID, nil, []string{"r2", "r1", "r4"})
}

func TestExecutor_ResumedCappedReviewGateRunsFixWithoutRereview(t *testing.T) {
	database, p, run, repo := setupTest(t)
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	stepResult, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStepWithoutAutoFixPolicy(stepResult.ID); err != nil {
		t.Fatal(err)
	}
	round := cappedFixGateFindings
	// The gate is parked exactly as applyReviewRoundCap leaves it: the step
	// carries the capped findings while the round keeps the full review.
	gate, _, _, ok := capReviewFindingsJSON(round, 1)
	if !ok {
		t.Fatal("cap the round findings")
	}
	if err := database.SetStepFindings(stepResult.ID, gate); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertReviewStepRound(stepResult.ID, 1, "initial", &round, nil, cappedFixReviewedHead, 10); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatusWithDuration(stepResult.ID, types.StepStatusAwaitingApproval, 10); err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}

	capped := &cappedFixStep{commit: true}
	exec := NewExecutor(database, p, &config.Config{Review: config.Review{MaxRounds: 1}}, nil, []Step{capped.step()}, nil)
	done := make(chan error, 1)
	go func() { done <- exec.Resume(context.Background(), run, repo, t.TempDir()) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := exec.Respond(types.StepReview, types.ActionFix, []string{"r1"}); err == nil {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("resume returned before the gate took a fix: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("recovered capped review never accepted fix")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if capped.calls != 1 || !capped.fixOnly {
		t.Fatalf("calls = %d, fix without rereview = %v; want one fix-only round", capped.calls, capped.fixOnly)
	}
	assertCappedFixCompleted(t, database, run.ID, []string{"r1"}, []string{"r2", "r4"})
}

func TestReviewRoundCapOf(t *testing.T) {
	if got := reviewRoundCapOf(`{"findings":[],"round_cap":3}`); got != 3 {
		t.Fatalf("reviewRoundCapOf = %d, want 3", got)
	}
	if got := reviewRoundCapOf(`{"findings":[]}`); got != 0 {
		t.Fatalf("reviewRoundCapOf(uncapped) = %d, want 0", got)
	}
}
