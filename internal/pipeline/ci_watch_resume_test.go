package pipeline

import (
	"context"
	"errors"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
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
