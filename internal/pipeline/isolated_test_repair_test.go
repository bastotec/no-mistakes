package pipeline

import (
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
