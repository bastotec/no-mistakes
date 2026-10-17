package pipeline

import (
	"context"
	"fmt"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const actionRerunTestPreview types.ApprovalAction = "rerun-test-preview"

type testPreviewResponse struct {
	result *ipc.PublishTestPreviewResult
	err    error
}

type testPreviewRequest struct {
	ctx     context.Context
	params  ipc.PublishTestPreviewParams
	publish func(*StepContext) (*ipc.PublishTestPreviewResult, error)
	done    chan testPreviewResponse
}

func (e *Executor) PublishTestPreview(ctx context.Context, p ipc.PublishTestPreviewParams, publish func(*StepContext) (*ipc.PublishTestPreviewResult, error)) (*ipc.PublishTestPreviewResult, error) {
	e.mu.Lock()
	run, err := e.db.GetRun(p.RunID)
	if err != nil {
		e.mu.Unlock()
		return nil, err
	}
	if run == nil || run.Status != types.RunRunning || run.RepoID != p.RepoID || run.HeadSHA != p.HeadSHA || p.HeadSHA == "" {
		e.mu.Unlock()
		return nil, fmt.Errorf("preview requires an active run and matching repository and exact current head")
	}
	rows, err := e.db.GetStepsByRun(p.RunID)
	if err != nil {
		e.mu.Unlock()
		return nil, err
	}
	for _, row := range rows {
		if row.StepName == types.StepTest && row.Status == types.StepStatusPending {
			if e.skips[types.StepTest] || (e.config != nil && e.config.Test.Skip) {
				e.mu.Unlock()
				return nil, fmt.Errorf("preview requires Test to run, not be skipped")
			}
			if e.pendingTestPreview != nil {
				e.mu.Unlock()
				return nil, fmt.Errorf("a pre-Test preview request is already waiting")
			}
			request := &testPreviewRequest{ctx: ctx, params: p, publish: publish, done: make(chan testPreviewResponse, 1)}
			e.pendingTestPreview = request
			e.mu.Unlock()
			defer func() {
				e.mu.Lock()
				if e.pendingTestPreview == request {
					e.pendingTestPreview = nil
				}
				e.mu.Unlock()
			}()
			select {
			case response := <-request.done:
				return response.result, response.err
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	defer e.mu.Unlock()
	s := e.waitingContext
	if !e.waiting || e.waitingStep != types.StepTest || s == nil || e.waitingProtectedPath || e.waitingRepairReconciliation || e.waitingShutdownInterruption {
		return nil, fmt.Errorf("preview requires pending Test or a settled, parked Test gate")
	}
	sr, err := e.db.GetStepResult(e.waitingStepResultID)
	if err != nil {
		return nil, err
	}
	if sr == nil || run.AwaitingAgentSince == nil || (sr.Status != types.StepStatusAwaitingApproval && sr.Status != types.StepStatusFixReview) {
		return nil, fmt.Errorf("run is no longer parked at Test with the supplied head")
	}
	return e.publishTestPreviewAt(s, ctx, p, publish)
}

func (e *Executor) publishTestPreviewAt(s *StepContext, ctx context.Context, p ipc.PublishTestPreviewParams, publish func(*StepContext) (*ipc.PublishTestPreviewResult, error)) (*ipc.PublishTestPreviewResult, error) {
	if s.Run.ID != p.RunID || s.Repo.ID != p.RepoID || s.Run.HeadSHA != p.HeadSHA {
		return nil, fmt.Errorf("preview run, repository or exact current head changed before publication")
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
