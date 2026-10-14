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

func commitFollowUp2(t *testing.T, repo *db.Repo) string { return commitContent(t, repo, "third") }
func commitFollowUp3(t *testing.T, repo *db.Repo) string { return commitContent(t, repo, "fourth") }

func commitContent(t *testing.T, repo *db.Repo, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo.WorkingPath, "test.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "commit", "-am", content)
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
	// The burst opened the branch, so its zero base carries no rewrite
	// evidence and the replacement keeps the push's own previous head.
	if second.BaseSHA != firstSHA {
		t.Fatalf("second run base = %s, want the push's previous head %s", second.BaseSHA, firstSHA)
	}
	third := commitFollowUp2(t, repo)
	thirdRun := pushReceived(t, p, repo.ID, secondSHA, third)
	fourth := commitFollowUp3(t, repo)
	fourthRun := pushReceived(t, p, repo.ID, third, fourth)
	waitForRunFinal(t, d, thirdRun)
	// A burst over an existing branch keeps the base from before the burst,
	// so a rewrite inside it still reads as one against the published head.
	if got := waitForRunFinal(t, d, fourthRun); got.BaseSHA != secondSHA {
		t.Fatalf("fourth run base = %s, want the burst's base %s", got.BaseSHA, secondSHA)
	}
	if got := review.execCnt.Load(); got != 2 {
		t.Fatalf("review executed %d times, want 2 (only the runs that outlived their quiet windows)", got)
	}
	stats, err := d.GetStats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.CoalescedRuns != 2 || stats.SupersededRuns != 0 {
		t.Fatalf("stats coalesced=%d superseded=%d, want 2 and 0", stats.CoalescedRuns, stats.SupersededRuns)
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
