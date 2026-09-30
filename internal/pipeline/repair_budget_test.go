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

func TestExhaustedRepairFindingsProvidesValidFixSelection(t *testing.T) {
	raw := `{"findings":[{"id":"ci-red","severity":"error","description":"CI failed","action":"auto-fix"},{"id":"needs-user","severity":"warning","description":"needs input","action":"ask-user"}],"summary":"blocked"}`
	got := exhaustedRepairFindings(raw, types.StepCI, "run-1", db.RepairBudgetDecision{
		Consumed:       1,
		Limit:          3,
		AuthorityLimit: 1,
	})
	findings, err := types.ParseFindingsJSON(got)
	if err != nil {
		t.Fatal(err)
	}
	budget := findings.Items[len(findings.Items)-1]
	want := "`no-mistakes axi respond --step ci --action fix --findings ci-red`"
	if !strings.Contains(budget.Description, want) {
		t.Fatalf("repair budget guidance = %q, want command %s", budget.Description, want)
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
