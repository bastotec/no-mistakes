package daemon

import (
	"context"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/db"
)

// HandleStopMonitor delegates to the executor that owns the actual boundary.
// A persisted freeze is replayable after restart without starting an executor.
func (m *RunManager) HandleStopMonitor(ctx context.Context, p db.MonitorDisposition) error {
	run, err := m.db.GetRun(p.RunID)
	if err != nil || run == nil {
		return fmt.Errorf("unknown run: %s", p.RunID)
	}
	branchMu := m.branchLock(run.RepoID, run.Branch)
	branchMu.Lock()
	defer branchMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	frozen, err := m.db.MonitorFrozen(p.RunID)
	if err != nil {
		return err
	}
	if frozen {
		return m.db.FreezeMonitor(p)
	}
	m.mu.Lock()
	exec := m.executors[p.RunID]
	m.mu.Unlock()
	if exec == nil {
		return fmt.Errorf("no owning executor; monitor cessation is unconfirmed")
	}
	return exec.StopMonitor(ctx, p)
}
