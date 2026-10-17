package pipeline

import (
	"context"
	"fmt"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// PublishTestPreview serializes publication against gate responses. The wait's
// cancellation cleanup also takes mu, so terminal custody cannot settle while
// publication is in flight. The publisher inherits cancellation from BOTH the
// request and run. It must never complete, skip or approve the Test step.
func (e *Executor) PublishTestPreview(ctx context.Context, p ipc.PublishTestPreviewParams, publish func(*StepContext) (*ipc.PublishTestPreviewResult, error)) (*ipc.PublishTestPreviewResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.waitingContext
	if !e.waiting || e.waitingStep != types.StepTest || s == nil || e.waitingProtectedPath || e.waitingRepairReconciliation || e.waitingShutdownInterruption {
		return nil, fmt.Errorf("preview requires a settled, parked Test gate on an active executor")
	}
	if s.Run.ID != p.RunID || s.Repo.ID != p.RepoID || s.Run.HeadSHA != p.HeadSHA || p.HeadSHA == "" {
		return nil, fmt.Errorf("preview run, repository or exact current head does not match operator evidence")
	}
	run, err := e.db.GetRun(p.RunID)
	if err != nil {
		return nil, err
	}
	sr, err := e.db.GetStepResult(e.waitingStepResultID)
	if err != nil {
		return nil, err
	}
	if run == nil || sr == nil || run.Status != types.RunRunning || run.HeadSHA != p.HeadSHA || run.AwaitingAgentSince == nil || (sr.Status != types.StepStatusAwaitingApproval && sr.Status != types.StepStatusFixReview) {
		return nil, fmt.Errorf("run is no longer parked at Test with the supplied head")
	}
	opCtx, cancel := context.WithTimeout(s.Ctx, 2*time.Minute)
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := opCtx.Err(); err != nil {
		return nil, err
	}
	copy := *s
	copy.Ctx = opCtx
	return publish(&copy)
}
