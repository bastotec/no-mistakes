package pipeline

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
	_ "modernc.org/sqlite"
)

func TestExecutor_TerminalizesRunWhenInitialStatusWriteFails(t *testing.T) {
	database, p, run, repo := setupTest(t)
	installRunStatusFailureTrigger(t, p.DB(), string(types.RunRunning))
	events := &eventCollector{}
	exec := NewExecutor(database, p, nil, nil, []Step{newPassStep(types.StepReview)}, events.handler)

	err := exec.Execute(context.Background(), run, repo, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "update run status") {
		t.Fatalf("Execute() error = %v", err)
	}
	assertDurableFailedRunAndEvent(t, database, run.ID, events)
}

func TestExecutor_TerminalizesRunWhenFinalCompletedWriteFails(t *testing.T) {
	database, p, run, repo := setupTest(t)
	installRunStatusFailureTrigger(t, p.DB(), string(types.RunCompleted))
	events := &eventCollector{}
	exec := NewExecutor(database, p, nil, nil, []Step{newPassStep(types.StepReview)}, events.handler)

	err := exec.Execute(context.Background(), run, repo, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "update run status") {
		t.Fatalf("Execute() error = %v", err)
	}
	assertDurableFailedRunAndEvent(t, database, run.ID, events)
}

func TestExecutor_TerminalHeadPreservationSuppressesIsolatedCallbacks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("callback fixture requires a POSIX shell")
	}
	for _, isolated := range []bool{false, true} {
		for _, recorded := range []bool{false, true} {
			t.Run(fmt.Sprintf("isolated=%v/recorded=%v", isolated, recorded), func(t *testing.T) {
				database, p, _, repo := setupTest(t)
				source := survivedWorkRepo(t)
				base := gitRun(t, source, "rev-parse", "HEAD")
				run, err := database.InsertRun(repo.ID, "isolation-failure", base, base)
				if err != nil {
					t.Fatal(err)
				}
				gitRun(t, source, "commit", "--allow-empty", "-m", "integrated head")
				head := gitRun(t, source, "rev-parse", "HEAD")
				if recorded {
					if err := database.UpdateRunHeadSHA(run.ID, head); err != nil {
						t.Fatal(err)
					}
					run.HeadSHA = head
				}
				if isolated {
					if err := database.EnableTestIsolation(run.ID); err != nil {
						t.Fatal(err)
					}
				}
				work := filepath.Join(t.TempDir(), "work")
				gitRun(t, source, "worktree", "add", "--detach", work, head)
				gitRun(t, source, "config", "core.hooksPath", ".githooks")
				marker := filepath.Join(t.TempDir(), "host-callback")
				failure := errors.New("isolated evidence failed")
				step := &adaptiveCallStep{name: types.StepTest, fn: func(*StepContext) (*StepOutcome, error) {
					hooks := filepath.Join(work, ".githooks")
					if err := os.Mkdir(hooks, 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(hooks, "reference-transaction"), []byte("#!/bin/sh\nprintf called >> '"+marker+"'\n"), 0700); err != nil {
						t.Fatal(err)
					}
					return nil, failure
				}}
				executor := NewExecutor(database, p, nil, nil, []Step{step}, nil)
				if err := executor.Execute(context.Background(), run, repo, work); err == nil || err.Error() != "step test failed: "+failure.Error() {
					t.Fatalf("unexpected terminal error: %v", err)
				}
				got, err := database.GetRun(run.ID)
				if err != nil || got.Status != types.RunFailed || got.HeadSHA != head || got.TerminalHeadVerifiedAt == nil {
					t.Fatalf("terminal head was not verified: %+v %v", got, err)
				}
				if got := gitRun(t, source, "rev-parse", custody.RecoveryRef(run.ID)); got != head {
					t.Fatalf("terminal recovery anchor = %s, want %s", got, head)
				}
				_, err = os.Stat(marker)
				if isolated && !os.IsNotExist(err) || !isolated && err != nil {
					t.Fatalf("terminal hook execution isolated=%v: %v", isolated, err)
				}
			})
		}
	}
}

func installRunStatusFailureTrigger(t *testing.T, path, status string) {
	t.Helper()
	raw, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	_, err = raw.Exec(`CREATE TRIGGER reject_test_run_status
		BEFORE UPDATE OF status ON runs
		WHEN NEW.status = '` + status + `'
		BEGIN
			SELECT RAISE(FAIL, 'injected status write failure');
		END`)
	if err != nil {
		t.Fatal(err)
	}
}

func assertDurableFailedRunAndEvent(t *testing.T, database *db.DB, runID string, events *eventCollector) {
	t.Helper()
	got, err := database.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunFailed {
		t.Fatalf("durable run status = %s, want failed", got.Status)
	}
	completed := events.findRunEvent(ipc.EventRunCompleted)
	if completed == nil || completed.Status == nil || *completed.Status != string(types.RunFailed) {
		t.Fatalf("terminal event = %+v, want failed run_completed", completed)
	}
}
