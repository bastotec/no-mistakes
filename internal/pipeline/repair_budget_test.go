package pipeline

import (
	"context"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExhaustedRepairFindingsKeepsUnsafeIDsOutOfGuidance(t *testing.T) {
	ids := []string{"space id", "comma,id", `quote"id`, "$(touch-pwn)", "--leading"}
	raw, err := types.MarshalFindingsJSON(types.Findings{
		Items: []types.Finding{
			{ID: ids[0], Severity: "error", Description: "CI failed", Action: types.ActionAutoFix},
			{ID: ids[1], Severity: "error", Description: "CI failed", Action: types.ActionAutoFix},
			{ID: ids[2], Severity: "error", Description: "CI failed", Action: types.ActionAutoFix},
			{ID: ids[3], Severity: "error", Description: "CI failed", Action: types.ActionAutoFix},
			{ID: ids[4], Severity: "error", Description: "CI failed", Action: types.ActionAutoFix},
		},
		Summary: "blocked",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := exhaustedRepairFindings(raw, types.StepCI, "run-1", db.RepairBudgetDecision{
		Consumed:                0,
		Limit:                   0,
		AuthorityLimit:          0,
		ExplicitRepairAvailable: true,
	})
	findings, err := types.ParseFindingsJSON(got)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range ids {
		if findings.Items[i].ID != want {
			t.Fatalf("finding %d ID = %q, want %q", i, findings.Items[i].ID, want)
		}
	}
	budget := findings.Items[len(findings.Items)-1]
	if budget.Category != repairBudgetFindingCategory || !strings.Contains(budget.Description, "Automatic repair budget exhausted") || !strings.Contains(budget.Description, "axi status") {
		t.Fatalf("legacy authority guidance = %+v", budget)
	}
	for _, id := range ids {
		if strings.Contains(budget.Description, id) {
			t.Fatalf("repair guidance interpolated finding ID %q: %q", id, budget.Description)
		}
	}
	if strings.Contains(budget.Description, "axi respond") {
		t.Fatalf("repair guidance claimed an exact response command: %q", budget.Description)
	}
}

func TestExecutor_ResumedLegacyOverrunRequiresOneBoundedResponse(t *testing.T) {
	database, p, run, repo := setupTest(t)
	database.UpdateRunStatus(run.ID, types.RunRunning)
	sr, err := database.InsertStepResult(run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStepWithAutoFixLimit(sr.ID, 3); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"ci-red","severity":"error","description":"serial check failed","action":"auto-fix"}],"summary":"serial check failed"}`
	for n := 1; n <= 5; n++ {
		trigger := "initial"
		if n > 1 {
			trigger = "user_fix"
		}
		if _, err := database.InsertStepRound(sr.ID, n, trigger, &findings, nil, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.ParkStepForApproval(run.ID, sr.ID, types.StepStatusFixReview, 1, 10, &findings); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := database.GetRoundsByStep(sr.ID)
	var calls atomic.Int32
	step := &adaptiveCallStep{name: types.StepCI, fn: func(sctx *StepContext) (*StepOutcome, error) {
		calls.Add(1)
		return &StepOutcome{}, nil
	}}
	exec := NewExecutor(database, p, &config.Config{AutoFix: config.AutoFix{CI: 3}}, &usageAgent{}, []Step{step}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var finished atomic.Bool
	t.Cleanup(func() {
		cancel()
		if !finished.Load() {
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Error("resume did not stop")
			}
		}
	})
	workDir := t.TempDir()
	go func() { err := exec.Resume(ctx, run, repo, workDir); finished.Store(true); done <- err }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		exec.mu.Lock()
		waiting := exec.waiting
		exec.mu.Unlock()
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("resumed gate not ready")
		}
		time.Sleep(time.Millisecond)
	}
	invocations, err := database.GetAgentInvocationsByRun(run.ID)
	if err != nil || len(invocations) != 0 || calls.Load() != 0 {
		t.Fatalf("resumption launched without authority: calls=%d invocations=%d %v", calls.Load(), len(invocations), err)
	}
	preserved, _ := database.GetRoundsByStep(sr.ID)
	if !reflect.DeepEqual(before, preserved) {
		t.Fatal("resumption rewrote legacy overrun records")
	}
	current, _ := database.GetRun(run.ID)
	if current.HeadSHA != run.HeadSHA {
		t.Fatal("resumption changed custody before authority")
	}
	if err := exec.Respond(types.StepCI, types.ActionFix, []string{"ci-red"}); err == nil {
		t.Fatal("configured ceiling accepted an impossible recovered repair")
	}
	select {
	case err := <-done:
		finished.Store(true)
		t.Fatalf("impossible recovered repair ended the run: %v", err)
	default:
	}
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil || len(steps) != 1 || steps[0].Status != types.StepStatusFixReview {
		t.Fatalf("impossible recovered repair did not preserve gate: %+v %v", steps, err)
	}
	if err := exec.Respond(types.StepCI, types.ActionApprove, nil); err != nil {
		t.Fatal(err)
	}
	waitExecutorDone(t, done)
	if calls.Load() != 0 {
		t.Fatal("configured ceiling launched a resumed repair")
	}
}
