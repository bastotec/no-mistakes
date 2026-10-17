package daemon

import (
	"encoding/json"
	"fmt"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
)

func TestContinuationLaunchRPCPinsStrictDescendantWithAdvancedMain(t *testing.T) {
	bin := t.TempDir()
	name := "gh"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", filepath.Join(bin, name), "../pipeline/fakecli")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fake gh: %v %s", err, out)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_CLI_MODE", "gh")
	const target = "https://github.com/upstream/widgets/pull/168"
	var calls atomic.Int32
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return []pipeline.Step{&existingPRObserveStep{calls: &calls, target: target}} })
	repo, base := setupTestGitRepo(t, p, d, "continuation")
	repo, err := d.UpdateRepoForkURL(repo.ID, "https://github.com/contributor/widgets.git")
	if err != nil {
		t.Fatal(err)
	}
	upstream, fork := t.TempDir(), t.TempDir()
	gitCmd(t, upstream, "init", "--bare")
	gitCmd(t, fork, "init", "--bare")
	gitCmd(t, repo.WorkingPath, "checkout", "-b", "feature")
	gitCmd(t, repo.WorkingPath, "commit", "--allow-empty", "-m", "published author change")
	published := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	gitCmd(t, repo.WorkingPath, "push", fork, "HEAD:refs/heads/feature")
	gitCmd(t, repo.WorkingPath, "commit", "--allow-empty", "-m", "unpublished correction")
	submitted := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	// Advance the actual upstream independently of the source fork.
	gitCmd(t, repo.WorkingPath, "checkout", "--detach", base)
	gitCmd(t, repo.WorkingPath, "commit", "--allow-empty", "-m", "advanced main")
	tip := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	gitCmd(t, repo.WorkingPath, "push", upstream, "HEAD:refs/heads/main")
	gitCmd(t, repo.WorkingPath, "checkout", "feature")
	gate := p.RepoDir(repo.ID)
	gitCmd(t, gate, "config", "url."+upstream+".insteadOf", "https://github.com/upstream/widgets.git")
	gitCmd(t, gate, "config", "url."+fork+".insteadOf", "https://github.com/contributor/widgets.git")
	payload := fmt.Sprintf(`{"number":168,"html_url":%q,"state":"open","base":{"ref":"main","repo":{"full_name":"upstream/widgets","html_url":"https://github.com/upstream/widgets"}},"head":{"ref":"feature","sha":%q,"repo":{"full_name":"contributor/widgets","html_url":"https://github.com/contributor/widgets"}}}`, target, published)
	t.Setenv("FAKE_CLI_EXISTING_PR_JSON", payload)
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	req := ipc.StartContinuationRunParams{StartExistingPRRunParams: ipc.StartExistingPRRunParams{RepoID: repo.ID, Branch: "feature", HeadSHA: submitted, Intent: "continue the unpublished correction without rewriting author history", URL: target}, WorkDir: repo.WorkingPath}
	var result ipc.RerunResult
	if err := client.Call(ipc.MethodStartContinuationRun, &req, &result); err != nil {
		t.Fatal(err)
	}
	run := waitForRunTerminalState(t, d, result.RunID)
	if run.Status != types.RunCompleted || calls.Load() != 1 {
		t.Fatalf("run=%+v calls=%d", run, calls.Load())
	}
	pins, err := run.ContinuationPins()
	if err != nil || pins == nil || pins.PRHead != published || pins.BaseTip != tip || pins.MergeBase != base || *run.SubmittedHeadSHA != submitted {
		t.Fatalf("pins=%+v run=%+v err=%v", pins, run, err)
	}
	if got := gitOutput(t, fork, "rev-parse", "feature"); got != published {
		t.Fatal("launch published instead of importing objects")
	}
	var rerun ipc.RerunResult
	if err := client.Call(ipc.MethodRerun, &ipc.RerunParams{RepoID: repo.ID, Branch: "feature", PreviousRunID: run.ID}, &rerun); err == nil {
		t.Fatal("rerun silently repinned continuation")
	}
	// Old admission remains equal-head-only, not relaxed by the new operation.
	var ordinary ipc.RerunResult
	if err := client.Call(ipc.MethodStartExistingPRRun, &req.StartExistingPRRunParams, &ordinary); err == nil {
		t.Fatal("old existing-pr accepted unpublished descendant")
	}
	// A normal API cannot silently erase an existing run's pins.
	before, _ := json.Marshal(pins)
	loaded, _ := d.GetRun(run.ID)
	after, _ := json.Marshal(mustContinuationPins(t, loaded))
	if string(before) != string(after) {
		t.Fatal("refused ordinary launch changed durable continuation pins")
	}
}

func mustContinuationPins(t *testing.T, r interface {
	ContinuationPins() (*db.ContinuationPins, error)
}) *db.ContinuationPins {
	t.Helper()
	pins, err := r.ContinuationPins()
	if err != nil {
		t.Fatal(err)
	}
	return pins
}
