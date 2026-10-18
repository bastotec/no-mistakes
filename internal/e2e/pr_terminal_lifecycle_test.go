//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestClosedPRPublishedFixSyncJourney exercises the real CLI, isolated daemon,
// and real Git publication. A normal lifecycle observation retires the PR but
// must not require a fresh validation merely to obtain already-published fixes.
func TestClosedPRPublishedFixSyncJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: branchSyncScenario(t), AutoFix: map[types.StepName]int{types.StepReview: 1}})
	h.CommitChange("init-closed-sync", "seed.txt", "seed\n", "seed")
	initWorktree := h.AddWorktree("init-closed-sync")
	if out, err := h.RunInDir(initWorktree, "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	const branch = "feature/closed-sync"
	submitted := h.CommitChange(branch, "feature.txt", "unsafe\n", "add feature")
	operator := h.AddWorktree(branch)
	out, err := h.RunInDir(operator, "axi", "run", "--intent", "preserve the feature and published pipeline fixes")
	if err != nil || !strings.Contains(out, "sync-1") {
		t.Fatalf("review: %v\n%s", err, out)
	}
	if out, err = h.RunInDir(operator, "axi", "respond", "--action", "fix", "--finding", "sync-1"); err != nil {
		t.Fatalf("fix: %v\n%s", err, out)
	}
	if out, err = h.RunInDir(operator, "axi", "respond", "--action", "approve"); err != nil || !strings.Contains(out, "outcome: passed") {
		t.Fatalf("publish: %v\n%s", err, out)
	}
	run := h.WaitForRun(branch, 10*time.Second)
	pushed := h.UpstreamBranchSHA(branch)
	if run.Status != types.RunCompleted || pushed == submitted || run.HeadSHA != pushed {
		t.Fatalf("fixture did not publish fixes: %#v", run)
	}
	// Use the normal lifecycle persistence API, not a rewritten run status or
	// manufactured archive. No real forge or shared daemon is contacted.
	database, err := db.Open(paths.WithRoot(h.NMHome).DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.UpdateRunPRState(run.ID, "closed"); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(h.WorktreeRefSHA(branch)); got != submitted {
		t.Fatal("caller moved before explicit sync")
	}
	out, err = h.RunInDir(operator, "axi", "sync", "--check")
	if err != nil || !strings.Contains(out, "safe_fast_forward") || !strings.Contains(out, "pr_state: closed") {
		t.Fatalf("closed check: %v\n%s", err, out)
	}
	out, err = h.RunInDir(operator, "axi", "sync")
	if err != nil || !strings.Contains(out, "state: synchronized") || !strings.Contains(out, "changed: true") {
		t.Fatalf("closed sync: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(h.WorktreeRefSHA(branch)); got != pushed {
		t.Fatalf("caller head = %s, want %s", got, pushed)
	}
	if out, err := h.runGit(context.Background(), operator, "merge-base", "--is-ancestor", submitted, pushed); err != nil {
		t.Fatalf("original history lost: %v\n%s", err, out)
	}
	if h.UpstreamBranchSHA(branch) != pushed {
		t.Fatal("sync moved upstream")
	}
	runs, err := database.GetRunsByRepo(h.repoID())
	if err != nil || len(runs) != 1 {
		t.Fatalf("sync started another run: %#v, %v", runs, err)
	}
}

// TestTerminalPRRunDisappearsFromActiveListing reproduces the operator-visible
// defect with a real CLI and an isolated daemon/home. A terminal PR observation
// is the initiating trigger. The normal executor's immediate follow-up status
// write usually masks the defect, so this fixture stops at the durable
// observation boundary where an interruption used to leave status=running.
func TestTerminalPRRunDisappearsFromActiveListing(t *testing.T) {
	for _, state := range []string{"merged", "closed"} {
		t.Run(state, func(t *testing.T) {
			h := NewHarness(t, SetupOpts{Agent: "claude"})
			if out, err := h.Run("init"); err != nil {
				t.Fatalf("init: %v\n%s", err, out)
			}

			p := paths.WithRoot(h.NMHome)
			database, err := db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			run, err := database.InsertRun(h.repoID(), "feature/terminal-pr", "0123456789abcdef", "fedcba9876543210")
			if err != nil {
				_ = database.Close()
				t.Fatal(err)
			}
			if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
				_ = database.Close()
				t.Fatal(err)
			}
			if err := database.UpdateRunPRURL(run.ID, "https://github.com/test/repo/pull/42"); err != nil {
				_ = database.Close()
				t.Fatal(err)
			}
			if err := database.UpdateRunPRState(run.ID, state); err != nil {
				_ = database.Close()
				t.Fatal(err)
			}
			if err := database.Close(); err != nil {
				t.Fatal(err)
			}

			out, err := h.Run("runs")
			if err != nil {
				t.Fatalf("runs: %v\n%s", err, out)
			}
			t.Logf("no-mistakes runs after %s PR observation:\n%s", state, out)
			if !strings.Contains(out, "completed") || strings.Contains(out, "running") {
				t.Fatalf("terminal PR remained visibly active:\n%s", out)
			}

			activeDB, err := db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer activeDB.Close()
			active, err := activeDB.GetActiveRuns()
			if err != nil {
				t.Fatal(err)
			}
			if len(active) != 0 {
				t.Fatalf("authoritative DB still lists terminal PR run as active: %+v", active)
			}
		})
	}
}
