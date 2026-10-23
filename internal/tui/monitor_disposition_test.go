package tui

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestFrozenMonitorLiveEventAndSnapshotRenderStopped(t *testing.T) {
	for _, state := range []types.StepName{types.StepCI, types.StepReview} {
		t.Run(string(state), func(t *testing.T) {
			run := testRun()
			run.CIReady, run.CIReadyNoCI = true, true
			status := types.StepStatusRunning
			if state == types.StepReview {
				status = types.StepStatusAwaitingApproval
			}
			run.Steps = []ipc.StepResultInfo{{StepName: state, Status: status}}
			m := NewModel("/unused/sock", nil, run)
			m.width = 100
			m.logs = []string{"checks passed"}
			stopped, ready := true, false
			m.applyEvent(ipc.Event{Type: ipc.EventRunUpdated, RunID: run.ID, StateRev: 2, MonitorStopped: &stopped, CIReady: &ready, CIReadyNoCI: &ready})
			if !m.run.MonitorStopped || m.run.CIReady || m.run.CIReadyNoCI || m.stateRev != 2 {
				t.Fatalf("frozen event not applied: %+v", m.run)
			}
			assertStopped := func(m Model) {
				t.Helper()
				view := stripANSI(m.View())
				if !strings.Contains(view, "Monitor stopped") || strings.Contains(view, "✓ Checks passed") || strings.Contains(view, "still monitoring until merged or closed") {
					t.Fatalf("incorrect frozen view: %s", view)
				}
				if !strings.Contains(m.terminalTitle(), "Monitor stopped") {
					t.Fatalf("incorrect frozen title: %s", m.terminalTitle())
				}
			}
			assertStopped(m)
			stopped = false
			m.applyEvent(ipc.Event{Type: ipc.EventRunUpdated, RunID: run.ID, StateRev: 1, MonitorStopped: &stopped})
			if !m.run.MonitorStopped {
				t.Fatal("older event regressed frozen state")
			}
			snapshot := NewModel("/unused/sock", nil, m.run)
			snapshot.width = 100
			assertStopped(snapshot)
		})
	}
}
