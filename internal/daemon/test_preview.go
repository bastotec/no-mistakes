package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
)

func (m *RunManager) HandlePublishTestPreview(ctx context.Context, p ipc.PublishTestPreviewParams) (*ipc.PublishTestPreviewResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if m.shuttingDown.Load() {
		return nil, fmt.Errorf("daemon is shutting down")
	}
	run, err := m.db.GetRun(p.RunID)
	if err != nil {
		return nil, err
	}
	if run == nil || run.RepoID != p.RepoID {
		return nil, fmt.Errorf("preview run does not belong to requested repository")
	}
	branchMu := m.branchLock(run.RepoID, run.Branch)
	branchMu.Lock()
	defer branchMu.Unlock()
	m.mu.Lock()
	exec := m.executors[p.RunID]
	m.mu.Unlock()
	if exec == nil {
		return nil, fmt.Errorf("no active executor for preview run %s", p.RunID)
	}
	return exec.PublishTestPreview(ctx, p, steps.PublishTestPreview)
}
