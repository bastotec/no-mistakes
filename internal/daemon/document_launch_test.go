package daemon

import (
	"context"
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
