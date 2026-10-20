package pipeline

import (
	"context"
	"errors"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A step that daemon shutdown cut off returns the bare context error; the
// executor reads the cause and leaves the step running for recovery instead
// of failing it.
func TestExecutor_ShutdownCancelledStepIsNotFailed(t *testing.T) {
	database, p, run, repo := setupTest(t)
	step := &adaptiveCallStep{name: types.StepCI, fn: func(sctx *StepContext) (*StepOutcome, error) {
		<-sctx.Ctx.Done()
		return nil, sctx.Ctx.Err()
	}}
	exec := NewExecutor(database, p, &config.Config{}, nil, []Step{step}, nil)
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan error, 1)
	go func() { done <- exec.Execute(ctx, run, repo, t.TempDir()) }()
	waitForStepStatus(t, database, run.ID, types.StepCI, types.StepStatusRunning)
	cancel(ErrDaemonShutdown)
	if err := <-done; !errors.Is(err, ErrDaemonShutdown) {
		t.Fatalf("Execute() = %v, want ErrDaemonShutdown", err)
	}
	if sr := stepResultByName(t, database, run.ID, types.StepCI); sr.Status != types.StepStatusRunning {
		t.Fatalf("ci step = %s, want it left running for recovery", sr.Status)
	}
}

// Only a run whose CI step is watching an open PR, with every earlier step
// finished and nothing parked or repairing, counts as a CI watch.
func TestRecoverableCIWatch(t *testing.T) {
	database, _, run, _ := setupTest(t)
	steps := []Step{&adaptiveCallStep{name: types.StepReview}, &adaptiveCallStep{name: types.StepCI}}
	review, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	ci, err := database.InsertStepResult(run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	reload := func() {
		t.Helper()
		var err error
		if run, err = database.GetRun(run.ID); err != nil {
			t.Fatal(err)
		}
	}
	reload()
	if _, err := RecoverableCIWatch(database, run, steps); err == nil {
		t.Fatal("a run without a PR counted as a CI watch")
	}
	if err := database.UpdateRunPRURL(run.ID, "https://github.com/test/repo/pull/1"); err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(ci.ID); err != nil {
		t.Fatal(err)
	}
	reload()
	if _, err := RecoverableCIWatch(database, run, steps); err == nil {
		t.Fatal("a CI step running before review finished counted as a CI watch")
	}
	if err := database.CompleteStep(review.ID, 0, 1, ""); err != nil {
		t.Fatal(err)
	}
	if index, err := RecoverableCIWatch(database, run, steps); err != nil || index != 1 {
		t.Fatalf("RecoverableCIWatch = %d, %v; want the CI step", index, err)
	}
	if err := database.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}
	reload()
	if _, err := RecoverableCIWatch(database, run, steps); err == nil {
		t.Fatal("a run parked at a gate counted as a CI watch")
	}
}

// settlingCIStep is a CI step whose PR is already merged or closed: its gate
// reconciliation settles the watch, so Execute must never run.
type settlingCIStep struct{ executed bool }

func (s *settlingCIStep) Name() types.StepName { return types.StepCI }
func (s *settlingCIStep) Execute(*StepContext) (*StepOutcome, error) {
	s.executed = true
	return &StepOutcome{}, nil
}
func (s *settlingCIStep) ReconcileApprovalGate(*StepContext) (bool, error) { return true, nil }

func setupCIWatch(t *testing.T) (*db.DB, *paths.Paths, *db.Run, *db.Repo) {
	t.Helper()
	database, p, run, repo := setupTest(t)
	review, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteStep(review.ID, 0, 1, ""); err != nil {
		t.Fatal(err)
	}
	ci, err := database.InsertStepResult(run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(ci.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunPRURL(run.ID, "https://github.com/test/repo/pull/1"); err != nil {
		t.Fatal(err)
	}
	if run, err = database.GetRun(run.ID); err != nil {
		t.Fatal(err)
	}
	return database, p, run, repo
}

// A resumed watch that cannot watch (provider gone, credentials expired)
// skips; that must end the run as ci_monitor_interrupted, not complete it
// and reclaim the worktree.
func TestResumeCIWatch_SkippedWatchEndsInterrupted(t *testing.T) {
	database, p, run, repo := setupCIWatch(t)
	ci := &adaptiveCallStep{name: types.StepCI, fn: func(*StepContext) (*StepOutcome, error) {
		return &StepOutcome{Skipped: true, SkipReason: "gh not authenticated"}, nil
	}}
	exec := NewExecutor(database, p, &config.Config{}, nil, []Step{&adaptiveCallStep{name: types.StepReview}, ci}, nil)
	if err := exec.ResumeCIWatch(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunCIMonitorInterrupted {
		t.Fatalf("run = %s, want %s", got.Status, types.RunCIMonitorInterrupted)
	}
	sr := stepResultByName(t, database, run.ID, types.StepCI)
	if sr.Status != types.StepStatusSkipped || sr.Error == nil || *sr.Error != types.RunCIMonitorInterruptedReason {
		t.Fatalf("ci step = %s (%v), want skipped with the interrupted reason", sr.Status, sr.Error)
	}
}

// A PR that merged or closed while the daemon was down settles the watch
// before it starts, so an --existing-pr watch never re-validates a PR that
// is already done.
func TestResumeCIWatch_SettledPRCompletesWithoutWatching(t *testing.T) {
	database, p, run, repo := setupCIWatch(t)
	ci := &settlingCIStep{}
	exec := NewExecutor(database, p, &config.Config{}, nil, []Step{&adaptiveCallStep{name: types.StepReview}, ci}, nil)
	if err := exec.ResumeCIWatch(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if ci.executed {
		t.Fatal("the watch ran against a PR that was already settled")
	}
	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunCompleted {
		t.Fatalf("run = %s, want completed", got.Status)
	}
	if sr := stepResultByName(t, database, run.ID, types.StepCI); sr.Status != types.StepStatusCompleted {
		t.Fatalf("ci step = %s, want completed", sr.Status)
	}
}
