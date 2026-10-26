package pipeline

import (
	"context"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type monitorStopRequest struct {
	ctx    context.Context
	pin    db.MonitorDisposition
	result chan error
}

// StopMonitor never cancels the executor or a repair. Polling requests are
// acknowledged only by the next quiescent CI boundary; a parked Review uses
// the same mutex as Respond, so an accepted response wins or the freeze wins.
func (e *Executor) StopMonitor(ctx context.Context, p db.MonitorDisposition) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	if e.monitorFrozen {
		err := e.db.FreezeMonitor(p)
		e.mu.Unlock()
		return err
	}
	if err := e.db.ValidateMonitorDisposition(p); err != nil {
		e.mu.Unlock()
		return err
	}
	if p.State == "parked-review" {
		if !e.waiting || e.waitingStep != types.StepReview || e.waitingContext == nil || e.waitingRepairReconciliation || e.waitingShutdownInterruption {
			e.mu.Unlock()
			return fmt.Errorf("executor is not parked at Review")
		}
		if err := ctx.Err(); err != nil {
			e.mu.Unlock()
			return err
		}
		if err := e.waitingContext.Ctx.Err(); err != nil {
			e.mu.Unlock()
			return err
		}
		err := e.freezeMonitor(p)
		e.mu.Unlock()
		return err
	}
	e.mu.Unlock()
	req := monitorStopRequest{ctx: ctx, pin: p, result: make(chan error, 1)}
	select {
	case e.monitorStopCh <- req:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-req.result:
		return err
	case <-ctx.Done():
		return fmt.Errorf("monitor boundary acknowledgement unconfirmed: %w", ctx.Err())
	}
}

func (e *Executor) freezeMonitor(p db.MonitorDisposition) error {
	if err := e.db.FreezeMonitor(p); err != nil {
		return err
	}
	e.monitorFrozen = true
	stopped, ready := true, false
	e.onEvent(ipc.Event{
		Type:           ipc.EventRunUpdated,
		RunID:          p.RunID,
		RepoID:         p.RepoID,
		MonitorStopped: &stopped,
		CIReady:        &ready,
		CIReadyNoCI:    &ready,
	})
	return nil
}

func (e *Executor) monitorCheckpoint(ctx context.Context) error {
	select {
	case req := <-e.monitorStopCh:
		e.mu.Lock()
		err := req.ctx.Err()
		if err == nil {
			err = ctx.Err()
		}
		if err == nil {
			err = e.freezeMonitor(req.pin)
		}
		req.result <- err
		e.mu.Unlock()
	default:
	}
	e.mu.Lock()
	frozen := e.monitorFrozen
	e.mu.Unlock()
	if frozen {
		<-ctx.Done()
		return context.Cause(ctx)
	}
	return nil
}
