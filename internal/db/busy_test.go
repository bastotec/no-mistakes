package db

import (
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
