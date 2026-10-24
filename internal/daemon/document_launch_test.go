package daemon

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestDocumentLaunchImportsWithoutPushPinsTimeoutAndRefusesRepin(t *testing.T) {
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return []pipeline.Step{&mockPassStep{name: types.StepDocument}} })
	repo, _ := setupTestGitRepo(t, p, d, "document-launch")
	gitCmd(t, repo.WorkingPath, "checkout", "-b", "feature")
	gitCmd(t, repo.WorkingPath, "commit", "--allow-empty", "-m", "source")
	head := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	request := ipc.StartDocumentRunParams{StartHistoryRunParams: ipc.StartHistoryRunParams{StartFreshRunParams: ipc.StartFreshRunParams{RepoID: repo.ID, Branch: "feature", HeadSHA: head, Intent: "validate all work", LaunchNonce: "document-nonce", ValidationGeneration: "document-v1"}, WorkDir: repo.WorkingPath}, DocumentTimeout: "90m"}
	var result ipc.StartFreshRunResult
	for _, bad := range []string{"", "0s", "-1m", "unlimited", "999999999999999999h"} {
		request.DocumentTimeout = bad
		if err := client.Call(ipc.MethodStartDocumentRun, &request, &result); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	runs, err := d.GetRunsByRepo(repo.ID)
	if err != nil || len(runs) != 0 {
		t.Fatalf("invalid request created run: %v %v", runs, err)
	}
	previous, err := d.InsertRun(repo.ID, "feature", head, head)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(previous.ID, types.RunFailed); err != nil {
		t.Fatal(err)
	}
	if err := d.SetRunCustodyReturned(previous.ID); err != nil {
		t.Fatal(err)
	}
	request.DocumentTimeout = "90m"
	if err := client.Call(ipc.MethodStartDocumentRun, &request, &result); err != nil {
		t.Fatal(err)
	}
	run := waitForRunTerminalState(t, d, result.Receipt.RunID)
	if run.Status != types.RunCompleted || run.HeadSHA != head {
		t.Fatalf("run=%+v", run)
	}
	timeout, err := d.DocumentTimeout(run.ID)
	if err != nil || timeout != 90*time.Minute {
		t.Fatalf("pin=%s %v", timeout, err)
	}
	if refs, err := git.Run(context.Background(), p.RepoDir(repo.ID), "for-each-ref", "--format=%(refname)", "refs/heads/feature"); err != nil || refs != "" {
		t.Fatalf("gate push occurred: %q %v", refs, err)
	}
	if got := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD"); got != head {
		t.Fatal("caller moved")
	}
	first := result.Receipt.RunID
	if err := client.Call(ipc.MethodStartDocumentRun, &request, &result); err != nil || result.Receipt.RunID != first {
		t.Fatalf("replay: %+v %v", result, err)
	}
	request.DocumentTimeout = "2h"
	if err := client.Call(ipc.MethodStartDocumentRun, &request, &result); err == nil || !strings.Contains(err.Error(), "different Document timeout") {
		t.Fatalf("repin: %v", err)
	}
}

func TestDocumentLaunchRefusesOwnedBranchWithoutCreatingRun(t *testing.T) {
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return []pipeline.Step{&mockPassStep{name: types.StepDocument}} })
	repo, base := setupTestGitRepo(t, p, d, "document-owned")
	gitCmd(t, repo.WorkingPath, "checkout", "-b", "feature")
	gitCmd(t, repo.WorkingPath, "commit", "--allow-empty", "-m", "source")
	head := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	owned, err := d.InsertRun(repo.ID, "feature", head, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(owned.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	request := ipc.StartDocumentRunParams{StartHistoryRunParams: ipc.StartHistoryRunParams{StartFreshRunParams: ipc.StartFreshRunParams{RepoID: repo.ID, Branch: "feature", HeadSHA: head, Intent: "same task", LaunchNonce: "document-owned", ValidationGeneration: "v1"}, WorkDir: repo.WorkingPath}, DocumentTimeout: "90m"}
	var result ipc.StartFreshRunResult
	if err := client.Call(ipc.MethodStartDocumentRun, &request, &result); err == nil {
		t.Fatal("owned branch accepted")
	}
	runs, err := d.GetRunsByRepo(repo.ID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("refusal created run: %v %v", runs, err)
	}
}

func TestDocumentLaunchAcceptsVerifiedUnmovedHeadWithPreservedDirtyDocument(t *testing.T) {
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return []pipeline.Step{&mockPassStep{name: types.StepDocument}} })
	repo, head := setupTestGitRepo(t, p, d, "document-released")
	gitCmd(t, repo.WorkingPath, "checkout", "-b", "feature")
	gitCmd(t, p.RepoDir(repo.ID), "update-ref", "refs/heads/feature", head)
	previous, err := d.InsertRun(repo.ID, "feature", head, head)
	if err != nil {
		t.Fatal(err)
	}
	intent := "validate all work"
	if err := d.UpdateRunIntent(previous.ID, db.RunIntent{Summary: intent, Source: db.RunIntentSourceAgent}); err != nil {
		t.Fatal(err)
	}
	dir := p.WorktreeDir(repo.ID, previous.ID)
	if err := git.WorktreeAdd(context.Background(), p.RepoDir(repo.ID), dir, head); err != nil {
		t.Fatal(err)
	}
	staged := []byte("staged Document edits\n")
	files := map[string][]byte{"test.txt": []byte("unfinished Document edits\n"), "partial.bin": {'d', 'o', 'c', 0, 255, '\n'}}
	if err := os.WriteFile(filepath.Join(dir, "test.txt"), staged, 0o600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "test.txt")
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	step, err := d.InsertStepResult(previous.ID, types.StepDocument)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.FailStep(step.ID, "agent timed out after 10m; recent output observed", 600000); err != nil {
		t.Fatal(err)
	}
	if got, verified := preserveRunHead(d, dir, previous); !verified || got != head {
		t.Fatalf("terminal head = %q, verified = %v", got, verified)
	}
	if err := d.UpdateRunStatusWithVerifiedHead(previous.ID, types.RunFailed, head); err != nil {
		t.Fatal(err)
	}
	m := NewRunManager(d, p, nil)
	m.removeRunWorktree(repo.ID, previous.ID, p.RepoDir(repo.ID), dir, "run_finished")
	cleanupOrphanWorktrees(d, p, nil)
	preserved := t.TempDir()
	for name, want := range files {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(data) != string(want) {
			t.Fatalf("retained %s = %q, %v", name, data, err)
		}
		if err := os.WriteFile(filepath.Join(preserved, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	index := gitOutput(t, dir, "write-tree")
	stagedBytes, err := git.RunRaw(context.Background(), dir, "show", ":test.txt")
	if err != nil || string(stagedBytes) != string(staged) {
		t.Fatalf("retained staged bytes = %q, %v", stagedBytes, err)
	}
	if err := os.WriteFile(filepath.Join(preserved, "staged.txt"), stagedBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	svc := branchsync.Service{DB: d, Repo: repo, WorkDir: repo.WorkingPath, GateDir: p.RepoDir(repo.ID), Paths: p}
	if state := svc.InspectCached(context.Background()); state.State != branchsync.StateUserOwned {
		t.Fatalf("unchanged terminal head not released: %+v", state)
	}
	if state := svc.Recover(context.Background(), false); state.State != branchsync.StateUserOwned || !state.Recovered || state.Changed {
		t.Fatalf("supported recovery = %+v", state)
	}
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	request := ipc.StartDocumentRunParams{StartHistoryRunParams: ipc.StartHistoryRunParams{StartFreshRunParams: ipc.StartFreshRunParams{RepoID: repo.ID, Branch: "feature", HeadSHA: head, Intent: intent, LaunchNonce: "released-document", ValidationGeneration: "v1"}, WorkDir: repo.WorkingPath}, DocumentTimeout: "90m"}
	var result ipc.StartFreshRunResult
	if err := client.Call(ipc.MethodStartDocumentRun, &request, &result); err != nil {
		t.Fatal(err)
	}
	run := waitForRunTerminalState(t, d, result.Receipt.RunID)
	if run.ID == previous.ID || run.Status != types.RunCompleted || run.HeadSHA != head || run.Intent == nil || *run.Intent != intent {
		t.Fatalf("continuation = %+v", run)
	}
	if timeout, err := d.DocumentTimeout(run.ID); err != nil || timeout != 90*time.Minute {
		t.Fatalf("continuation timeout = %s, %v", timeout, err)
	}
	previous, err = d.GetRun(previous.ID)
	if err != nil || previous == nil || previous.CustodyReturnedAt != nil || previous.TerminalHeadVerifiedAt == nil {
		t.Fatalf("released run evidence = %+v, %v", previous, err)
	}
	for name := range files {
		want, err := os.ReadFile(filepath.Join(preserved, name))
		if err != nil {
			t.Fatal(err)
		}
		if got, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(got) != string(want) {
			t.Fatalf("continuation changed retained %s: %q, %v", name, got, err)
		}
	}
	if got := gitOutput(t, dir, "write-tree"); got != index {
		t.Fatal("continuation changed retained index")
	}
	if got := gitOutput(t, dir, "rev-parse", "HEAD"); got != head {
		t.Fatal("continuation changed retained head")
	}
}

func TestDocumentLaunchReconcilesRecoveredRebase(t *testing.T) {
	for _, retained := range []bool{true, false} {
		t.Run(map[bool]string{true: "retained", false: "lost-content"}[retained], func(t *testing.T) {
			p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return []pipeline.Step{&mockPassStep{name: types.StepDocument}} })
			repo, base := setupTestGitRepo(t, p, d, "rebased-document")
			ctx := context.Background()
			gateDir := p.RepoDir(repo.ID)
			gitCmd(t, repo.WorkingPath, "checkout", "-b", "feature")
			if err := os.WriteFile(filepath.Join(repo.WorkingPath, "feature.txt"), []byte("operator work\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, repo.WorkingPath, "add", "feature.txt")
			gitCmd(t, repo.WorkingPath, "commit", "-m", "operator work")
			submitted := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
			gitCmd(t, gateDir, "fetch", repo.WorkingPath, submitted+":refs/heads/feature")
			gitCmd(t, repo.WorkingPath, "checkout", "-b", "advanced", base)
			if err := os.WriteFile(filepath.Join(repo.WorkingPath, "upstream.txt"), []byte("upstream advance\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, repo.WorkingPath, "add", "upstream.txt")
			gitCmd(t, repo.WorkingPath, "commit", "-m", "upstream advance")
			if retained {
				gitCmd(t, repo.WorkingPath, "cherry-pick", submitted)
			}
			head := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
			previous, err := d.InsertRun(repo.ID, "feature", submitted, base)
			if err != nil {
				t.Fatal(err)
			}
			if err := d.UpdateRunHeadSHA(previous.ID, head); err != nil {
				t.Fatal(err)
			}
			if err := d.UpdateRunStatusWithVerifiedHead(previous.ID, types.RunFailed, head); err != nil {
				t.Fatal(err)
			}
			anchor := "refs/no-mistakes/recover/" + previous.ID
			gitCmd(t, gateDir, "fetch", repo.WorkingPath, head+":"+anchor)
			gitCmd(t, repo.WorkingPath, "checkout", "feature")
			if retained {
				svc := branchsync.Service{DB: d, Repo: repo, WorkDir: repo.WorkingPath, GateDir: gateDir, Paths: p}
				if state := svc.Recover(ctx, false); !state.Recovered || !state.Changed {
					t.Fatalf("adoption = %+v", state)
				}
			} else {
				gitCmd(t, repo.WorkingPath, "checkout", "-B", "feature", head)
				if err := d.SetRunCustodyReturned(previous.ID); err != nil {
					t.Fatal(err)
				}
			}
			if got := gitOutput(t, gateDir, "rev-parse", "refs/heads/feature"); got != submitted {
				t.Fatal("recovery unexpectedly moved gate mirror")
			}
			if _, err := git.Run(ctx, gateDir, "merge-base", "--is-ancestor", submitted, head); err == nil {
				t.Fatal("fixture is not a rewritten history")
			}
			before, err := d.GetRunsByRepo(repo.ID)
			if err != nil {
				t.Fatal(err)
			}
			client, err := ipc.Dial(p.Socket())
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			request := ipc.StartDocumentRunParams{StartHistoryRunParams: ipc.StartHistoryRunParams{StartFreshRunParams: ipc.StartFreshRunParams{RepoID: repo.ID, Branch: "feature", HeadSHA: head, Intent: "same task", LaunchNonce: "rebased-document", ValidationGeneration: "v1"}, WorkDir: repo.WorkingPath}, DocumentTimeout: "90m"}
			var result ipc.StartFreshRunResult
			launchErr := client.Call(ipc.MethodStartDocumentRun, &request, &result)
			if !retained {
				if launchErr == nil || !strings.Contains(launchErr.Error(), "retention") {
					t.Fatalf("lost content accepted: %v", launchErr)
				}
				after, err := d.GetRunsByRepo(repo.ID)
				if err != nil || len(after) != len(before) {
					t.Fatalf("refusal created run: %v, %v", after, err)
				}
				if got := gitOutput(t, gateDir, "rev-parse", "refs/heads/feature"); got != submitted {
					t.Fatal("refusal moved mirror")
				}
			} else {
				if launchErr != nil {
					t.Fatal(launchErr)
				}
				run := waitForRunTerminalState(t, d, result.Receipt.RunID)
				if run.Status != types.RunCompleted || run.HeadSHA != head {
					t.Fatalf("continuation = %+v", run)
				}
				if timeout, err := d.DocumentTimeout(run.ID); err != nil || timeout != 90*time.Minute {
					t.Fatalf("timeout = %s, %v", timeout, err)
				}
				if archived := gitOutput(t, gateDir, "rev-parse", "refs/tags/no-mistakes-abandoned/feature/"+submitted); archived != submitted {
					t.Fatalf("submitted head not archived: %s", archived)
				}
			}
			if got := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD"); got != head {
				t.Fatal("caller head moved")
			}
			if got := gitOutput(t, gateDir, "rev-parse", anchor); got != head {
				t.Fatal("terminal recovery anchor moved")
			}
		})
	}
}

func TestDocumentLaunchRefusesUnresolvedCustodyBeforeImport(t *testing.T) {
	for _, evidence := range []string{"no-runs", "other-branch", "legacy", "legacy-returned", "unverified"} {
		t.Run(evidence, func(t *testing.T) {
			p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return []pipeline.Step{&mockPassStep{name: types.StepDocument}} })
			repo, base := setupTestGitRepo(t, p, d, "unresolved-document")
			gitCmd(t, repo.WorkingPath, "checkout", "-b", "feature")
			gitCmd(t, repo.WorkingPath, "commit", "--allow-empty", "-m", "source")
			head := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
			gitCmd(t, p.RepoDir(repo.ID), "update-ref", "refs/heads/feature", base)
			if evidence != "no-runs" {
				branch := "feature"
				if evidence == "other-branch" {
					branch = "other"
				}
				previous, err := d.InsertRun(repo.ID, branch, head, base)
				if err != nil {
					t.Fatal(err)
				}
				if err := d.UpdateRunStatus(previous.ID, types.RunFailed); err != nil {
					t.Fatal(err)
				}
				if evidence != "unverified" {
					raw, err := sql.Open("sqlite", p.DB())
					if err != nil {
						t.Fatal(err)
					}
					defer raw.Close()
					if _, err := raw.Exec("UPDATE runs SET submitted_head_sha = NULL WHERE id = ?", previous.ID); err != nil {
						t.Fatal(err)
					}
				}
				if evidence == "legacy-returned" {
					if err := d.SetRunCustodyReturned(previous.ID); err != nil {
						t.Fatal(err)
					}
				}
			}
			before, err := d.GetRunsByRepo(repo.ID)
			if err != nil {
				t.Fatal(err)
			}
			client, err := ipc.Dial(p.Socket())
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			request := ipc.StartDocumentRunParams{StartHistoryRunParams: ipc.StartHistoryRunParams{StartFreshRunParams: ipc.StartFreshRunParams{RepoID: repo.ID, Branch: "feature", HeadSHA: head, Intent: "same task", LaunchNonce: "unresolved-document", ValidationGeneration: "v1"}, WorkDir: repo.WorkingPath}, DocumentTimeout: "90m"}
			var result ipc.StartFreshRunResult
			if err := client.Call(ipc.MethodStartDocumentRun, &request, &result); err == nil || !strings.Contains(err.Error(), "custody") {
				t.Fatalf("unresolved custody accepted: %v", err)
			}
			after, err := d.GetRunsByRepo(repo.ID)
			if err != nil || len(after) != len(before) {
				t.Fatalf("refusal created run: %v %v", after, err)
			}
			if got := gitOutput(t, p.RepoDir(repo.ID), "rev-parse", "refs/heads/feature"); got != base {
				t.Fatal("refusal changed gate head")
			}
			if _, err := git.Run(context.Background(), p.RepoDir(repo.ID), "cat-file", "-e", head); err == nil {
				t.Fatal("refusal imported caller head")
			}
		})
	}
}
