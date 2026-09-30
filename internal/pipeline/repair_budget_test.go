package pipeline

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

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
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	step := &adaptiveCallStep{name: types.StepCI, fn: func(sctx *StepContext) (*StepOutcome, error) {
		if !sctx.Fixing {
			t.Error("resume reran the recorded validation instead of waiting for authority")
		}
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
		case <-sctx.Ctx.Done():
			return nil, sctx.Ctx.Err()
		}
		if _, err := sctx.Agent.Run(sctx.Ctx, agent.RunOpts{Prompt: "authorized repair"}); err != nil {
			return nil, err
		}
		return &StepOutcome{NeedsApproval: true, AutoFixable: true, Findings: findings}, nil
	}}
	exec := NewExecutor(database, p, &config.Config{AutoFix: config.AutoFix{CI: 3}}, &usageAgent{}, []Step{step}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var finished atomic.Bool
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
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
	if err := exec.Respond(types.StepCI, types.ActionFix, []string{"ci-red"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("explicit fix did not start")
	}
	// Preserve the started invocation; repeated responses cannot launch or cancel
	// it while it is in flight.
	for n := 0; n < 10; n++ {
		if err := exec.Respond(types.StepCI, types.ActionFix, []string{"ci-red"}); err == nil {
			t.Fatal("retry accepted without a gate")
		}
	}
	if calls.Load() != 1 {
		t.Fatal("duplicate in-flight launch")
	}
	releaseOnce.Do(func() { close(release) })
	waitForStepStatus(t, database, run.ID, types.StepCI, types.StepStatusFixReview)
	invocations, err = database.GetAgentInvocationsByRun(run.ID)
	if err != nil || len(invocations) != 1 || calls.Load() != 1 {
		t.Fatalf("resumed repair renewed automatic budget: calls=%d invocations=%d %v", calls.Load(), len(invocations), err)
	}
	steps, _ := database.GetStepsByRun(run.ID)
	if steps[0].FindingsJSON == nil || !strings.Contains(*steps[0].FindingsJSON, "consumed 5 repairs") || !strings.Contains(*steps[0].FindingsJSON, "requested additional repair 6") {
		t.Fatalf("missing stable authority request: %+v", steps[0])
	}
	if err := exec.Respond(types.StepCI, types.ActionApprove, nil); err != nil {
		t.Fatal(err)
	}
	waitExecutorDone(t, done)
}
