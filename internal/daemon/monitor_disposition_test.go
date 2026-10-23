package daemon

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestFrozenMonitorOwnerReplayRecoveryAndRetention(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	repo, err := d.InsertRepo(t.TempDir(), "https://github.com/example/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("a", 40)
	run, err := d.InsertRun(repo.ID, "frozen", head, head)
	if err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(d.UpdateRunStatus(run.ID, types.RunRunning))
	must(d.UpdateRunPRURL(run.ID, "https://github.com/example/repo/pull/1"))
	must(d.UpdateRunPRState(run.ID, "open"))
	must(d.UpdateRunPublication(run.ID, db.PushBinding{HeadSHA: head}))
	must(d.SetRunAwaitingAgent(run.ID))
	for _, name := range types.AllSteps() {
		sr, err := d.InsertStepResult(run.ID, name)
		must(err)
		state := types.StepStatusCompleted
		if name == types.StepReview {
			state = types.StepStatusAwaitingApproval
		} else if name.Order() > types.StepReview.Order() {
			state = types.StepStatusPending
		}
		must(d.UpdateStepStatus(sr.ID, state))
	}
	pin := db.MonitorDisposition{RunID: run.ID, RepoID: repo.ID, PRURL: "https://github.com/example/repo/pull/1", PublishedHead: head, ObservedHead: head, State: "parked-review"}
	mgr := NewRunManager(d, p, nil)
	if err := mgr.HandleStopMonitor(context.Background(), pin); err == nil {
		t.Fatal("ownerless nonfrozen run falsely acknowledged")
	}
	// Simulate the durable write previously acknowledged by the executor; the
	// daemon replay path must not start a replacement executor after restart.
	must(d.FreezeMonitor(pin))
	before, _ := d.GetRun(run.ID)
	srv := ipc.NewServer()
	registerHandlers(srv, mgr, d, func() {})
	// Use a short socket path independently of the database's temp path.
	sockRoot, err := os.MkdirTemp("", "nm-freeze-")
	must(err)
	defer os.RemoveAll(sockRoot)
	socket := filepath.Join(sockRoot, "s")
	must(srv.Listen(socket))
	exited := make(chan error, 1)
	go func() { exited <- srv.ServeReady() }()
	defer func() {
		srv.Close()
		select {
		case <-exited:
		case <-time.After(time.Second):
			t.Error("server outlived test")
		}
	}()
	client, err := ipc.Dial(socket)
	must(err)
	defer client.Close()
	var result ipc.StopMonitorResult
	must(client.Call(ipc.MethodStopMonitor, &pin, &result))
	if !result.MonitorStopped {
		t.Fatal("replay unacknowledged")
	}
	if err := mgr.HandleCancel(run.ID); err == nil {
		t.Fatal("frozen run aborted")
	}
	if plans := mgr.recoverableParkedRuns(context.Background()); len(plans) != 0 {
		t.Fatal("frozen run selected for resume")
	}
	wt := p.WorktreeDir(repo.ID, run.ID)
	must(os.MkdirAll(wt, 0755))
	file := filepath.Join(wt, "evidence")
	must(os.WriteFile(file, []byte("keep"), 0600))
	mgr.removeRunWorktree(repo.ID, run.ID, p.RepoDir(repo.ID), wt, "test")
	mgr.cleanupRunEvidence(&config.Config{}, run.ID)
	if _, err := os.Stat(file); err != nil {
		t.Fatal("frozen worktree lost", err)
	}
	must(d.SetRunCIReady(run.ID, true))
	current, _ := d.GetRun(run.ID)
	info := runToInfo(d, current, nil)
	if !info.MonitorStopped || info.CIReady {
		t.Fatal("status advertises frozen monitor as live readiness")
	}
	must(d.SetRunCIReady(run.ID, false)) // restore fixture-only readiness before exact evidence comparison
	// Startup must retain the unfinished row and all gate state.
	_, err = d.RecoverStaleRuns("restart")
	must(err)
	after, _ := d.GetRun(run.ID)
	// The readiness toggles above are fixture operations; compare all other run evidence.
	before.UpdatedAt = after.UpdatedAt
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("replay/recovery changed evidence: before=%+v after=%+v", before, after)
	}
}
