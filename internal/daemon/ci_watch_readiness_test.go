package daemon

import (
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A recovered run whose CI step will watch again loses its pre-restart
// readiness before IPC opens; a run past CI keeps it.
func TestClearResumedWatchReadiness(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	repo, err := d.InsertRepoWithID("r", "/tmp/r", "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	newRun := func(ciStatus types.StepStatus) *db.Run {
		t.Helper()
		run, err := d.InsertRun(repo.ID, "feature-"+string(ciStatus), "abc", "def")
		if err != nil {
			t.Fatal(err)
		}
		ci, err := d.InsertStepResult(run.ID, types.StepCI)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.UpdateStepStatusWithDuration(ci.ID, ciStatus, 1); err != nil {
			t.Fatal(err)
		}
		if err := d.SetRunCIReady(run.ID, true); err != nil {
			t.Fatal(err)
		}
		if run, err = d.GetRun(run.ID); err != nil {
			t.Fatal(err)
		}
		return run
	}
	watching := newRun(types.StepStatusRunning)
	if err := clearResumedWatchReadiness(d, watching); err != nil {
		t.Fatal(err)
	}
	if got, _ := d.GetRun(watching.ID); got.CIReadyAt != nil || watching.CIReadyAt != nil {
		t.Fatal("a resuming watch kept its pre-restart readiness")
	}
	done := newRun(types.StepStatusCompleted)
	if err := clearResumedWatchReadiness(d, done); err != nil {
		t.Fatal(err)
	}
	if got, _ := d.GetRun(done.ID); got.CIReadyAt == nil {
		t.Fatal("a run past CI lost its readiness")
	}
}
