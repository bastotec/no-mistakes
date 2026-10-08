package daemon

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gatecontext"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestShutdownRefusesActiveAgentPeer(t *testing.T) {
	p, database := startTestDaemon(t)
	repo, err := database.InsertRepo(t.TempDir(), "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := database.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatalf("mark run active: %v", err)
	}
	step, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatalf("insert step: %v", err)
	}
	if err := database.StartStep(step.ID); err != nil {
		t.Fatalf("start step: %v", err)
	}
	pid := os.Getpid()
	if err := database.SetStepAgentActivity(step.ID, "agent started", &pid); err != nil {
		t.Fatalf("set agent pid: %v", err)
	}

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatalf("dial daemon: %v", err)
	}
	defer client.Close()
	err = client.Call(ipc.MethodShutdown, &ipc.ShutdownParams{}, &ipc.ShutdownResult{})
	if err == nil || !strings.Contains(err.Error(), gatecontext.ErrorCode) {
		t.Fatalf("shutdown error = %v, want %s refusal", err, gatecontext.ErrorCode)
	}
	var health ipc.HealthResult
	if err := client.CallWithTimeout(ipc.MethodHealth, &ipc.HealthParams{}, &health, time.Second); err != nil {
		t.Fatalf("daemon stopped after refused shutdown: %v", err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunCompleted); err != nil {
		t.Fatalf("complete run: %v", err)
	}
}

func TestRunManagerShutdownParksUnrecoverableActiveInvocation(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	repo, err := database.InsertRepo(t.TempDir(), "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	step, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(step.ID); err != nil {
		t.Fatal(err)
	}

	manager := NewRunManager(database, p, func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepReview}}
	})
	runCtx, cancel := context.WithCancelCause(context.Background())
	manager.mu.Lock()
	manager.cancels[run.ID] = cancel
	manager.mu.Unlock()
	manager.wg.Add(1)
	go func() {
		defer manager.wg.Done()
		<-runCtx.Done()
	}()

	manager.Shutdown()

	storedRun, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedRun.Status != types.RunRunning || storedRun.AwaitingAgentSince == nil {
		t.Fatalf("run after shutdown = status %s, awaiting %v", storedRun.Status, storedRun.AwaitingAgentSince)
	}
	storedStep, err := database.GetStepResult(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedStep.Status != types.StepStatusAwaitingApproval || storedStep.FindingsJSON == nil || !pipeline.HasDaemonShutdownInterruption(*storedStep.FindingsJSON) {
		t.Fatalf("step after shutdown = status %s, findings %v", storedStep.Status, storedStep.FindingsJSON)
	}
	if err := pipeline.ValidateRecoveredRun(database, storedRun, manager.steps()); err != nil {
		t.Fatalf("shutdown park is not recoverable: %v", err)
	}
}
