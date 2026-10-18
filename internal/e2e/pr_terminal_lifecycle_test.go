//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestClosedPRPublishedFixSyncJourney drives isolated executable evidence, not
// an installed consumer: real CLI/daemon/Git/SQLite, with only the agent and
// forge replaced. The CI monitor observes closure through the fake forge; the
// test never rewrites recorded run authority or manufactures archive bindings.
func TestClosedPRPublishedFixSyncJourney(t *testing.T) {
	for _, scenario := range []string{"clean", "dirty", "diverged", "equivalent_diverged", "remote_advanced", "merged"} {
		t.Run(scenario, func(t *testing.T) {
			closedPRPublishedFixSyncJourney(t, scenario)
		})
	}
}

func closedPRPublishedFixSyncJourney(t *testing.T, scenario string) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: branchSyncScenario(t), AutoFix: map[types.StepName]int{types.StepReview: 1}})
	ctx := context.Background()
	mustGit := func(dir string, args ...string) string {
		t.Helper()
		out, err := h.runGit(ctx, dir, args...)
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	const branch = "feature/closed-sync"
	const repo = "test/closed-sync"
	remoteURL := "https://github.com/" + repo + ".git"
	configureGitURLRewrite(t, h, remoteURL, h.UpstreamDir)
	mustGit(h.WorkDir, "remote", "set-url", "origin", remoteURL)
	stateDir := t.TempDir()
	forge := existingPRForgeState{
		PRRepo: repo, PRNumber: "42", BaseRef: "main", SourceRepo: repo,
		SourceRef: branch, SourceDir: h.UpstreamDir, State: "open",
		BodyFile: filepath.Join(stateDir, "body.md"), TitleFile: filepath.Join(stateDir, "title.txt"),
		CreatedPR: "https://github.com/" + repo + "/pull/42",
	}
	forgePath := filepath.Join(stateDir, "forge.json")
	writeForge := func(state string) {
		t.Helper()
		forge.State = state
		forge.ViewState = state
		forge.Merged = state == "merged"
		if forge.Merged {
			forge.State = "closed"
		}
		data, err := json.Marshal(forge)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(forgePath+".tmp", data, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(forgePath+".tmp", forgePath); err != nil {
			t.Fatal(err)
		}
	}
	writeForge("open")
	t.Setenv("FAKEAGENT_GH_MODE", "existing-pr")
	t.Setenv("FAKEAGENT_GH_PR_CONFIG", forgePath)
	t.Setenv("FAKEAGENT_GH_LOG", filepath.Join(stateDir, "gh.log"))
	h.CommitChange("init-closed-sync", "seed.txt", "seed\n", "seed")
	initWorktree := h.AddWorktree("init-closed-sync")
	if out, err := h.RunInDir(initWorktree, "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	submitted := h.CommitChange(branch, "feature.txt", "unsafe\n", "add feature")
	operator := h.AddWorktree(branch)
	out, err := h.RunInDir(operator, "axi", "run", "--intent", "preserve the feature and published pipeline fixes")
	if err != nil || !strings.Contains(out, "sync-1") {
		t.Fatalf("review: %v\n%s", err, out)
	}
	if out, err = h.RunInDir(operator, "axi", "respond", "--action", "fix", "--finding", "sync-1"); err != nil {
		t.Fatalf("fix: %v\n%s", err, out)
	}
	if out, err = h.RunInDir(operator, "axi", "respond", "--action", "approve"); err != nil || !strings.Contains(out, "outcome: checks-passed") {
		t.Fatalf("publish: %v\n%s", err, out)
	}
	t.Logf("isolated binary %s published; fake forge checks passed:\n%s", h.NMBin, out)
	lifecycle := "closed"
	if scenario == "merged" {
		lifecycle = "merged"
	}
	writeForge(lifecycle)
	run := h.WaitForRun(branch, 45*time.Second)
	pushed := h.UpstreamBranchSHA(branch)
	if run.Status != types.RunCompleted || pushed == submitted || run.HeadSHA != pushed {
		t.Fatalf("fixture did not publish fixes: %#v", run)
	}
	database, err := db.Open(paths.WithRoot(h.NMHome).DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	before, err := database.GetRun(run.ID)
	if err != nil || before.PRState == nil || *before.PRState != lifecycle || before.CustodyReturnedAt != nil {
		t.Fatalf("normal CI lifecycle observation missing: %#v, %v", before, err)
	}
	if got := strings.TrimSpace(h.WorktreeRefSHA(branch)); got != submitted {
		t.Fatal("caller moved before explicit sync")
	}
	wantSafety := ""
	switch scenario {
	case "dirty":
		if err := os.WriteFile(filepath.Join(operator, "untracked.txt"), []byte("keep dirty work\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		wantSafety = "blocked_dirty"
	case "diverged":
		if err := os.WriteFile(filepath.Join(operator, "local.txt"), []byte("keep local work\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		mustGit(operator, "add", "local.txt")
		mustGit(operator, "commit", "-m", "independent local work")
		wantSafety = "blocked_diverged"
	case "equivalent_diverged":
		if err := os.WriteFile(filepath.Join(operator, "feature.txt"), []byte("safe\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		mustGit(operator, "commit", "-am", "independent equivalent fix")
		if mustGit(operator, "rev-parse", "HEAD^{tree}") != mustGit(h.UpstreamDir, "rev-parse", pushed+"^{tree}") {
			t.Fatal("fixture trees are not equivalent")
		}
		wantSafety = "blocked_diverged"
	case "remote_advanced":
		peer := filepath.Join(stateDir, "peer")
		mustGit(stateDir, "clone", "--branch", branch, h.UpstreamDir, peer)
		mustGit(peer, "config", "user.name", "Peer")
		mustGit(peer, "config", "user.email", "peer@example.com")
		mustGit(peer, "commit", "--allow-empty", "-m", "independent remote addition")
		mustGit(peer, "push", "origin", branch)
		wantSafety = "blocked_remote_advanced"
	case "merged":
		wantSafety = "blocked_merged"
	}
	localBefore := mustGit(operator, "rev-parse", "HEAD")
	remoteBefore := h.UpstreamBranchSHA(branch)
	statusBefore := mustGit(operator, "status", "--porcelain=v1", "--untracked-files=all")
	// Refresh may populate its private fetch ref. Refusal must preserve local
	// branches, not prevent the observation cache from being populated.
	refsBefore := mustGit(operator, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads")
	if scenario == "clean" {
		for _, mode := range []string{"--recover", "--release"} {
			out, err := h.RunInDir(operator, "axi", "sync", mode)
			t.Logf("intact binding %s (error %v):\n%s", mode, err, out)
			want := "blocked_" + strings.TrimPrefix(mode, "--") + "_not_applicable"
			if err == nil || !strings.Contains(out, want) || strings.Contains(out, "changed: true") || strings.Contains(out, "recovered: true") || strings.Contains(out, "released: true") {
				t.Fatalf("%s bypassed intact published binding: %v\n%s", mode, err, out)
			}
			if got := mustGit(operator, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads"); got != refsBefore {
				t.Fatalf("%s changed branch refs", mode)
			}
		}
	}
	out, err = h.RunInDir(operator, "axi", "sync", "--check")
	t.Logf("sync --check (error %v):\n%s", err, out)
	if wantSafety == "" {
		if err != nil || !strings.Contains(out, "safe_fast_forward") || !strings.Contains(out, "pr_state: closed") {
			t.Fatalf("closed check: %v\n%s", err, out)
		}
	} else if err == nil || !strings.Contains(out, wantSafety) {
		t.Fatalf("unsafe check must refuse %s: %v\n%s", scenario, err, out)
	}
	if mustGit(operator, "rev-parse", "HEAD") != localBefore {
		t.Fatal("check moved caller HEAD")
	}
	out, err = h.RunInDir(operator, "axi", "sync")
	t.Logf("sync (error %v):\n%s", err, out)
	if wantSafety == "" {
		if err != nil || !strings.Contains(out, "state: synchronized") || !strings.Contains(out, "changed: true") {
			t.Fatalf("closed sync: %v\n%s", err, out)
		}
		if mustGit(operator, "rev-parse", "HEAD") != pushed || mustGit(operator, "show", "HEAD:feature.txt") != "safe" {
			t.Fatal("caller did not adopt the exact published fix")
		}
		out, err = h.RunInDir(operator, "axi", "sync")
		t.Logf("idempotent sync (error %v):\n%s", err, out)
		if err != nil || !strings.Contains(out, "changed: false") || !strings.Contains(out, "already_synchronized") {
			t.Fatalf("idempotent sync: %v\n%s", err, out)
		}
	} else {
		if err == nil || !strings.Contains(out, wantSafety) || strings.Contains(out, "changed: true") {
			t.Fatalf("unsafe sync must refuse %s: %v\n%s", scenario, err, out)
		}
		if mustGit(operator, "rev-parse", "HEAD") != localBefore || mustGit(operator, "status", "--porcelain=v1", "--untracked-files=all") != statusBefore {
			t.Fatal("refusal changed local history or dirty work")
		}
		if got := mustGit(operator, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads"); got != refsBefore {
			t.Fatal("refusal changed branch refs")
		}
	}
	mustGit(operator, "merge-base", "--is-ancestor", submitted, "HEAD")
	mustGit(h.UpstreamDir, "merge-base", "--is-ancestor", submitted, pushed)
	if scenario == "dirty" {
		data, err := os.ReadFile(filepath.Join(operator, "untracked.txt"))
		if err != nil || string(data) != "keep dirty work\n" {
			t.Fatalf("dirty work changed: %q, %v", data, err)
		}
	}
	if h.UpstreamBranchSHA(branch) != remoteBefore {
		t.Fatal("sync moved upstream")
	}
	after, err := database.GetRun(run.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("sync changed run authority: before=%#v after=%#v, %v", before, after, err)
	}
	runs, err := database.GetRunsByRepo(h.repoID())
	if err != nil || len(runs) != 1 {
		t.Fatalf("sync started another run: %#v, %v", runs, err)
	}
	t.Logf("preserved submitted=%s published=%s local=%s remote=%s; run %s remains %s/%s without custody override", submitted, pushed, mustGit(operator, "rev-parse", "HEAD"), remoteBefore, run.ID, after.Status, *after.PRState)
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
