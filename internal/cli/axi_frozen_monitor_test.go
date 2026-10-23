package cli

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
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
	must(database.UpdateRunStatus(r.ID, types.RunRunning))
	must(database.UpdateRunPRURL(r.ID, url))
	must(database.UpdateRunPRState(r.ID, "open"))
	must(database.UpdateRunPublication(r.ID, db.PushBinding{HeadSHA: head}))
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
	must(database.FreezeMonitor(db.MonitorDisposition{RunID: r.ID, RepoID: repo.ID, PRURL: url, PublishedHead: head, ObservedHead: head, State: "parked-review"}))
	for _, selection := range []string{"", r.ID} {
		out := axiStatusOutput(t, selection)
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
