package daemon

import (
	"encoding/json"
	"fmt"
	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

func TestContinuationLaunchRPCPinsStrictDescendantWithAdvancedMain(t *testing.T) {
	for _, outstandingCustody := range []bool{false, true} {
		t.Run(fmt.Sprintf("outstanding-custody=%v", outstandingCustody), func(t *testing.T) {
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
			var prior *db.Run
			var preserved string
			if outstandingCustody {
				prior, err = d.InsertRun(repo.ID, "feature", published, base)
				if err != nil {
					t.Fatal(err)
				}
				gitCmd(t, repo.WorkingPath, "checkout", "--detach", published)
				if err := os.WriteFile(filepath.Join(repo.WorkingPath, "prior-repair.txt"), []byte("unpublished pipeline repair\n"), 0600); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, repo.WorkingPath, "add", "prior-repair.txt")
				gitCmd(t, repo.WorkingPath, "commit", "-m", "prior pipeline repair")
				preserved = gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
				gitCmd(t, gate, "fetch", "--no-tags", "--", repo.WorkingPath, preserved)
				gitCmd(t, gate, "update-ref", custody.RecoveryRef(prior.ID), preserved)
				gitCmd(t, gate, "update-ref", "refs/heads/feature", published)
				gitCmd(t, repo.WorkingPath, "checkout", "feature")
				if err := d.UpdateRunStatusWithVerifiedHead(prior.ID, types.RunFailed, preserved); err != nil {
					t.Fatal(err)
				}
			}
			client, err := ipc.Dial(p.Socket())
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			req := ipc.StartContinuationRunParams{StartExistingPRRunParams: ipc.StartExistingPRRunParams{RepoID: repo.ID, Branch: "feature", HeadSHA: submitted, Intent: "continue the unpublished correction without rewriting author history", URL: target}, WorkDir: repo.WorkingPath}
			var result ipc.RerunResult
			err = client.Call(ipc.MethodStartContinuationRun, &req, &result)
			if outstandingCustody {
				if err == nil || !strings.Contains(err.Error(), "branch custody is not available") {
					t.Fatalf("continuation bypassed unpublished custody: %v", err)
				}
				stored, err := d.GetRun(prior.ID)
				if err != nil || stored.HeadSHA != preserved || stored.Status != types.RunFailed || stored.CustodyReturnedAt != nil {
					t.Fatalf("prior run custody changed: %+v, %v", stored, err)
				}
				runs, err := d.GetRunsByRepo(repo.ID)
				if err != nil || len(runs) != 1 || calls.Load() != 0 {
					t.Fatalf("refused launch created work: runs=%v calls=%d err=%v", runs, calls.Load(), err)
				}
				if got := gitOutput(t, gate, "rev-parse", "refs/heads/feature"); got != published {
					t.Fatal("refusal moved gate branch")
				}
				if got := gitOutput(t, gate, "rev-parse", custody.RecoveryRef(prior.ID)); got != preserved {
					t.Fatal("refusal lost prior unpublished repair")
				}
				if got := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD"); got != submitted {
					t.Fatal("refusal changed caller head")
				}
				if got := gitOutput(t, fork, "rev-parse", "feature"); got != published {
					t.Fatal("refusal changed source remote")
				}
				return
			}
			if err != nil {
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
		})
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
