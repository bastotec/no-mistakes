package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A push that lands while the previous push's run is still waiting out its
// quiet window replaces that run before any step ran, and the replaced run is
// recorded as coalesced rather than superseded.
func TestPushQuietWindowCoalescesAFollowUpPush(t *testing.T) {
	window := 2 * time.Second
	previous := testPushQuietWindow
	testPushQuietWindow = &window
	t.Cleanup(func() { testPushQuietWindow = previous })

	review := &mockPassStep{name: types.StepReview}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{review}
	})
	repo, firstSHA := setupTestGitRepo(t, p, d, "quiet-window-repo")

	if err := os.WriteFile(filepath.Join(repo.WorkingPath, "test.txt"), []byte("follow-up"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "commit", "-am", "follow-up")
	gitCmd(t, repo.WorkingPath, "push", "gate", "HEAD:refs/heads/main")
	secondSHA := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	push := func(old, head string) string {
		t.Helper()
		var result ipc.PushReceivedResult
		if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
			Gate: p.RepoDir("quiet-window-repo"),
			Ref:  "refs/heads/main",
			Old:  old,
			New:  head,
		}, &result); err != nil {
			t.Fatal(err)
		}
		return result.RunID
	}

	firstRun := push("0000000000000000000000000000000000000000", firstSHA)
	secondRun := push(firstSHA, secondSHA)

	first := waitForRunFinal(t, d, firstRun)
	if first.Status != types.RunCancelled || first.Error == nil || *first.Error != types.RunCancelReasonCoalesced {
		t.Fatalf("first run = %s %v, want cancelled with %q", first.Status, first.Error, types.RunCancelReasonCoalesced)
	}
	second := waitForRunTerminalState(t, d, secondRun)
	if second.Status != types.RunCompleted {
		t.Fatalf("second run status = %s, want completed", second.Status)
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
	window := 50 * time.Millisecond
	previous := testPushQuietWindow
	testPushQuietWindow = &window
	t.Cleanup(func() { testPushQuietWindow = previous })

	slow := &mockSlowStep{name: types.StepReview}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{slow}
	})
	repo, firstSHA := setupTestGitRepo(t, p, d, "quiet-window-started-repo")
	if err := os.WriteFile(filepath.Join(repo.WorkingPath, "test.txt"), []byte("follow-up"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "commit", "-am", "follow-up")
	gitCmd(t, repo.WorkingPath, "push", "gate", "HEAD:refs/heads/main")
	secondSHA := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var first ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("quiet-window-started-repo"), Ref: "refs/heads/main",
		Old: "0000000000000000000000000000000000000000", New: firstSHA,
	}, &first); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		run, err := d.GetRun(first.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status == types.RunRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("first run never started, status %s", run.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
	var second ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("quiet-window-started-repo"), Ref: "refs/heads/main",
		Old: firstSHA, New: secondSHA,
	}, &second); err != nil {
		t.Fatal(err)
	}
	cancelled := waitForRunFinal(t, d, first.RunID)
	if cancelled.Error == nil || *cancelled.Error != types.RunCancelReasonSuperseded {
		t.Fatalf("first run error = %v, want %q", cancelled.Error, types.RunCancelReasonSuperseded)
	}
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
