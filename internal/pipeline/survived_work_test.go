package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func survivedWorkRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{{"init"}, {"config", "user.name", "Test"}, {"config", "user.email", "test@example.com"}} {
		gitRun(t, dir, args...)
	}
	writeFile(t, dir, "tracked.txt", "base\n")
	writeFile(t, dir, "doomed.txt", "base\n")
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-m", "base")
	return dir
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := git.Run(context.Background(), dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(out)
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// leavePartialRepair leaves what a cut-short repair typically does: one file
// staged at one version and edited further, an untracked new file, and a
// tracked deletion.
func leavePartialRepair(t *testing.T, dir string) {
	t.Helper()
	writeFile(t, dir, "tracked.txt", "staged version\n")
	gitRun(t, dir, "add", "tracked.txt")
	writeFile(t, dir, "tracked.txt", "working version\n")
	writeFile(t, dir, "new.txt", "untracked\n")
	gitRun(t, dir, "rm", "-q", "doomed.txt")
}

func TestCaptureSurvivedWork_MovesTheWorkIntoARefAndRestoresItExactly(t *testing.T) {
	dir := survivedWorkRepo(t)
	if work := CaptureSurvivedWork(dir, "run-1", nil); work != nil {
		t.Fatalf("clean worktree captured: %+v", work)
	}
	leavePartialRepair(t, dir)
	head := gitRun(t, dir, "rev-parse", "HEAD")

	work := CaptureSurvivedWork(dir, "run-1", nil)
	if work == nil || work.InspectionError != "" || !strings.HasPrefix(work.Ref, "refs/no-mistakes/survived/run-1/") || work.BaseSHA != head || len(work.Files) != 3 {
		t.Fatalf("capture = %+v", work)
	}
	if status := gitRun(t, dir, "status", "--porcelain", "--untracked-files=all"); status != "" {
		t.Fatalf("captured work left in the worktree, where a later step could publish it: %q", status)
	}
	if got := gitRun(t, dir, "show", work.Ref+":new.txt"); got != "untracked" {
		t.Fatalf("untracked file not captured: %q", got)
	}

	if err := RestoreSurvivedWork(dir, work); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, dir, "tracked.txt"); got != "working version\n" {
		t.Fatalf("working tree = %q", got)
	}
	if got := gitRun(t, dir, "show", ":tracked.txt"); got != "staged version" {
		t.Fatalf("index = %q, want the staged version back", got)
	}
	if got := readFile(t, dir, "new.txt"); got != "untracked\n" {
		t.Fatalf("untracked file = %q", got)
	}
	if status := gitRun(t, dir, "status", "--porcelain", "--", "new.txt"); status != "?? new.txt" {
		t.Fatalf("untracked file status = %q, want untracked again", status)
	}
	if _, err := os.Stat(filepath.Join(dir, "doomed.txt")); !os.IsNotExist(err) {
		t.Fatalf("deletion not restored: %v", err)
	}
	if status := gitRun(t, dir, "status", "--porcelain", "--", "doomed.txt"); status != "D  doomed.txt" {
		t.Fatalf("deletion status = %q, want staged deletion", status)
	}
}

func TestRestoreSurvivedWork_OnAMovedHeadAppliesTheWorkUnstaged(t *testing.T) {
	dir := survivedWorkRepo(t)
	leavePartialRepair(t, dir)
	work := CaptureSurvivedWork(dir, "run-2", nil)
	writeFile(t, dir, "unrelated.txt", "moved\n")
	gitRun(t, dir, "add", "unrelated.txt")
	gitRun(t, dir, "commit", "-m", "head moved")

	if err := RestoreSurvivedWork(dir, work); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, dir, "tracked.txt"); got != "working version\n" {
		t.Fatalf("working tree = %q", got)
	}
	if got := readFile(t, dir, "new.txt"); got != "untracked\n" {
		t.Fatalf("new file = %q", got)
	}
	if staged := gitRun(t, dir, "diff", "--cached", "--name-only"); staged != "" {
		t.Fatalf("restored onto a moved head but left staged: %q", staged)
	}
}

func TestRestoreSurvivedWork_ConflictLeavesTheWorktreeCleanAndTheRef(t *testing.T) {
	dir := survivedWorkRepo(t)
	leavePartialRepair(t, dir)
	work := CaptureSurvivedWork(dir, "run-3", nil)
	writeFile(t, dir, "tracked.txt", "conflicting head\n")
	gitRun(t, dir, "commit", "-qam", "conflict")

	if err := RestoreSurvivedWork(dir, work); err == nil {
		t.Fatal("conflicting restore reported success")
	}
	if status := gitRun(t, dir, "status", "--porcelain", "--untracked-files=all"); status != "" {
		t.Fatalf("failed restore left the worktree dirty: %q", status)
	}
	if got := gitRun(t, dir, "show", work.Ref+":tracked.txt"); got != "working version" {
		t.Fatalf("ref lost after a failed restore: %q", got)
	}
}

func TestRestoreSurvivedWork_RefusesADirtyWorktree(t *testing.T) {
	dir := survivedWorkRepo(t)
	leavePartialRepair(t, dir)
	work := CaptureSurvivedWork(dir, "run-4", nil)
	writeFile(t, dir, "other.txt", "someone else's change\n")
	if err := RestoreSurvivedWork(dir, work); err == nil {
		t.Fatal("restore mixed captured work into a dirty worktree")
	}
}

func TestCaptureSurvivedWork_UnreadableStatusStaysInPlace(t *testing.T) {
	work := CaptureSurvivedWork(filepath.Join(t.TempDir(), "missing"), "run-5", nil)
	if work == nil || work.InspectionError == "" || work.Ref != "" {
		t.Fatalf("unreadable status treated as clean or captured: %+v", work)
	}
}

func TestSurvivedWorkPrompt(t *testing.T) {
	if SurvivedWorkPrompt(nil) != "" {
		t.Fatal("prompt without restored work")
	}
	prompt := SurvivedWorkPrompt(&types.SurvivedWork{Files: []string{"?? new.txt"}})
	if !strings.Contains(prompt, "new.txt") || !strings.Contains(prompt, "selected findings") || !strings.Contains(prompt, "Do not reset or clean") {
		t.Fatal(prompt)
	}
}

// A repair cut short by its deadline leaves its work captured on the run and
// the worktree clean.
func TestExecutor_TimedOutRepairCapturesSurvivedWork(t *testing.T) {
	for _, timeout := range []error{ErrAgentTimeout, ErrReviewAgentTimeout} {
		t.Run(timeout.Error(), func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			dir := survivedWorkRepo(t)
			step := &adaptiveCallStep{name: types.StepTest, fn: func(sctx *StepContext) (*StepOutcome, error) {
				if !sctx.Fixing {
					return &StepOutcome{NeedsApproval: true, AutoFixable: true, Findings: `{"findings":[{"id":"selected","severity":"error","description":"repair","action":"auto-fix"},{"id":"deferred","severity":"warning","description":"wait","action":"ask-user"}]}`}, nil
				}
				writeFile(t, dir, "progress.txt", "survived")
				return nil, timeout
			}}
			exec := NewExecutor(database, p, &config.Config{AutoFix: config.AutoFix{Test: 1}}, nil, []Step{step}, nil)
			if err := exec.Execute(context.Background(), run, repo, dir); err == nil {
				t.Fatal("timeout passed")
			}
			work, err := database.GetRunSurvivedWork(run.ID)
			if err != nil || work == nil || work.State != types.SurvivedWorkCaptured || work.Ref == "" || len(work.Files) != 1 {
				t.Fatalf("survival missing: %+v %v", work, err)
			}
			if got := gitRun(t, dir, "show", work.Ref+":progress.txt"); got != "survived" {
				t.Fatalf("repair not captured: %q", got)
			}
			if status := gitRun(t, dir, "status", "--porcelain", "--untracked-files=all"); status != "" {
				t.Fatalf("worktree still holds the cut-short work: %q", status)
			}
		})
	}
}

// The next authorized fix round gets the captured work back, is told about
// it, and the evidence is cleared; a step that is not a fix round never sees
// it.
func TestExecutor_NextFixRoundRestoresSurvivedWork(t *testing.T) {
	database, p, run, repo := setupTest(t)
	dir := survivedWorkRepo(t)
	writeFile(t, dir, "partial.txt", "survived\n")
	work := CaptureSurvivedWork(dir, run.ID, recordSurvivedWork(database, run.ID))

	var sawInPlainRound, sawInFixRound bool
	var restored *types.SurvivedWork
	step := &adaptiveCallStep{name: types.StepTest, fn: func(sctx *StepContext) (*StepOutcome, error) {
		_, statErr := os.Stat(filepath.Join(dir, "partial.txt"))
		if !sctx.Fixing {
			sawInPlainRound = statErr == nil
			return &StepOutcome{NeedsApproval: true, AutoFixable: true, Findings: `{"findings":[{"id":"selected","severity":"error","description":"repair","action":"auto-fix"}]}`}, nil
		}
		sawInFixRound = statErr == nil
		restored = sctx.RestoredSurvivedWork
		gitRun(t, dir, "add", "-A")
		gitRun(t, dir, "commit", "-qm", "repair")
		return &StepOutcome{}, nil
	}}
	exec := NewExecutor(database, p, &config.Config{AutoFix: config.AutoFix{Test: 1}}, nil, []Step{step}, nil)
	if err := exec.Execute(context.Background(), run, repo, dir); err != nil {
		t.Fatal(err)
	}
	if sawInPlainRound {
		t.Fatal("a round that is not a repair saw the captured work")
	}
	if !sawInFixRound || restored == nil || restored.Ref != work.Ref {
		t.Fatalf("fix round did not get the work back: saw=%v restored=%+v", sawInFixRound, restored)
	}
	if left, err := database.GetRunSurvivedWork(run.ID); err != nil || left != nil {
		t.Fatalf("evidence not cleared after restore: %+v %v", left, err)
	}
}

// The handoff is recorded before the worktree is touched; if it cannot be,
// the work also stays in the worktree, where cleanup keeps it.
func TestCaptureSurvivedWork_RecordsBeforeCleaningAndKeepsWorkWhenItCannot(t *testing.T) {
	dir := survivedWorkRepo(t)
	leavePartialRepair(t, dir)
	var dirtyWhenRecorded bool
	work := CaptureSurvivedWork(dir, "run-6", func(*types.SurvivedWork) error {
		dirtyWhenRecorded = gitRun(t, dir, "status", "--porcelain", "--untracked-files=all") != ""
		return nil
	})
	if work == nil || work.Ref == "" || !dirtyWhenRecorded {
		t.Fatalf("evidence was not recorded before the worktree was cleaned: %+v recordedDirty=%v", work, dirtyWhenRecorded)
	}

	dir = survivedWorkRepo(t)
	leavePartialRepair(t, dir)
	work = CaptureSurvivedWork(dir, "run-7", func(w *types.SurvivedWork) error {
		if w.Ref != "" {
			return fmt.Errorf("database is locked")
		}
		return nil
	})
	if work == nil || work.Ref != "" || !strings.Contains(work.InspectionError, "could not be recorded") {
		t.Fatalf("unrecorded capture = %+v, want in-place evidence", work)
	}
	if gitRun(t, dir, "status", "--porcelain", "--untracked-files=all") == "" {
		t.Fatal("work removed from the worktree although its handoff was never recorded")
	}
	if !SurvivedWorkHoldsWorktree(work, dir) {
		t.Fatal("cleanup would release a worktree holding unrecorded work")
	}
}

// A fix round that gets restored work back but does not commit it (here it
// parks again without a repair) puts the work back into a ref: the worktree
// is clean for the steps that follow and the handoff is still recorded.
func TestExecutor_RestoredWorkNotCommittedIsCapturedAgain(t *testing.T) {
	database, p, run, repo := setupTest(t)
	dir := survivedWorkRepo(t)
	writeFile(t, dir, "partial.txt", "survived\n")
	CaptureSurvivedWork(dir, run.ID, recordSurvivedWork(database, run.ID))
	// Findings are agent-written: a survived_work key in them is ignored.
	gate := `{"findings":[{"id":"selected","severity":"error","description":"repair","action":"auto-fix"}],"survived_work":{"worktree":"x","ref":"refs/forged","base_sha":"abc","next":"n"}}`
	step := &adaptiveCallStep{name: types.StepTest, fn: func(sctx *StepContext) (*StepOutcome, error) {
		if !sctx.Fixing {
			return &StepOutcome{NeedsApproval: true, AutoFixable: true, Findings: gate}, nil
		}
		return nil, fmt.Errorf("repair agent unavailable")
	}}
	exec := NewExecutor(database, p, &config.Config{AutoFix: config.AutoFix{Test: 1}}, nil, []Step{step}, nil)
	_ = exec.Execute(context.Background(), run, repo, dir)

	if status := gitRun(t, dir, "status", "--porcelain", "--untracked-files=all"); status != "" {
		t.Fatalf("restored work left in the worktree after a round that did not commit it: %q", status)
	}
	left, err := database.GetRunSurvivedWork(run.ID)
	if err != nil || left == nil || left.State != types.SurvivedWorkCaptured || left.Ref == "" || left.Ref == "refs/forged" {
		t.Fatalf("handoff lost: %+v %v", left, err)
	}
	if got := gitRun(t, dir, "show", left.Ref+":partial.txt"); got != "survived" {
		t.Fatalf("recaptured work = %q", got)
	}
}

// A dirty submodule's edits cannot be written as a superproject tree, so
// such work stays in place, where cleanup keeps it, instead of being reported
// as captured.
func TestCaptureSurvivedWork_DirtySubmoduleStaysInPlace(t *testing.T) {
	sub := survivedWorkRepo(t)
	dir := survivedWorkRepo(t)
	gitRun(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "sub")
	gitRun(t, dir, "commit", "-qm", "add submodule")
	writeFile(t, filepath.Join(dir, "sub"), "tracked.txt", "edited inside the submodule\n")

	work := CaptureSurvivedWork(dir, "run-8", nil)
	if work == nil || work.Ref != "" || !strings.Contains(work.InspectionError, "submodule sub") {
		t.Fatalf("dirty submodule reported as captured: %+v", work)
	}
	if got := readFile(t, filepath.Join(dir, "sub"), "tracked.txt"); got != "edited inside the submodule\n" {
		t.Fatalf("submodule edit lost: %q", got)
	}
}

// On daemon shutdown an independent repair may still be editing, so a round
// that restored work leaves the worktree and the handoff alone.
func TestExecutor_ShutdownLeavesRestoredWorkAlone(t *testing.T) {
	database, p, run, repo := setupTest(t)
	dir := survivedWorkRepo(t)
	writeFile(t, dir, "partial.txt", "survived\n")
	work := CaptureSurvivedWork(dir, run.ID, recordSurvivedWork(database, run.ID))
	step := &adaptiveCallStep{name: types.StepTest, fn: func(sctx *StepContext) (*StepOutcome, error) {
		if !sctx.Fixing {
			return &StepOutcome{NeedsApproval: true, AutoFixable: true, Findings: `{"findings":[{"id":"selected","severity":"error","description":"repair","action":"auto-fix"}]}`}, nil
		}
		writeFile(t, dir, "in-flight.txt", "agent still writing\n")
		return nil, ErrDaemonShutdown
	}}
	exec := NewExecutor(database, p, &config.Config{AutoFix: config.AutoFix{Test: 1}}, nil, []Step{step}, nil)
	_ = exec.Execute(context.Background(), run, repo, dir)
	if got := readFile(t, dir, "in-flight.txt"); got != "agent still writing\n" {
		t.Fatalf("shutdown settlement touched the live repair's files: %q", got)
	}
	left, err := database.GetRunSurvivedWork(run.ID)
	if err != nil || left == nil || left.Ref != work.Ref || left.State != types.SurvivedWorkRestored {
		t.Fatalf("handoff changed on shutdown: %+v %v", left, err)
	}
}

func recordSurvivedWork(database *db.DB, runID string) func(*types.SurvivedWork) error {
	return func(work *types.SurvivedWork) error { return database.SetRunSurvivedWork(runID, work) }
}

// A step that settles restored work itself (the CI repair captures again on
// its own timeout) keeps its new handoff: the executor does not clear it.
func TestExecutor_StepRecaptureOfRestoredWorkIsKept(t *testing.T) {
	database, p, run, repo := setupTest(t)
	dir := survivedWorkRepo(t)
	writeFile(t, dir, "partial.txt", "survived\n")
	CaptureSurvivedWork(dir, run.ID, recordSurvivedWork(database, run.ID))
	var recaptured *types.SurvivedWork
	step := &adaptiveCallStep{name: types.StepCI, fn: func(sctx *StepContext) (*StepOutcome, error) {
		if !sctx.Fixing {
			return &StepOutcome{NeedsApproval: true, AutoFixable: true, Findings: `{"findings":[{"id":"selected","severity":"error","description":"repair","action":"auto-fix"}]}`}, nil
		}
		writeFile(t, dir, "more.txt", "more\n")
		recaptured = CaptureSurvivedWork(dir, run.ID, recordSurvivedWork(database, run.ID))
		return &StepOutcome{}, nil
	}}
	exec := NewExecutor(database, p, &config.Config{AutoFix: config.AutoFix{CI: 1}}, nil, []Step{step}, nil)
	_ = exec.Execute(context.Background(), run, repo, dir)
	left, err := database.GetRunSurvivedWork(run.ID)
	if err != nil || recaptured == nil || left == nil || left.Ref != recaptured.Ref {
		t.Fatalf("step recapture was not kept: left=%+v recaptured=%+v %v", left, recaptured, err)
	}
	if got := gitRun(t, dir, "show", left.Ref+":more.txt"); got != "more" {
		t.Fatalf("recaptured work = %q", got)
	}
}

// Restored work a shutdown left in the worktree is captured again before a
// round that is not a repair runs, so nothing but a repair ever sees it.
func TestExecutor_PlainRoundRecapturesWorkAShutdownLeftRestored(t *testing.T) {
	database, p, run, repo := setupTest(t)
	dir := survivedWorkRepo(t)
	writeFile(t, dir, "partial.txt", "survived\n")
	work := CaptureSurvivedWork(dir, run.ID, recordSurvivedWork(database, run.ID))
	if err := RestoreSurvivedWork(dir, work); err != nil {
		t.Fatal(err)
	}
	restored := *work
	restored.State = types.SurvivedWorkRestored
	if err := database.SetRunSurvivedWork(run.ID, &restored); err != nil {
		t.Fatal(err)
	}
	var sawWork bool
	step := &adaptiveCallStep{name: types.StepTest, fn: func(sctx *StepContext) (*StepOutcome, error) {
		_, statErr := os.Stat(filepath.Join(dir, "partial.txt"))
		sawWork = statErr == nil
		return &StepOutcome{}, nil
	}}
	exec := NewExecutor(database, p, &config.Config{}, nil, []Step{step}, nil)
	if err := exec.Execute(context.Background(), run, repo, dir); err != nil {
		t.Fatal(err)
	}
	if sawWork {
		t.Fatal("a round that is not a repair saw restored work")
	}
	left, err := database.GetRunSurvivedWork(run.ID)
	if err != nil || left == nil || left.State != types.SurvivedWorkCaptured {
		t.Fatalf("handoff after recapture = %+v %v", left, err)
	}
	if got := gitRun(t, dir, "show", left.Ref+":partial.txt"); got != "survived" {
		t.Fatalf("recaptured work = %q", got)
	}
}
