package daemon

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A run whose CI step was only watching its open PR when the daemon stopped
// resumes watching after a restart, instead of ending as
// ci_monitor_interrupted: a daemon swap must not cost an open PR its watch.
func TestRecoverResumesInterruptedCIWatch(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "dtest")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })
	p := paths.WithRoot(tmpDir)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	mockClaude := writeMockClaude(t, t.TempDir())
	if err := os.WriteFile(p.ConfigFile(), []byte("agent: claude\npush_quiet_window: \"0s\"\nagent_path_override:\n  claude: "+mockClaude+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	repo, headSHA := setupTestGitRepo(t, p, d, "resume-ci-watch")
	run, err := d.InsertRun(repo.ID, "feature", headSHA, headSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunPRURL(run.ID, "https://github.com/test/repo/pull/42"); err != nil {
		t.Fatal(err)
	}
	worktree := p.WorktreeDir(repo.ID, run.ID)
	if err := gitpkg.WorktreeAdd(context.Background(), p.RepoDir(repo.ID), worktree, headSHA); err != nil {
		t.Fatal(err)
	}
	if err := d.SetRunWorktreeDir(run.ID, worktree); err != nil {
		t.Fatal(err)
	}
	review, err := d.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.CompleteStep(review.ID, 0, 1, ""); err != nil {
		t.Fatal(err)
	}
	ci, err := d.InsertStepResult(run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.StartStep(ci.ID); err != nil {
		t.Fatal(err)
	}

	ciStep := &mockPassStep{name: types.StepCI}
	errCh := make(chan error, 1)
	go func() {
		errCh <- RunWithOptions(p, d, func() []pipeline.Step {
			return []pipeline.Step{&mockPassStep{name: types.StepReview}, ciStep}
		})
	}()
	defer func() {
		client, dialErr := ipc.Dial(p.Socket())
		if dialErr == nil {
			_ = client.Call(ipc.MethodShutdown, &ipc.ShutdownParams{}, nil)
			_ = client.Close()
		}
		select {
		case <-errCh:
		case <-time.After(35 * time.Second):
			t.Error("daemon did not stop")
		}
	}()

	waitForDaemonReady(t, p)
	done := waitForRunTerminalState(t, d, run.ID)
	if done.Status != types.RunCompleted {
		t.Fatalf("resumed CI watch ended %s (%v), want completed", done.Status, done.Error)
	}
	if ciStep.execCnt.Load() != 1 {
		t.Fatalf("CI step ran %d times after the restart, want 1", ciStep.execCnt.Load())
	}
}

// The real CI step, watching an open PR, survives a daemon restart: the
// shutdown leaves the watch resumable rather than ending it, the next daemon
// re-attaches to the same PR, and the run settles when the PR merges.
func TestCIWatchSurvivesDaemonRestartWithRealCIStep(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mock gh is a shell script")
	}
	tmpDir, err := os.MkdirTemp("", "dtest")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })
	p := paths.WithRoot(tmpDir)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	mockClaude := writeMockClaude(t, t.TempDir())
	if err := os.WriteFile(p.ConfigFile(), []byte("agent: claude\npush_quiet_window: \"0s\"\nagent_path_override:\n  claude: "+mockClaude+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	repo, headSHA := setupTestGitRepo(t, p, d, "ci-watch-restart")
	ghDir := t.TempDir()
	stateFile := filepath.Join(ghDir, "state")
	ghLog := filepath.Join(ghDir, "gh.log")
	headFile := filepath.Join(ghDir, "head")
	if err := os.WriteFile(headFile, []byte(headSHA), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stateFile, []byte("OPEN"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
printf '%s\n' "$*" >>` + shellQuoteForTest(ghLog) + `
case "$*" in
  "auth status"*) exit 0 ;;
  "pr view 42 "*headRefOid*) cat ` + shellQuoteForTest(headFile) + `; exit 0 ;;
  "pr view 42 "*mergeable*) printf '%s\n' MERGEABLE; exit 0 ;;
  "api "*actions/runs*) printf '%s\n' '[{"total_count":0,"workflow_runs":[]}]'; exit 0 ;;
  "api "*graphql*) printf '%s\n' '{"data":{"repository":{"object":{"statusCheckRollup":{"contexts":{"nodes":[{"__typename":"CheckRun","databaseId":1,"name":"build","status":"IN_PROGRESS","conclusion":"","startedAt":"2026-10-20T00:00:00Z"}],"pageInfo":{"hasNextPage":false}}}}}}}'; exit 0 ;;
  "pr view 42 "*) cat ` + shellQuoteForTest(stateFile) + `; exit 0 ;;
esac
exit 1
`
	if err := os.WriteFile(filepath.Join(ghDir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", ghDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	run, err := d.InsertRun(repo.ID, "feature", headSHA, headSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunPRURL(run.ID, "https://github.com/test/repo/pull/42"); err != nil {
		t.Fatal(err)
	}
	worktree := p.WorktreeDir(repo.ID, run.ID)
	if err := gitpkg.WorktreeAdd(context.Background(), p.RepoDir(repo.ID), worktree, headSHA); err != nil {
		t.Fatal(err)
	}
	if err := d.SetRunWorktreeDir(run.ID, worktree); err != nil {
		t.Fatal(err)
	}
	ci, err := d.InsertStepResult(run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.StartStep(ci.ID); err != nil {
		t.Fatal(err)
	}

	startDaemon := func() func() {
		errCh := make(chan error, 1)
		go func() {
			errCh <- RunWithOptions(p, d, func() []pipeline.Step {
				return []pipeline.Step{(&steps.CIStep{}).SetPollIntervalOverride(50 * time.Millisecond)}
			})
		}()
		waitForDaemonReady(t, p)
		return func() {
			client, dialErr := ipc.Dial(p.Socket())
			if dialErr == nil {
				_ = client.Call(ipc.MethodShutdown, &ipc.ShutdownParams{}, nil)
				_ = client.Close()
			}
			select {
			case <-errCh:
			case <-time.After(35 * time.Second):
				t.Error("daemon did not stop")
			}
		}
	}
	// waitForWatching waits for several successful checks polls: each poll
	// reads the checks, and none of them may have failed to read.
	waitForWatching := func() {
		t.Helper()
		before, _ := os.ReadFile(ghLog)
		logBefore, _ := os.ReadFile(filepath.Join(p.RunLogDir(run.ID), "ci.log"))
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			data, _ := os.ReadFile(ghLog)
			if strings.Count(string(data[len(before):]), "graphql") >= 3 {
				stepLog, _ := os.ReadFile(filepath.Join(p.RunLogDir(run.ID), "ci.log"))
				if len(stepLog) >= len(logBefore) && strings.Contains(string(stepLog[len(logBefore):]), "could not check") {
					t.Fatalf("CI step could not read its checks:\n%s", stepLog)
				}
				if sr, err := d.GetStepResult(ci.ID); err != nil || sr.Status != types.StepStatusRunning {
					t.Fatalf("CI step is not watching: %v (%v)", sr, err)
				}
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		data, _ := os.ReadFile(ghLog)
		stepLog, _ := os.ReadFile(filepath.Join(p.RunLogDir(run.ID), "ci.log"))
		t.Fatalf("CI step never polled the PR's checks; gh saw:\n%s\nci log:\n%s", data, stepLog)
	}

	stop := startDaemon()
	waitForWatching()
	stop()
	cut, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cut.Status != types.RunRunning {
		t.Fatalf("run after shutdown = %s (%v), want it left running for the next daemon", cut.Status, cut.Error)
	}

	stop = startDaemon()
	defer stop()
	waitForWatching()
	if err := os.WriteFile(stateFile, []byte("MERGED"), 0o644); err != nil {
		t.Fatal(err)
	}
	done := waitForRunTerminalState(t, d, run.ID)
	if done.Status != types.RunCompleted {
		t.Fatalf("resumed CI watch ended %s (%v), want completed after the merge", done.Status, done.Error)
	}
}
