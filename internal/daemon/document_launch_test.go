package daemon

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

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

func TestDocumentLaunchRefusesUnresolvedCustodyBeforeImport(t *testing.T) {
	for _, evidence := range []string{"no-runs", "other-branch", "legacy", "legacy-returned", "user-owned"} {
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
				if evidence == "user-owned" {
					if err := d.RecordRunTerminalHeadEvidence(previous.ID, head); err != nil {
						t.Fatal(err)
					}
				}
				if err := d.UpdateRunStatus(previous.ID, types.RunFailed); err != nil {
					t.Fatal(err)
				}
				if evidence != "user-owned" {
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
