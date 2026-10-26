package cli

import (
	"bytes"
	"context"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/spf13/cobra"
)

func TestFrozenMonitorStatusAndHomePreserveEvidenceWithoutGateGuidance(t *testing.T) {
	repoDir, _, database, repo := setupAxiQueryRepo(t)
	run(t, repoDir, "git", "checkout", "-b", "feature/frozen")
	chdir(t, repoDir)
	head := strings.Repeat("a", 40)
	r, err := database.InsertRun(repo.ID, "feature/frozen", head, head)
	if err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	url := "https://github.com/example/repo/pull/1"
	published := strings.Repeat("b", 40)
	must(database.UpdateRunStatus(r.ID, types.RunRunning))
	must(database.UpdateRunPRURL(r.ID, url))
	must(database.UpdateRunPRState(r.ID, "open"))
	must(database.UpdateRunPublication(r.ID, db.PushBinding{HeadSHA: published}))
	must(database.UpdateRunHeadSHA(r.ID, head))
	must(database.SetRunAwaitingAgent(r.ID))
	for _, name := range types.AllSteps() {
		sr, err := database.InsertStepResult(r.ID, name)
		must(err)
		state := types.StepStatusCompleted
		if name == types.StepReview {
			state = types.StepStatusAwaitingApproval
		} else if name.Order() > types.StepReview.Order() {
			state = types.StepStatusPending
		}
		must(database.UpdateStepStatus(sr.ID, state))
	}
	before, err := database.GetRun(r.ID)
	must(err)
	stepsBefore, err := database.GetStepsByRun(r.ID)
	must(err)
	must(database.FreezeMonitor(db.MonitorDisposition{RunID: r.ID, RepoID: repo.ID, PRURL: url, PublishedHead: published, ObservedHead: head, State: "parked-review"}))
	for _, selection := range []string{"", r.ID} {
		out := axiStatusOutput(t, selection)
		for _, pin := range []string{"repo_id: " + repo.ID, "last_pushed_sha: " + published, "head_sha: " + head} {
			if !strings.Contains(out, pin) {
				t.Fatalf("status lost monitor-stop pin %q: %s", pin, out)
			}
		}
		if !strings.Contains(out, "monitor_stopped: true") || !strings.Contains(out, "validation_finished: false") {
			t.Fatalf("status lost disposition: %s", out)
		}
		if strings.Contains(out, "axi respond") || strings.Contains(out, "\ngate:") {
			t.Fatalf("frozen status offers a gate response: %s", out)
		}
	}
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&out)
	must(runAxiHome(cmd))
	for _, pin := range []string{"repo_id: " + repo.ID, "last_pushed_sha: " + published, "head_sha: " + head} {
		if !strings.Contains(out.String(), pin) {
			t.Fatalf("home lost monitor-stop pin %q: %s", pin, out.String())
		}
	}
	if !strings.Contains(out.String(), "monitor_stopped: true") || strings.Contains(out.String(), "axi respond") || strings.Contains(out.String(), "\ngate:") {
		t.Fatalf("incorrect frozen home: %s", out.String())
	}
	after, err := database.GetRun(r.ID)
	must(err)
	stepsAfter, err := database.GetStepsByRun(r.ID)
	must(err)
	if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(stepsBefore, stepsAfter) {
		t.Fatal("status/home changed preserved evidence")
	}
}

func TestDriveFrozenMonitorReturnsWithoutRespondingOrReadiness(t *testing.T) {
	for _, tc := range []struct {
		name        string
		autoApprove bool
		ciReady     bool
	}{
		{name: "observe"},
		{name: "yes", autoApprove: true},
		{name: "stale-ready", autoApprove: true, ciReady: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ciStatus := types.StepStatusPending
			if tc.ciReady {
				ciStatus = types.StepStatusRunning
			}
			frozen := &ipc.RunInfo{
				ID: "frozen-run", Status: types.RunRunning, MonitorStopped: true, CIReady: tc.ciReady,
				Steps: []ipc.StepResultInfo{
					{StepName: types.StepReview, Status: types.StepStatusAwaitingApproval},
					{StepName: types.StepCI, Status: ciStatus},
				},
			}
			before := *frozen
			source := &scriptedRunStateSource{
				subscriptions: []scriptedSubscription{{events: make(chan ipc.Event)}},
				runs:          []*ipc.RunInfo{frozen},
			}
			reconciler := newRunReconciler(source, frozen.ID)
			defer reconciler.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			got, ready, err := driveRunWithReconciler(ctx, io.Discard, nil, reconciler, frozen.ID, tc.autoApprove)
			if err != nil || got != frozen || ready {
				t.Fatalf("drive frozen run = %+v, ready=%v, err=%v", got, ready, err)
			}
			if !reflect.DeepEqual(before, *got) {
				t.Fatal("driver changed preserved evidence")
			}
			for _, readiness := range []bool{false, true} {
				var out bytes.Buffer
				cmd := &cobra.Command{}
				cmd.SetContext(context.Background())
				cmd.SetOut(&out)
				if err := renderDriveResult(cmd, got, readiness); err != nil {
					t.Fatal(err)
				}
				for _, want := range []string{"outcome: monitor-stopped", "monitor_stopped: true", "validation_finished: false", "custody_returned: false"} {
					if !strings.Contains(out.String(), want) {
						t.Fatalf("drive result lost %q: %s", want, out.String())
					}
				}
				for _, forbidden := range []string{"axi respond", "checks-passed", "outcome: passed", "\ngate:"} {
					if strings.Contains(out.String(), forbidden) {
						t.Fatalf("frozen result offered %q: %s", forbidden, out.String())
					}
				}
			}
		})
	}
}

func TestAxiMonitorStopRefusesManagedWorktreeBeforeRPC(t *testing.T) {
	repoDir, p, _, repo := setupAxiQueryRepo(t)
	run(t, repoDir, "git", "clone", "--bare", repoDir, p.RepoDir(repo.ID))
	wt := p.WorktreeDir(repo.ID, "contained")
	run(t, repoDir, "git", "--git-dir="+p.RepoDir(repo.ID), "worktree", "add", "--detach", wt, "HEAD")
	chdir(t, wt)
	t.Setenv("NO_MISTAKES_GATE", "")
	out, err := executeCmd("axi", "monitor-stop", "--run", "exact-run", "--repo-id", repo.ID, "--pr-url", "https://github.com/example/repo/pull/1", "--published-head", strings.Repeat("a", 40), "--observed-head", strings.Repeat("b", 40), "--state", "parked-review")
	if err == nil || !strings.Contains(out, "nested_gate_context") {
		t.Fatalf("managed worktree reached monitor-stop RPC: err=%v output=%s", err, out)
	}
}
