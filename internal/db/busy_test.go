package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Separate processes (the daemon, a repair wrapper, the CLI) each open the
// database. A transaction that reads before it writes must wait for another
// writer rather than fail with SQLITE_BUSY: a lock must never fail a run.
func TestConcurrentWritersAcrossHandlesNeverHitBusy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	repo, err := first.InsertRepo("/tmp/repo", "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := first.InsertRun(repo.ID, "feature/x", "head", "base")
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 1000)
	for _, d := range []*DB{first, second, first, second} {
		wg.Add(1)
		go func(d *DB) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				if err := d.UpdateRunPRState(run.ID, "open"); err != nil {
					errs <- err
				}
				if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
					errs <- err
				}
				if err := d.UpdateRunError(run.ID, "transient"); err != nil {
					errs <- err
				}
			}
		}(d)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent write failed: %v", err)
	}
}

// A lifecycle write outlasts a lock held past busy_timeout by retrying.
func TestLifecycleWriteRetriesPastALongLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	repo, err := d.InsertRepo("/tmp/repo", "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature/x", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	// Another process holds the write lock longer than this handle waits.
	holder, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(1)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	tx, err := holder.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec(`PRAGMA busy_timeout = 50`); err != nil {
		t.Fatal(err)
	}
	saved := busyRetryDelays
	busyRetryDelays = []time.Duration{100 * time.Millisecond, 100 * time.Millisecond, 100 * time.Millisecond}
	defer func() { busyRetryDelays = saved }()
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = tx.Rollback()
	}()
	if err := d.UpdateRunStatus(run.ID, types.RunFailed); err != nil {
		t.Fatalf("status write lost to a lock: %v", err)
	}
	got, err := d.GetRun(run.ID)
	if err != nil || got.Status != types.RunFailed {
		t.Fatalf("run = %+v, %v", got, err)
	}
}

// Beginning a read-then-write transaction must acquire the reservation before
// taking a snapshot. Otherwise a competing commit makes promotion fail at once.
func TestReadWriteTransactionWaitsBeforeTakingSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var timeout int
	if err := d.sql.QueryRow(`PRAGMA busy_timeout`).Scan(&timeout); err != nil || timeout != 30000 {
		t.Fatalf("busy timeout = %d, %v; want 30000", timeout, err)
	}
	repo, err := d.InsertRepo("/tmp/repo", "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature/x", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	holder, err := sql.Open("sqlite", path+"?_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	tx, err := holder.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE runs SET pr_state = 'open' WHERE id = ?`, run.ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	begun := make(chan *sql.Tx, 1)
	beginErr := make(chan error, 1)
	go func() {
		next, err := d.sql.BeginTx(ctx, nil)
		if err != nil {
			beginErr <- err
			return
		}
		begun <- next
	}()
	select {
	case next := <-begun:
		next.Rollback()
		t.Fatal("transaction began while a competing writer held its reservation")
	case err := <-beginErr:
		t.Fatal(err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case next := <-begun:
		defer next.Rollback()
		var state string
		if err := next.QueryRow(`SELECT pr_state FROM runs WHERE id = ?`, run.ID).Scan(&state); err != nil || state != "open" {
			t.Fatalf("snapshot = %q, %v; want competing committed state", state, err)
		}
		if _, err := next.Exec(`UPDATE runs SET pr_state = 'closed' WHERE id = ?`, run.ID); err != nil {
			t.Fatalf("promote transaction to writer: %v", err)
		}
		if err := next.Commit(); err != nil {
			t.Fatal(err)
		}
	case err := <-beginErr:
		t.Fatal(err)
	case <-time.After(3 * time.Second):
		t.Fatal("transaction did not resume after writer committed")
	}
}

// Every lifecycle entrypoint must survive an actual busy-timeout expiration,
// not merely a shorter lock that SQLite's own busy handler can wait out.
func TestLifecycleEntrypointsRetryAfterBusyTimeout(t *testing.T) {
	for _, operation := range []string{"status", "error", "error-status", "verified-error", "verified-status", "step-failure", "pr-state"} {
		t.Run(operation, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.sqlite")
			d, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			repo, err := d.InsertRepo("/tmp/repo", "https://github.com/test/repo", "main")
			if err != nil {
				t.Fatal(err)
			}
			run, err := d.InsertRun(repo.ID, "feature/x", "head", "base")
			if err != nil {
				t.Fatal(err)
			}
			step, err := d.InsertStepResult(run.ID, types.StepCI)
			if err != nil {
				t.Fatal(err)
			}
			if err := d.StartStep(step.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := d.sql.Exec(`PRAGMA busy_timeout = 20`); err != nil {
				t.Fatal(err)
			}
			holder, err := sql.Open("sqlite", path+"?_txlock=immediate")
			if err != nil {
				t.Fatal(err)
			}
			defer holder.Close()
			tx, err := holder.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			done := make(chan error, 1)
			go func() {
				switch operation {
				case "status":
					done <- d.UpdateRunStatus(run.ID, types.RunFailed)
				case "error":
					done <- d.UpdateRunError(run.ID, "lock regression")
				case "error-status":
					done <- d.UpdateRunErrorStatus(run.ID, "lock regression", types.RunFailed)
				case "verified-error":
					done <- d.UpdateRunErrorStatusWithVerifiedHead(run.ID, "lock regression", types.RunFailed, "verified")
				case "verified-status":
					done <- d.UpdateRunStatusWithVerifiedHead(run.ID, types.RunFailed, "verified")
				case "step-failure":
					done <- d.FailStep(step.ID, "lock regression", 123)
				case "pr-state":
					done <- d.UpdateRunPRState(run.ID, "merged")
				}
			}()
			select {
			case err := <-done:
				t.Fatalf("write returned before lock release: %v", err)
			case <-time.After(100 * time.Millisecond):
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("write did not recover from busy timeout: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("write did not finish after lock release")
			}
			got, err := d.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			switch operation {
			case "step-failure":
				gotStep, err := d.GetStepResult(step.ID)
				if err != nil || gotStep.Status != types.StepStatusFailed || gotStep.Error == nil || *gotStep.Error != "lock regression" || gotStep.DurationMS == nil || *gotStep.DurationMS != 123 {
					t.Fatalf("step = %+v, %v", gotStep, err)
				}
			case "pr-state":
				if got.PRState == nil || *got.PRState != "merged" || got.Status != types.RunCompleted {
					t.Fatalf("terminal PR state did not finalize run: %+v", got)
				}
				gotStep, err := d.GetStepResult(step.ID)
				if err != nil || gotStep.Status != types.StepStatusCompleted {
					t.Fatalf("terminal PR state did not finalize CI: %+v, %v", gotStep, err)
				}
			default:
				if got.Status != types.RunFailed {
					t.Fatalf("status = %s", got.Status)
				}
				if operation == "error" || operation == "error-status" || operation == "verified-error" {
					if got.Error == nil || *got.Error != "lock regression" {
						t.Fatalf("error = %v", got.Error)
					}
				}
				if operation == "verified-error" || operation == "verified-status" {
					if got.HeadSHA != "verified" || got.TerminalHeadVerifiedAt == nil {
						t.Fatalf("verified head was not persisted: %+v", got)
					}
				}
			}
		})
	}
}
