package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
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
	_ = filepath.Join
}
