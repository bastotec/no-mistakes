package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestHistoryLaunch_RPCImportsExactIntegrationWithoutGatePush(t *testing.T) {
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return []pipeline.Step{&steps.RebaseStep{}} })
	repo, base := setupTestGitRepo(t, p, d, "history-launch")
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")
	gitCmd(t, repo.WorkingPath, "remote", "add", "origin", upstream)
	gitCmd(t, repo.WorkingPath, "push", "origin", "HEAD:refs/heads/main")
	gitCmd(t, p.RepoDir(repo.ID), "remote", "set-url", "origin", upstream)
	gitCmd(t, repo.WorkingPath, "checkout", "-b", "preserved")
	gitCmd(t, repo.WorkingPath, "commit", "--allow-empty", "-m", "preserved work")
	preserved := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	gitCmd(t, repo.WorkingPath, "checkout", "-b", "integration", base)
	gitCmd(t, repo.WorkingPath, "commit", "--allow-empty", "-m", "base advance")
	base = gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	gitCmd(t, repo.WorkingPath, "push", "origin", "HEAD:refs/heads/main")
	gitCmd(t, repo.WorkingPath, "merge", "--no-ff", "preserved", "-m", "explicit integration")
	head := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	request := ipc.StartHistoryRunParams{
		StartFreshRunParams: ipc.StartFreshRunParams{
			RepoID: repo.ID, Branch: "integration", HeadSHA: head, Intent: "preserve both exact histories",
			PRBaseBranch: "main", LaunchNonce: "history-nonce", ValidationGeneration: "generation",
		},
		WorkDir: repo.WorkingPath, PreserveHistoryBaseSHA: base,
	}
	var result ipc.StartFreshRunResult
	if err := client.Call(ipc.MethodStartHistoryRun, &request, &result); err != nil {
		t.Fatal(err)
	}
	run := waitForRunTerminalState(t, d, result.Receipt.RunID)
	if run.Status != types.RunCompleted || run.HeadSHA != head || run.SubmittedHeadSHA == nil || *run.SubmittedHeadSHA != head ||
		run.PreserveHistoryBaseSHA == nil || *run.PreserveHistoryBaseSHA != base || run.PRBaseBranch == nil || *run.PRBaseBranch != "main" {
		t.Fatalf("constrained launch = %+v", run)
	}
	if got := gitOutput(t, repo.WorkingPath, "rev-list", "--parents", "-n", "1", "HEAD"); got != head+" "+base+" "+preserved {
		t.Fatalf("original source history changed: %s", got)
	}
	// Starting validation moves neither the private branch nor upstream.
	if refs, err := git.Run(context.Background(), p.RepoDir(repo.ID), "for-each-ref", "--format=%(refname)", "refs/heads/integration"); err != nil || refs != "" {
		t.Fatalf("launch mutated the gate branch: %s", refs)
	}
	if refs, err := git.Run(context.Background(), upstream, "for-each-ref", "--format=%(refname)", "refs/heads/integration"); err != nil || refs != "" {
		t.Fatalf("launch published early: %s", refs)
	}
	firstID := result.Receipt.RunID
	if err := client.Call(ipc.MethodStartHistoryRun, &request, &result); err != nil || result.Receipt.RunID != firstID {
		t.Fatalf("exact replay did not reuse its run: %+v %v", result, err)
	}
	request.PreserveHistoryBaseSHA = strings.Repeat("c", 40)
	if err := client.Call(ipc.MethodStartHistoryRun, &request, &result); err == nil || !strings.Contains(err.Error(), "conflicting launch") {
		t.Fatalf("receipt replay allowed repinning: %v", err)
	}
}

func TestHistoryLaunch_RejectsBeforeStartingOrChangingRefs(t *testing.T) {
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return []pipeline.Step{&mockPassStep{name: types.StepReview}} })
	repo, base := setupTestGitRepo(t, p, d, "history-refusals")
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")
	gitCmd(t, repo.WorkingPath, "remote", "add", "origin", upstream)
	gitCmd(t, repo.WorkingPath, "push", "origin", "HEAD:refs/heads/main")
	gitCmd(t, repo.WorkingPath, "checkout", "-b", "integration")
	gitCmd(t, repo.WorkingPath, "commit", "--allow-empty", "-m", "source")
	head := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	for _, tc := range []struct {
		name   string
		change func(*ipc.StartHistoryRunParams)
		want   string
	}{
		{"empty-pin", func(p *ipc.StartHistoryRunParams) { p.PreserveHistoryBaseSHA = "" }, "full nonzero"},
		{"moving-ref", func(p *ipc.StartHistoryRunParams) { p.PreserveHistoryBaseSHA = "main" }, "full nonzero"},
		{"missing-base-branch", func(p *ipc.StartHistoryRunParams) { p.PRBaseBranch = "" }, "--base-branch"},
		{"skipped-rebase", func(p *ipc.StartHistoryRunParams) { p.SkipSteps = []types.StepName{types.StepRebase} }, "skipped"},
		{"changed-head", func(p *ipc.StartHistoryRunParams) { p.HeadSHA = base }, "head changed"},
		{"unavailable-base", func(p *ipc.StartHistoryRunParams) { p.PreserveHistoryBaseSHA = strings.Repeat("d", 40) }, "not an ancestor"},
		{"unregistered-worktree", func(p *ipc.StartHistoryRunParams) { p.WorkDir = t.TempDir() }, "worktree"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := ipc.StartHistoryRunParams{
				StartFreshRunParams: ipc.StartFreshRunParams{
					RepoID: repo.ID, Branch: "integration", HeadSHA: head, Intent: "preserve history",
					PRBaseBranch: "main", LaunchNonce: "nonce-" + tc.name, ValidationGeneration: "generation",
				}, WorkDir: repo.WorkingPath, PreserveHistoryBaseSHA: base,
			}
			tc.change(&request)
			var result ipc.StartFreshRunResult
			if err := client.Call(ipc.MethodStartHistoryRun, &request, &result); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
			if run, err := d.GetRunByLaunchNonce(repo.ID, "integration", request.LaunchNonce); err != nil || run != nil {
				t.Fatalf("refusal created a run: %+v %v", run, err)
			}
		})
	}
	// A dirty submission must not be imported even when its committed head matches.
	if err := os.WriteFile(filepath.Join(repo.WorkingPath, "uncommitted.txt"), []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	request := ipc.StartHistoryRunParams{StartFreshRunParams: ipc.StartFreshRunParams{
		RepoID: repo.ID, Branch: "integration", HeadSHA: head, Intent: "preserve", PRBaseBranch: "main", LaunchNonce: "dirty", ValidationGeneration: "generation",
	}, WorkDir: repo.WorkingPath, PreserveHistoryBaseSHA: base}
	var result ipc.StartFreshRunResult
	if err := client.Call(ipc.MethodStartHistoryRun, &request, &result); err == nil || !strings.Contains(err.Error(), "clean") {
		t.Fatalf("dirty source allowed: %v", err)
	}
	if got := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD"); got != head {
		t.Fatal("refusal changed the caller head")
	}
}
