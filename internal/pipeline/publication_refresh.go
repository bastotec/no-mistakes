package pipeline

import (
	"context"
	"fmt"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type publicationRefreshResponse struct {
	result *ipc.RefreshPublicationResult
	err    error
}

// PublicationRefreshRequest is consumed only by the CI monitor between polls,
// never concurrently with a repair, push, or executor gate transition.
type PublicationRefreshRequest struct {
	Params ipc.RefreshPublicationParams
	ctx    context.Context
	done   chan publicationRefreshResponse
}

func (r *PublicationRefreshRequest) Serve(s *StepContext, publish func(*StepContext, ipc.RefreshPublicationParams) (*ipc.RefreshPublicationResult, error)) {
	ctx, cancel := context.WithTimeout(s.Ctx, 2*time.Minute)
	defer cancel()
	stop := context.AfterFunc(r.ctx, cancel)
	defer stop()
	copy := *s
	copy.Ctx = ctx
	if err := r.ctx.Err(); err != nil {
		r.done <- publicationRefreshResponse{err: err}
		return
	}
	result, err := publish(&copy, r.Params)
	r.done <- publicationRefreshResponse{result, err}
}

func (e *Executor) RefreshPublication(ctx context.Context, p ipc.RefreshPublicationParams) (*ipc.RefreshPublicationResult, error) {
	run, err := e.db.GetRun(p.RunID)
	if err != nil {
		return nil, err
	}
	if run == nil || run.Status != types.RunRunning || run.RepoID != p.RepoID || run.HeadSHA != p.HeadSHA || run.PRURL == nil || *run.PRURL != p.PRURL {
		return nil, fmt.Errorf("refresh requires the active run's exact repository, PR and current head")
	}
	rows, err := e.db.GetStepsByRun(p.RunID)
	if err != nil {
		return nil, err
	}
	monitoring := false
	for _, row := range rows {
		if row.StepName == types.StepCI && row.Status == types.StepStatusRunning {
			monitoring = true
		}
	}
	if !monitoring || run.AwaitingAgentSince != nil {
		return nil, fmt.Errorf("refresh requires a running CI monitor, not a repair or approval gate")
	}
	request := &PublicationRefreshRequest{Params: p, ctx: ctx, done: make(chan publicationRefreshResponse, 1)}
	select {
	case e.publicationRefreshCh <- request:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case response := <-request.done:
		return response.result, response.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
