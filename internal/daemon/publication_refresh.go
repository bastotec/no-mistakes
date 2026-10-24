package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

func (m *RunManager) HandleRefreshPublication(ctx context.Context, p ipc.RefreshPublicationParams) (*ipc.RefreshPublicationResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	if m.shuttingDown.Load() {
		return nil, fmt.Errorf("daemon is shutting down")
	}
	m.mu.Lock()
	exec := m.executors[p.RunID]
	m.mu.Unlock()
	if exec == nil {
		return nil, fmt.Errorf("refresh requires this run's active CI executor; terminal publication retry is a separate operation")
	}
	return exec.RefreshPublication(ctx, p)
}
