package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// setPushQuietWindow rewrites the test daemon's global config; every launch
// reads it afresh.
func setPushQuietWindow(t *testing.T, p *paths.Paths, window string) {
	t.Helper()
	data, err := os.ReadFile(p.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	config := "push_quiet_window: \"" + window + "\"\n"
	for _, line := range strings.Split(string(data), "\n") {
		if line != "" && !strings.HasPrefix(line, "push_quiet_window:") {
			config += line + "\n"
		}
	}
	if err := os.WriteFile(p.ConfigFile(), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
}

// commitFollowUp adds a second commit to the work repo and pushes it to the
// gate, returning its SHA.
func commitFollowUp(t *testing.T, repo *db.Repo) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo.WorkingPath, "test.txt"), []byte("follow-up"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "commit", "-am", "follow-up")
	gitCmd(t, repo.WorkingPath, "push", "gate", "HEAD:refs/heads/main")
	return gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
}

func pushReceived(t *testing.T, p *paths.Paths, repoID, old, head string) string {
	t.Helper()
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var result ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir(repoID), Ref: "refs/heads/main", Old: old, New: head,
	}, &result); err != nil {
		t.Fatal(err)
	}
	return result.RunID
}

// A push that lands while the previous push's run is still waiting out its
// quiet window replaces that run before any step ran, and the replaced run is
// recorded as coalesced rather than superseded.
func TestPushQuietWindowCoalescesAFollowUpPush(t *testing.T) {
	review := &mockPassStep{name: types.StepReview}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{review}
	})
	setPushQuietWindow(t, p, "2s")
	repo, firstSHA := setupTestGitRepo(t, p, d, "quiet-window-repo")
	secondSHA := commitFollowUp(t, repo)

	firstRun := pushReceived(t, p, repo.ID, refreshTestZeroSHA, firstSHA)
	secondRun := pushReceived(t, p, repo.ID, firstSHA, secondSHA)

	first := waitForRunFinal(t, d, firstRun)
	if first.Status != types.RunCancelled || first.Error == nil || *first.Error != types.RunCancelReasonCoalesced {
		t.Fatalf("first run = %s %v, want cancelled with %q", first.Status, first.Error, types.RunCancelReasonCoalesced)
	}
	second := waitForRunFinal(t, d, secondRun)
	if second.Status != types.RunCompleted {
		t.Fatalf("second run status = %s, want completed", second.Status)
	}
	if second.BaseSHA != firstSHA {
		t.Fatalf("second run base = %s, want its own previous head %s (the replaced push only appended)", second.BaseSHA, firstSHA)
	}
	if got := review.execCnt.Load(); got != 1 {
		t.Fatalf("review executed %d times, want 1 (only the run that outlived its quiet window)", got)
	}
	stats, err := d.GetStats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.CoalescedRuns != 1 || stats.SupersededRuns != 0 {
		t.Fatalf("stats coalesced=%d superseded=%d, want 1 and 0", stats.CoalescedRuns, stats.SupersededRuns)
	}
}

// A push to a run that already started its steps keeps today's semantics: the
// active run is superseded.
func TestPushAfterQuietWindowSupersedesTheStartedRun(t *testing.T) {
	slow := &mockSlowStep{name: types.StepReview}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{slow}
	})
	setPushQuietWindow(t, p, "50ms")
	repo, firstSHA := setupTestGitRepo(t, p, d, "quiet-window-started-repo")
	secondSHA := commitFollowUp(t, repo)

	firstRun := pushReceived(t, p, repo.ID, refreshTestZeroSHA, firstSHA)
	waitForRunStatus(t, d, firstRun, types.RunRunning)
	pushReceived(t, p, repo.ID, firstSHA, secondSHA)
	if cancelled := waitForRunFinal(t, d, firstRun); cancelled.Error == nil || *cancelled.Error != types.RunCancelReasonSuperseded {
		t.Fatalf("first run error = %v, want %q", cancelled.Error, types.RunCancelReasonSuperseded)
	}
}

// Only a push coalesces: a rerun that replaces a run still inside its quiet
// window supersedes it, so stats never count a rerun as an avoided restart.
func TestRerunDuringQuietWindowIsNotCountedAsCoalesced(t *testing.T) {
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepReview}}
	})
	setPushQuietWindow(t, p, "30s")
	repo, firstSHA := setupTestGitRepo(t, p, d, "quiet-window-rerun-repo")
	firstRun := pushReceived(t, p, repo.ID, refreshTestZeroSHA, firstSHA)

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var rerun ipc.RerunResult
	if err := client.Call(ipc.MethodRerun, &ipc.RerunParams{RepoID: repo.ID, Branch: "main"}, &rerun); err != nil {
		t.Fatal(err)
	}
	if first := waitForRunFinal(t, d, firstRun); first.Error == nil || *first.Error != types.RunCancelReasonSuperseded {
		t.Fatalf("first run error = %v, want %q", first.Error, types.RunCancelReasonSuperseded)
	}
}

// A run whose window expires while a launch on its branch is still setting up
// stays in its window until that launch releases the branch lock, so the
// launch replaces it as coalesced instead of the run starting its steps first.
func TestQuietWindowWaitsForAnInFlightLaunchOnItsBranch(t *testing.T) {
	m := NewRunManager(nil, nil, func() []pipeline.Step { return nil })
	m.quieting["run"] = true
	branchMu := m.branchLock("repo", "main")
	branchMu.Lock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.endQuietWindow(ctx, "repo", "main", "run", time.Millisecond)
		close(done)
	}()
	time.Sleep(200 * time.Millisecond)
	m.mu.Lock()
	stillQuiet := m.quieting["run"]
	m.mu.Unlock()
	if !stillQuiet {
		t.Fatal("run left its quiet window while a launch held the branch lock")
	}
	cancel()
	<-done
	branchMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.quieting["run"] {
		t.Fatal("a run cancelled inside its window must still read as quieting")
	}
}

func waitForRunStatus(t *testing.T, d *db.DB, runID string, want types.RunStatus) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		run, err := d.GetRun(runID)
		if err != nil {
			t.Fatal(err)
		}
		if run != nil && run.Status == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("run %s never reached %s", runID, want)
}

// waitForRunFinal is waitForRunTerminalState for any terminal status,
// cancelled included.
func waitForRunFinal(t *testing.T, d *db.DB, runID string) *db.Run {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		run, err := d.GetRun(runID)
		if err != nil {
			t.Fatal(err)
		}
		if run != nil && run.Status.Terminal() {
			return run
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("run %s did not reach a terminal state", runID)
	return nil
}

// A push that replaces a run whose own push rewrote history keeps that
// rewrite's evidence: it takes the replaced run's pre-rewrite base, so the
// rebase step still sees a force push. A push replacing an append-only run
// keeps its own previous head (TestPushQuietWindowCoalescesAFollowUpPush).
func TestPushReplacingARewriteKeepsThePreRewriteBase(t *testing.T) {
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepReview}}
	})
	setPushQuietWindow(t, p, "30s")
	repo, rootSHA := setupTestGitRepo(t, p, d, "quiet-window-rewrite-repo")
	dropped := commitFollowUp(t, repo)

	gitCmd(t, repo.WorkingPath, "reset", "--hard", rootSHA)
	if err := os.WriteFile(filepath.Join(repo.WorkingPath, "other.txt"), []byte("rewrite"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "add", "other.txt")
	gitCmd(t, repo.WorkingPath, "commit", "-m", "rewrite")
	gitCmd(t, repo.WorkingPath, "push", "-f", "gate", "HEAD:refs/heads/main")
	rewritten := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	rewriteRun := pushReceived(t, p, repo.ID, dropped, rewritten)

	if err := os.WriteFile(filepath.Join(repo.WorkingPath, "other.txt"), []byte("follow-up"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "commit", "-am", "follow-up on the rewrite")
	gitCmd(t, repo.WorkingPath, "push", "gate", "HEAD:refs/heads/main")
	followUp := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	followUpRun := pushReceived(t, p, repo.ID, rewritten, followUp)

	waitForRunFinal(t, d, rewriteRun)
	run, err := d.GetRun(followUpRun)
	if err != nil {
		t.Fatal(err)
	}
	if run.BaseSHA != dropped {
		t.Fatalf("follow-up base = %s, want the pre-rewrite head %s", run.BaseSHA, dropped)
	}

	// The evidence keeps propagating for as long as the burst does.
	appended := commitContent(t, repo, "append")
	appendRun := pushReceived(t, p, repo.ID, followUp, appended)
	if run, err = d.GetRun(appendRun); err != nil {
		t.Fatal(err)
	}
	if run.BaseSHA != dropped {
		t.Fatalf("append base = %s, want the pre-rewrite head %s", run.BaseSHA, dropped)
	}
}

func commitContent(t *testing.T, repo *db.Repo, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo.WorkingPath, "test.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "commit", "-am", content)
	gitCmd(t, repo.WorkingPath, "push", "gate", "HEAD:refs/heads/main")
	return gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
}
