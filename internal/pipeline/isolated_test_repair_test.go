package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func isolatedRepairContext(t *testing.T) *StepContext {
	t.Helper()
	database, _, run, repo := setupTest(t)
	step, err := database.InsertStepResult(run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStepWithAutoFixLimit(step.ID, 1); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"test-1","severity":"error","action":"auto-fix","description":"fixture failure"}]}`
	round, err := database.InsertStepRound(step.ID, 1, "initial", &findings, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if decision, err := database.ReserveStepRepairWithSelection(step.ID, round.ID, 1, nil); err != nil || !decision.Granted {
		t.Fatalf("reserve repair: %+v %v", decision, err)
	}
	if err := database.ClaimStepRepair(step.ID, round.ID); err != nil {
		t.Fatal(err)
	}
	return &StepContext{DB: database, Run: run, Repo: repo, Fixing: true, StepResultID: step.ID, WorkDir: t.TempDir(), IsolatedRepairRound: func() string { return round.ID }}
}
func TestIsolatedTestRepairTerminalReplayDoesNotRepeatSideEffects(t *testing.T) {
	sctx := isolatedRepairContext(t)
	calls := 0
	work := func(mark func(int) error) (*StepOutcome, error) {
		calls++
		if err := mark(12345); err != nil {
			return nil, err
		}
		return &StepOutcome{FixSummary: "isolated fixture repaired", Findings: `{"findings":[]}`}, nil
	}
	first, err := sctx.RunIsolatedTestRepair(work)
	if err != nil {
		t.Fatal(err)
	}
	// New StepContext represents a recovered executor; the only replay source
	// is the durable invocation, never an agent's judgement about its old tools.
	recovered := *sctx
	second, err := recovered.RunIsolatedTestRepair(work)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || first.FixSummary != second.FixSummary || first.Findings != second.Findings {
		t.Fatalf("repair was replayed: calls=%d first=%+v second=%+v", calls, first, second)
	}
}
func TestIsolatedTestInterruptedRepairRefusesReplay(t *testing.T) {
	sctx := isolatedRepairContext(t)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("expected simulated process interruption")
			}
		}()
		sctx.RunIsolatedTestRepair(func(mark func(int) error) (*StepOutcome, error) {
			if err := mark(12345); err != nil {
				t.Fatal(err)
			}
			panic("simulated daemon death before terminal result")
		})
	}()
	called := false
	_, err := sctx.RunIsolatedTestRepair(func(func(int) error) (*StepOutcome, error) { called = true; return &StepOutcome{}, nil })
	if err == nil || !strings.Contains(err.Error(), "refusing to replay") || called {
		t.Fatalf("interrupted side effects were replayed: called=%v err=%v", called, err)
	}
}

func TestIsolatedTestRepairReplaysTypedFailures(t *testing.T) {
	for _, failure := range []error{fmt.Errorf("fix: %w", ErrTestAgentTimeout), fmt.Errorf("fix: %w", &ProtectedPathError{Path: "package.lock", Rule: "*.lock"})} {
		t.Run(failure.Error(), func(t *testing.T) {
			sctx := isolatedRepairContext(t)
			calls := 0
			work := func(mark func(int) error) (*StepOutcome, error) {
				calls++
				if err := mark(os.Getpid()); err != nil {
					return nil, err
				}
				payload, err := json.Marshal(EncodeIsolatedTestError(failure))
				if err != nil {
					return nil, err
				}
				var received IsolatedTestError
				if err := json.Unmarshal(payload, &received); err != nil {
					return nil, err
				}
				return nil, received.Restore()
			}
			for attempt := 0; attempt < 2; attempt++ {
				_, err := sctx.RunIsolatedTestRepair(work)
				if errors.Is(failure, ErrTestAgentTimeout) {
					if !errors.Is(err, ErrTestAgentTimeout) {
						t.Fatalf("timeout identity lost on attempt %d: %v", attempt, err)
					}
				} else {
					var refusal *ProtectedPathError
					if !errors.As(err, &refusal) || refusal.Path != "package.lock" || refusal.Rule != "*.lock" {
						t.Fatalf("refusal details lost on attempt %d: %v", attempt, err)
					}
					outcome := ProtectedPathOutcome(err)
					if outcome == nil || !outcome.NeedsApproval || !HasProtectedPathRefusal(outcome.Findings) {
						t.Fatalf("refusal no longer parks: %+v", outcome)
					}
				}
			}
			if calls != 1 {
				t.Fatalf("terminal failure reran worker %d times", calls)
			}
		})
	}
}

func TestExecutor_IsolatedTestTimeoutCapturesSurvivedWork(t *testing.T) {
	database, p, run, repo := setupTest(t)
	dir := survivedWorkRepo(t)
	step := &adaptiveCallStep{name: types.StepTest, fn: func(sctx *StepContext) (*StepOutcome, error) {
		if !sctx.Fixing {
			return &StepOutcome{NeedsApproval: true, AutoFixable: true, Findings: `{"findings":[{"id":"selected","severity":"error","description":"repair","action":"auto-fix"}]}`}, nil
		}
		work := func(mark func(int) error) (*StepOutcome, error) {
			if err := mark(os.Getpid()); err != nil {
				return nil, err
			}
			writeFile(t, dir, "progress.txt", "survived")
			encoded, err := json.Marshal(EncodeIsolatedTestError(fmt.Errorf("isolated fix: %w", ErrTestAgentTimeout)))
			if err != nil {
				return nil, err
			}
			var received IsolatedTestError
			if err := json.Unmarshal(encoded, &received); err != nil {
				return nil, err
			}
			return nil, received.Restore()
		}
		if _, err := sctx.RunIsolatedTestRepair(work); !errors.Is(err, ErrTestAgentTimeout) {
			return nil, fmt.Errorf("initial timeout lost: %v", err)
		}
		return sctx.RunIsolatedTestRepair(func(func(int) error) (*StepOutcome, error) { t.Error("terminal repair reran"); return nil, nil })
	}}
	executor := NewExecutor(database, p, &config.Config{AutoFix: config.AutoFix{Test: 1}}, nil, []Step{step}, nil)
	_ = executor.Execute(context.Background(), run, repo, dir)
	work, err := database.GetRunSurvivedWork(run.ID)
	if err != nil || work == nil || work.State != types.SurvivedWorkCaptured {
		t.Fatalf("isolated timeout left no handoff: %+v %v", work, err)
	}
	if got := gitRun(t, dir, "show", work.Ref+":progress.txt"); got != "survived" {
		t.Fatalf("repair lost: %q", got)
	}
}

func TestExecutor_IsolatedTestShutdownRetainsRecoverableRepairGate(t *testing.T) {
	database, p, run, repo := setupTest(t)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	step := &adaptiveCallStep{name: types.StepTest, fn: func(sctx *StepContext) (*StepOutcome, error) {
		if !sctx.Fixing {
			return &StepOutcome{NeedsApproval: true, AutoFixable: true, Findings: `{"findings":[{"id":"test-1","severity":"error","description":"repair","action":"auto-fix"}]}`}, nil
		}
		return sctx.RunIsolatedTestRepair(func(mark func(int) error) (*StepOutcome, error) {
			if err := mark(12345); err != nil {
				return nil, err
			}
			cancel(ErrDaemonShutdown)
			return nil, errors.Join(context.Cause(sctx.Ctx), ErrIsolatedTestHandoff)
		})
	}}
	executor := NewExecutor(database, p, &config.Config{}, nil, []Step{step}, nil)
	done := make(chan struct{})
	var executeErr error
	workdir := t.TempDir()
	go func() {
		executeErr = executor.Execute(ctx, run, repo, workdir)
		close(done)
	}()
	defer func() { cancel(nil); <-done }()
	waitForStepStatus(t, database, run.ID, types.StepTest, types.StepStatusAwaitingApproval)
	if err := executor.Respond(types.StepTest, types.ActionFix, []string{"test-1"}); err != nil {
		t.Fatal(err)
	}
	<-done
	if !errors.Is(executeErr, ErrDaemonShutdown) {
		t.Fatalf("shutdown classification lost: %v", executeErr)
	}
	results, err := database.GetStepsByRun(run.ID)
	if err != nil || len(results) != 1 || results[0].Status != types.StepStatusFixing {
		t.Fatalf("shutdown failed active Test repair: %+v %v", results, err)
	}
	rounds, err := database.GetRoundsByStep(results[0].ID)
	if err != nil || len(rounds) == 0 {
		t.Fatalf("repair round missing: %+v %v", rounds, err)
	}
	if _, err := database.MarkRepairInvocationUnresolved(results[0].ID, rounds[len(rounds)-1].ID); err != nil {
		t.Fatal(err)
	}
	stored, err := database.GetRun(run.ID)
	if err != nil || stored.Status != types.RunRunning {
		t.Fatalf("shutdown failed active run: %+v %v", stored, err)
	}
	gate, err := executor.recoveredGate(run.ID)
	if err != nil || gate == nil || !gate.repairUnresolved {
		t.Fatalf("interrupted repair gate is not recoverable: %+v %v", gate, err)
	}
}

func TestIsolatedTestIncompleteHandoffRetainsRepairCustody(t *testing.T) {
	sctx := isolatedRepairContext(t)
	_, err := sctx.RunIsolatedTestRepair(func(mark func(int) error) (*StepOutcome, error) {
		if err := mark(os.Getpid()); err != nil {
			return nil, err
		}
		return nil, errors.Join(ErrTestAgentTimeout, ErrIsolatedTestHandoff)
	})
	if !errors.Is(err, ErrTestAgentTimeout) || !errors.Is(err, ErrIsolatedTestHandoff) {
		t.Fatalf("handoff failure lost: %v", err)
	}
	called := false
	_, err = sctx.RunIsolatedTestRepair(func(func(int) error) (*StepOutcome, error) { called = true; return &StepOutcome{}, nil })
	if err == nil || !strings.Contains(err.Error(), "refusing to replay") || called {
		t.Fatalf("incomplete handoff replayed: called=%v err=%v", called, err)
	}
}
