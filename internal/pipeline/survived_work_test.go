package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	if work, _ := CaptureSurvivedWork(dir, "run-1", nil); work != nil {
		t.Fatalf("clean worktree captured: %+v", work)
	}
	leavePartialRepair(t, dir)
	head := gitRun(t, dir, "rev-parse", "HEAD")

	work, _ := CaptureSurvivedWork(dir, "run-1", nil)
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

func TestRestoreSurvivedWork_OnAMovedHeadRestoresBothLayers(t *testing.T) {
	dir := survivedWorkRepo(t)
	leavePartialRepair(t, dir)
	work, _ := CaptureSurvivedWork(dir, "run-2", nil)
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
	if staged := gitRun(t, dir, "show", ":tracked.txt"); staged != "staged version" {
		t.Fatalf("staged layer = %q, want the staged version", staged)
	}
	if staged := gitRun(t, dir, "diff", "--cached", "--name-only"); staged != "doomed.txt\ntracked.txt" {
		t.Fatalf("staged paths = %q", staged)
	}
}

// Staged-only work (staged, then removed from the working tree) survives a
// moved-HEAD restore too.
func TestRestoreSurvivedWork_OnAMovedHeadKeepsStagedOnlyWork(t *testing.T) {
	dir := survivedWorkRepo(t)
	writeFile(t, dir, "staged-only.txt", "only in the index\n")
	gitRun(t, dir, "add", "staged-only.txt")
	if err := os.Remove(filepath.Join(dir, "staged-only.txt")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "tracked.txt", "edited\n")
	work, _ := CaptureSurvivedWork(dir, "run-14", nil)
	writeFile(t, dir, "unrelated.txt", "moved\n")
	gitRun(t, dir, "add", "unrelated.txt")
	gitRun(t, dir, "commit", "-qm", "head moved")
	if err := RestoreSurvivedWork(dir, work); err != nil {
		t.Fatal(err)
	}
	if staged := gitRun(t, dir, "show", ":staged-only.txt"); staged != "only in the index" {
		t.Fatalf("staged-only file = %q", staged)
	}
}

func TestRestoreSurvivedWork_ConflictLeavesTheWorktreeCleanAndTheRef(t *testing.T) {
	dir := survivedWorkRepo(t)
	leavePartialRepair(t, dir)
	work, _ := CaptureSurvivedWork(dir, "run-3", nil)
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
	work, _ := CaptureSurvivedWork(dir, "run-4", nil)
	writeFile(t, dir, "other.txt", "someone else's change\n")
	if err := RestoreSurvivedWork(dir, work); err == nil {
		t.Fatal("restore mixed captured work into a dirty worktree")
	}
}

func TestCaptureSurvivedWork_UnreadableStatusStaysInPlace(t *testing.T) {
	work, _ := CaptureSurvivedWork(filepath.Join(t.TempDir(), "missing"), "run-5", nil)
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
	work, _ := CaptureSurvivedWork(dir, run.ID, recordSurvivedWork(database, run.ID))

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
	var states []types.SurvivedWorkState
	var dirtyWhenRecorded bool
	work, _ := CaptureSurvivedWork(dir, "run-6", func(w *types.SurvivedWork) error {
		if len(states) == 0 {
			dirtyWhenRecorded = gitRun(t, dir, "status", "--porcelain", "--untracked-files=all") != ""
		}
		states = append(states, w.State)
		return nil
	})
	// First in place, while the worktree still holds the work; captured
	// only once the worktree is verified clean.
	if len(states) != 2 || states[0] != types.SurvivedWorkInPlace || states[1] != types.SurvivedWorkCaptured {
		t.Fatalf("recorded states = %v, want in_place then captured", states)
	}
	if work == nil || work.Ref == "" || !dirtyWhenRecorded {
		t.Fatalf("evidence was not recorded before the worktree was cleaned: %+v recordedDirty=%v", work, dirtyWhenRecorded)
	}

	dir = survivedWorkRepo(t)
	leavePartialRepair(t, dir)
	work, _ = CaptureSurvivedWork(dir, "run-7", func(w *types.SurvivedWork) error {
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
	_, _ = CaptureSurvivedWork(dir, run.ID, recordSurvivedWork(database, run.ID))
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

	work, _ := CaptureSurvivedWork(dir, "run-8", nil)
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
	work, _ := CaptureSurvivedWork(dir, run.ID, recordSurvivedWork(database, run.ID))
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
	_, _ = CaptureSurvivedWork(dir, run.ID, recordSurvivedWork(database, run.ID))
	var recaptured *types.SurvivedWork
	step := &adaptiveCallStep{name: types.StepCI, fn: func(sctx *StepContext) (*StepOutcome, error) {
		if !sctx.Fixing {
			return &StepOutcome{NeedsApproval: true, AutoFixable: true, Findings: `{"findings":[{"id":"selected","severity":"error","description":"repair","action":"auto-fix"}]}`}, nil
		}
		writeFile(t, dir, "more.txt", "more\n")
		recaptured, _ = CaptureSurvivedWork(dir, run.ID, recordSurvivedWork(database, run.ID))
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
	work, _ := CaptureSurvivedWork(dir, run.ID, recordSurvivedWork(database, run.ID))
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

// Only the latest capture is kept: a replaced capture's ref is deleted, and
// so is the last one once a repair committed the work.
func TestSurvivedWorkRecorderReleasesSupersededRefs(t *testing.T) {
	database, _, run, _ := setupTest(t)
	dir := survivedWorkRepo(t)
	record := SurvivedWorkRecorder(database, run.ID, dir)
	writeFile(t, dir, "first.txt", "one\n")
	first, _ := CaptureSurvivedWork(dir, run.ID, record)
	writeFile(t, dir, "second.txt", "two\n")
	second, _ := CaptureSurvivedWork(dir, run.ID, record)
	if first == nil || second == nil || first.Ref == second.Ref {
		t.Fatalf("captures = %+v / %+v", first, second)
	}
	if _, err := git.Run(context.Background(), dir, "rev-parse", "--verify", "-q", first.Ref); err == nil {
		t.Fatal("superseded capture ref kept")
	}
	if err := ClearSurvivedWork(database, run.ID, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := git.Run(context.Background(), dir, "rev-parse", "--verify", "-q", second.Ref); err == nil {
		t.Fatal("committed capture ref kept")
	}
}

// In-place work whose handoff cannot be recorded fails loudly and marks the
// worktree, so cleanup keeps it without the row.
func TestCaptureSurvivedWork_UnrecordableInPlaceWorkMarksTheWorktree(t *testing.T) {
	dir := survivedWorkRepo(t)
	writeFile(t, dir, "partial.txt", "survived\n")
	failing := func(*types.SurvivedWork) error { return fmt.Errorf("disk full") }
	work, err := CaptureSurvivedWork(dir, "run-9", failing)
	if work == nil || work.State != types.SurvivedWorkInPlace || err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("capture = %+v, %v; want in-place work and the record error", work, err)
	}
	if !SurvivedWorkMarked(dir) {
		t.Fatal("unrecorded in-place work left no marker")
	}
	if err := os.Remove(filepath.Join(dir, "partial.txt")); err != nil {
		t.Fatal(err)
	}
	if SurvivedWorkMarked(dir) {
		t.Fatal("marker held a clean worktree")
	}
}

// A cancelled round (shutdown or abort, as the CI repair reports it) never
// settles restored work: a dispatched repair may still be editing.
func TestExecutor_CancelledRoundLeavesRestoredWorkAlone(t *testing.T) {
	database, p, run, repo := setupTest(t)
	dir := survivedWorkRepo(t)
	writeFile(t, dir, "partial.txt", "survived\n")
	work, _ := CaptureSurvivedWork(dir, run.ID, recordSurvivedWork(database, run.ID))
	step := &adaptiveCallStep{name: types.StepCI, fn: func(sctx *StepContext) (*StepOutcome, error) {
		if !sctx.Fixing {
			return &StepOutcome{NeedsApproval: true, AutoFixable: true, Findings: `{"findings":[{"id":"selected","severity":"error","description":"repair","action":"auto-fix"}]}`}, nil
		}
		writeFile(t, dir, "in-flight.txt", "agent still writing\n")
		return nil, context.Canceled
	}}
	exec := NewExecutor(database, p, &config.Config{AutoFix: config.AutoFix{CI: 1}}, nil, []Step{step}, nil)
	_ = exec.Execute(context.Background(), run, repo, dir)
	if got := readFile(t, dir, "in-flight.txt"); got != "agent still writing\n" {
		t.Fatalf("cancelled round's settlement touched the live repair's files: %q", got)
	}
	left, err := database.GetRunSurvivedWork(run.ID)
	if err != nil || left == nil || left.Ref != work.Ref || left.State != types.SurvivedWorkRestored {
		t.Fatalf("handoff changed on cancel: %+v %v", left, err)
	}
}

// A submodule configured to hide its dirty state still counts: its edits are
// kept in place rather than read as a clean worktree.
func TestCaptureSurvivedWork_IgnoredDirtySubmoduleStillCounts(t *testing.T) {
	sub := survivedWorkRepo(t)
	dir := survivedWorkRepo(t)
	gitRun(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "sub")
	gitRun(t, dir, "commit", "-qm", "add submodule")
	gitRun(t, dir, "config", "submodule.sub.ignore", "dirty")
	writeFile(t, filepath.Join(dir, "sub"), "tracked.txt", "edited inside the submodule\n")
	work, _ := CaptureSurvivedWork(dir, "run-10", nil)
	if work == nil || work.State != types.SurvivedWorkInPlace {
		t.Fatalf("ignored dirty submodule read as clean: %+v", work)
	}
}

// A restore that fails stops the round: running without the captured work
// would let a later capture replace it.
func TestExecutor_FailedRestoreStopsTheRound(t *testing.T) {
	database, p, run, repo := setupTest(t)
	dir := survivedWorkRepo(t)
	writeFile(t, dir, "partial.txt", "survived\n")
	work, _ := CaptureSurvivedWork(dir, run.ID, recordSurvivedWork(database, run.ID))
	ran := false
	step := &adaptiveCallStep{name: types.StepTest, fn: func(sctx *StepContext) (*StepOutcome, error) {
		if !sctx.Fixing {
			// Something else dirtied the worktree, so the restore refuses.
			writeFile(t, dir, "other.txt", "x\n")
			return &StepOutcome{NeedsApproval: true, AutoFixable: true, Findings: `{"findings":[{"id":"selected","severity":"error","description":"repair","action":"auto-fix"}]}`}, nil
		}
		ran = true
		return &StepOutcome{}, nil
	}}
	exec := NewExecutor(database, p, &config.Config{AutoFix: config.AutoFix{Test: 1}}, nil, []Step{step}, nil)
	if err := exec.Execute(context.Background(), run, repo, dir); err == nil || !strings.Contains(err.Error(), "could not restore interrupted repair work") {
		t.Fatalf("Execute() = %v, want the restore failure", err)
	}
	if ran {
		t.Fatal("the repair ran without the captured work")
	}
	if sr := stepResultByName(t, database, run.ID, types.StepTest); sr.Status != types.StepStatusFailed {
		t.Fatalf("step status = %s, want failed", sr.Status)
	}
	left, err := database.GetRunSurvivedWork(run.ID)
	if err != nil || left == nil || left.Ref != work.Ref || left.State != types.SurvivedWorkCaptured {
		t.Fatalf("handoff after failed restore = %+v %v", left, err)
	}
}

// Work split between its ref and a worktree that could not be cleaned is
// never continued in part.
func TestExecutor_SplitInPlaceWorkStopsTheRound(t *testing.T) {
	database, p, run, repo := setupTest(t)
	dir := survivedWorkRepo(t)
	writeFile(t, dir, "partial.txt", "survived\n")
	work, _ := CaptureSurvivedWork(dir, run.ID, recordSurvivedWork(database, run.ID))
	split := *work
	split.State = types.SurvivedWorkInPlace
	if err := database.SetRunSurvivedWork(run.ID, &split); err != nil {
		t.Fatal(err)
	}
	step := &adaptiveCallStep{name: types.StepTest, fn: func(sctx *StepContext) (*StepOutcome, error) {
		if !sctx.Fixing {
			writeFile(t, dir, "leftover.txt", "not cleaned\n")
			return &StepOutcome{NeedsApproval: true, AutoFixable: true, Findings: `{"findings":[{"id":"selected","severity":"error","description":"repair","action":"auto-fix"}]}`}, nil
		}
		t.Fatal("the repair ran on half of the work")
		return nil, nil
	}}
	exec := NewExecutor(database, p, &config.Config{AutoFix: config.AutoFix{Test: 1}}, nil, []Step{step}, nil)
	if err := exec.Execute(context.Background(), run, repo, dir); err == nil || !strings.Contains(err.Error(), "split between") {
		t.Fatalf("Execute() = %v, want the split-work refusal", err)
	}
}

// git clean leaves a nested repository behind and the snapshot holds only
// its gitlink, so such work is kept in place, not reported as captured.
func TestCaptureSurvivedWork_NestedRepositoryStaysInPlace(t *testing.T) {
	dir := survivedWorkRepo(t)
	nested := filepath.Join(dir, "fixture")
	gitRun(t, dir, "init", "-q", nested)
	writeFile(t, nested, "edited.txt", "committed\n")
	gitRun(t, nested, "add", "-A")
	gitRun(t, nested, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "fixture")
	writeFile(t, nested, "edited.txt", "inside a nested repo\n")
	work, _ := CaptureSurvivedWork(dir, "run-11", nil)
	if work == nil || work.State != types.SurvivedWorkInPlace || work.Ref != "" || !strings.Contains(work.InspectionError, "nested repository fixture") {
		t.Fatalf("nested repository reported as captured: %+v", work)
	}
	if got := readFile(t, nested, "edited.txt"); got != "inside a nested repo\n" {
		t.Fatalf("nested edit lost: %q", got)
	}
}

// A submodule path with a space is matched exactly.
func TestCaptureSurvivedWork_DirtySubmoduleWithSpaceInPathStaysInPlace(t *testing.T) {
	sub := survivedWorkRepo(t)
	dir := survivedWorkRepo(t)
	gitRun(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "my lib")
	gitRun(t, dir, "commit", "-qm", "add submodule")
	writeFile(t, filepath.Join(dir, "my lib"), "tracked.txt", "edited inside the submodule\n")
	work, _ := CaptureSurvivedWork(dir, "run-12", nil)
	if work == nil || work.Ref != "" || !strings.Contains(work.InspectionError, "submodule my lib") {
		t.Fatalf("dirty submodule with a space reported as captured: %+v", work)
	}
}

// The moved-HEAD restore ignores diff.noprefix: the patch it builds and
// applies always carries a/ b/ prefixes, so a nested path lands where it was.
func TestRestoreSurvivedWork_MovedHeadIgnoresNoprefixConfig(t *testing.T) {
	dir := survivedWorkRepo(t)
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "src/config.json", "base\n")
	writeFile(t, dir, "config.json", "base\n")
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-qm", "two configs")
	writeFile(t, dir, "src/config.json", "edited\n")
	work, _ := CaptureSurvivedWork(dir, "run-13", nil)
	writeFile(t, dir, "unrelated.txt", "moves HEAD\n")
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-qm", "move head")
	gitRun(t, dir, "config", "diff.noprefix", "true")
	if err := RestoreSurvivedWork(dir, work); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, dir, "src/config.json"); got != "edited\n" {
		t.Fatalf("src/config.json = %q, want the restored edit", got)
	}
	if got := readFile(t, dir, "config.json"); got != "base\n" {
		t.Fatalf("config.json = %q, the edit landed on the wrong file", got)
	}
}

// Unstaged-only work merges three-way onto a moved HEAD whose change sits in
// the patch's context.
func TestRestoreSurvivedWork_OnAMovedHeadMergesUnstagedWork(t *testing.T) {
	dir := survivedWorkRepo(t)
	writeFile(t, dir, "lines.txt", "1\n2\n3\n4\n5\n6\n7\n")
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-qm", "lines")
	writeFile(t, dir, "lines.txt", "1\n2\n3\n4\nfive\n6\n7\n")
	work, _ := CaptureSurvivedWork(dir, "run-15", nil)
	writeFile(t, dir, "lines.txt", "1\n2\nthree\n4\n5\n6\n7\n")
	gitRun(t, dir, "commit", "-qam", "head moved")
	if err := RestoreSurvivedWork(dir, work); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, dir, "lines.txt"); got != "1\n2\nthree\n4\nfive\n6\n7\n" {
		t.Fatalf("merged = %q", got)
	}
	if staged := gitRun(t, dir, "diff", "--cached", "--name-only"); staged != "" {
		t.Fatalf("unstaged work came back staged: %q", staged)
	}
}

// Test's own deadline is a repair timeout too.
func TestExecutor_TestAgentTimeoutCapturesSurvivedWork(t *testing.T) {
	database, p, run, repo := setupTest(t)
	dir := survivedWorkRepo(t)
	step := &adaptiveCallStep{name: types.StepTest, fn: func(sctx *StepContext) (*StepOutcome, error) {
		if !sctx.Fixing {
			return &StepOutcome{NeedsApproval: true, AutoFixable: true, Findings: `{"findings":[{"id":"selected","severity":"error","description":"repair","action":"auto-fix"}]}`}, nil
		}
		writeFile(t, dir, "progress.txt", "survived")
		return nil, fmt.Errorf("fix: %w", ErrTestAgentTimeout)
	}}
	exec := NewExecutor(database, p, &config.Config{AutoFix: config.AutoFix{Test: 1}}, nil, []Step{step}, nil)
	_ = exec.Execute(context.Background(), run, repo, dir)
	work, err := database.GetRunSurvivedWork(run.ID)
	if err != nil || work == nil || work.State != types.SurvivedWorkCaptured {
		t.Fatalf("Test timeout left no handoff: %+v %v", work, err)
	}
}

// Rebase's fix round never gets captured work back: its resolver needs a
// clean worktree.
func TestExecutor_RebaseFixRoundDoesNotRestore(t *testing.T) {
	database, p, run, repo := setupTest(t)
	dir := survivedWorkRepo(t)
	writeFile(t, dir, "partial.txt", "survived\n")
	_, _ = CaptureSurvivedWork(dir, run.ID, recordSurvivedWork(database, run.ID))
	var sawWork bool
	step := &adaptiveCallStep{name: types.StepRebase, fn: func(sctx *StepContext) (*StepOutcome, error) {
		if !sctx.Fixing {
			return &StepOutcome{NeedsApproval: true, AutoFixable: true, Findings: `{"findings":[{"id":"conflict","severity":"error","description":"conflict","action":"auto-fix"}]}`}, nil
		}
		_, statErr := os.Stat(filepath.Join(dir, "partial.txt"))
		sawWork = statErr == nil
		return &StepOutcome{}, nil
	}}
	exec := NewExecutor(database, p, &config.Config{AutoFix: config.AutoFix{Rebase: 1}}, nil, []Step{step}, nil)
	_ = exec.Execute(context.Background(), run, repo, dir)
	if sawWork {
		t.Fatal("rebase fix round got captured work back")
	}
	if left, _ := database.GetRunSurvivedWork(run.ID); left == nil || left.State != types.SurvivedWorkCaptured {
		t.Fatalf("handoff after rebase fix = %+v", left)
	}
}

// A file the repair un-ignored is removed with the capture, even though
// HEAD's ignore rules hide it again after the reset.
func TestCaptureSurvivedWork_RemovesFilesTheRepairUnignored(t *testing.T) {
	dir := survivedWorkRepo(t)
	writeFile(t, dir, ".gitignore", ".env\n")
	gitRun(t, dir, "add", ".gitignore")
	gitRun(t, dir, "commit", "-qm", "ignore env")
	writeFile(t, dir, ".gitignore", "")
	writeFile(t, dir, ".env", "SECRET=interrupted\n")
	work, _ := CaptureSurvivedWork(dir, "run-16", nil)
	if work == nil || work.State != types.SurvivedWorkCaptured {
		t.Fatalf("capture = %+v", work)
	}
	if _, err := os.Stat(filepath.Join(dir, ".env")); !os.IsNotExist(err) {
		t.Fatalf(".env left in the worktree: %v", err)
	}
	if got := gitRun(t, dir, "show", work.Ref+":.env"); got != "SECRET=interrupted" {
		t.Fatalf("captured .env = %q", got)
	}
}

// Only steps whose fix round runs a repair agent get captured work back: a
// Rebase resolver needs a clean worktree, and a Push or PR retry would
// publish the work with no agent continuing it.
func TestRestoresSurvivedWorkOnlyForRepairSteps(t *testing.T) {
	for step, want := range map[types.StepName]bool{
		types.StepReview: true, types.StepTest: true, types.StepDocument: true, types.StepLint: true, types.StepCI: true,
		types.StepRebase: false, types.StepPush: false, types.StepPR: false, types.StepIntent: false,
	} {
		if got := restoresSurvivedWork(step); got != want {
			t.Errorf("restoresSurvivedWork(%s) = %v, want %v", step, got, want)
		}
	}
}

// A nested repository the repair un-ignored is caught before the reset
// restores the rule that would hide it.
func TestCaptureSurvivedWork_UnignoredNestedRepositoryStaysInPlace(t *testing.T) {
	dir := survivedWorkRepo(t)
	writeFile(t, dir, ".gitignore", "fixture/\n")
	gitRun(t, dir, "add", ".gitignore")
	gitRun(t, dir, "commit", "-qm", "ignore fixture")
	writeFile(t, dir, ".gitignore", "")
	nested := filepath.Join(dir, "fixture")
	gitRun(t, dir, "init", "-q", nested)
	writeFile(t, nested, "edited.txt", "committed\n")
	gitRun(t, nested, "add", "-A")
	gitRun(t, nested, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "fixture")
	writeFile(t, nested, "edited.txt", "inside a nested repo\n")
	work, _ := CaptureSurvivedWork(dir, "run-17", nil)
	if work == nil || work.State != types.SurvivedWorkInPlace {
		t.Fatalf("un-ignored nested repository reported as captured: %+v", work)
	}
	if got := readFile(t, nested, "edited.txt"); got != "inside a nested repo\n" {
		t.Fatalf("nested edit lost: %q", got)
	}
}

// A Rebase resolver timeout is not a repair: it captures nothing and leaves
// an inherited handoff as it was.
func TestExecutor_RebaseTimeoutKeepsTheHandoff(t *testing.T) {
	database, p, run, repo := setupTest(t)
	dir := survivedWorkRepo(t)
	writeFile(t, dir, "partial.txt", "survived\n")
	work, _ := CaptureSurvivedWork(dir, run.ID, recordSurvivedWork(database, run.ID))
	step := &adaptiveCallStep{name: types.StepRebase, fn: func(sctx *StepContext) (*StepOutcome, error) {
		if !sctx.Fixing {
			return &StepOutcome{NeedsApproval: true, AutoFixable: true, Findings: `{"findings":[{"id":"conflict","severity":"error","description":"conflict","action":"auto-fix"}]}`}, nil
		}
		writeFile(t, dir, "mergetool.orig", "backup\n")
		return nil, ErrAgentTimeout
	}}
	exec := NewExecutor(database, p, &config.Config{AutoFix: config.AutoFix{Rebase: 1}}, nil, []Step{step}, nil)
	_ = exec.Execute(context.Background(), run, repo, dir)
	left, err := database.GetRunSurvivedWork(run.ID)
	if err != nil || left == nil || left.Ref != work.Ref || left.State != types.SurvivedWorkCaptured {
		t.Fatalf("rebase timeout changed the handoff: %+v %v", left, err)
	}
}

// A nested repository whose path is quoted in plain porcelain (a space) is
// still caught.
func TestNestedRepositoryReadsNULDelimitedPaths(t *testing.T) {
	if got := nestedRepository("?? fixture repo/\x00?? a.txt\x00"); got != "fixture repo" {
		t.Fatalf("nestedRepository = %q", got)
	}
}

// A rename's source field is skipped, never read as its own entry.
func TestPorcelainZPathsSkipsRenameSources(t *testing.T) {
	got := porcelainZPaths("R  moved.sqlite\x00?? link/state.sqlite\x00?? real.txt\x00")
	if len(got) != 2 || got[0][1] != "moved.sqlite" || got[1][1] != "real.txt" {
		t.Fatalf("entries = %v", got)
	}
}

// A file the repair ignored is not in the snapshot; once the reset restores
// HEAD's rules it shows up, and the work stays in place instead of the file
// being cleaned away.
func TestCaptureSurvivedWork_KeepsFilesTheRepairIgnored(t *testing.T) {
	dir := survivedWorkRepo(t)
	writeFile(t, dir, ".gitignore", "scratch.txt\n")
	writeFile(t, dir, "scratch.txt", "not in the snapshot\n")
	work, _ := CaptureSurvivedWork(dir, "run-18", nil)
	if work == nil || work.State != types.SurvivedWorkInPlace || work.Ref == "" {
		t.Fatalf("capture = %+v, want in place with its ref", work)
	}
	if got := readFile(t, dir, "scratch.txt"); got != "not in the snapshot\n" {
		t.Fatalf("scratch.txt = %q", got)
	}
}

// A protected-path refusal leaves the index and files for the operator:
// restored work is not recaptured out from under it.
func TestExecutor_ProtectedPathRefusalLeavesRestoredWork(t *testing.T) {
	database, p, run, repo := setupTest(t)
	dir := survivedWorkRepo(t)
	writeFile(t, dir, "package.lock", "locked\n")
	work, _ := CaptureSurvivedWork(dir, run.ID, recordSurvivedWork(database, run.ID))
	step := &adaptiveCallStep{name: types.StepTest, fn: func(sctx *StepContext) (*StepOutcome, error) {
		if !sctx.Fixing {
			return &StepOutcome{NeedsApproval: true, AutoFixable: true, Findings: `{"findings":[{"id":"selected","severity":"error","description":"repair","action":"auto-fix"}]}`}, nil
		}
		return nil, &ProtectedPathError{Path: "package.lock", Rule: "*.lock"}
	}}
	exec := NewExecutor(database, p, &config.Config{AutoFix: config.AutoFix{Test: 1}}, nil, []Step{step}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = exec.Execute(ctx, run, repo, dir)
	if got := readFile(t, dir, "package.lock"); got != "locked\n" {
		t.Fatalf("refused file taken from the worktree: %q", got)
	}
	left, err := database.GetRunSurvivedWork(run.ID)
	if err != nil || left == nil || left.Ref != work.Ref || left.State != types.SurvivedWorkRestored {
		t.Fatalf("handoff after refusal = %+v %v", left, err)
	}
}

// A round that is not a repair never runs over interrupted work still in
// the worktree.
func TestExecutor_PlainRoundRefusesWorkStillInTheWorktree(t *testing.T) {
	database, p, run, repo := setupTest(t)
	dir := survivedWorkRepo(t)
	writeFile(t, dir, "partial.txt", "survived\n")
	if err := database.SetRunSurvivedWork(run.ID, &types.SurvivedWork{State: types.SurvivedWorkInPlace, Worktree: dir, Next: "recover by hand"}); err != nil {
		t.Fatal(err)
	}
	ran := false
	step := &adaptiveCallStep{name: types.StepDocument, fn: func(sctx *StepContext) (*StepOutcome, error) {
		ran = true
		return &StepOutcome{}, nil
	}}
	exec := NewExecutor(database, p, &config.Config{}, nil, []Step{step}, nil)
	if err := exec.Execute(context.Background(), run, repo, dir); err == nil || !strings.Contains(err.Error(), "only a repair may continue it") {
		t.Fatalf("Execute() = %v, want the refusal", err)
	}
	if ran {
		t.Fatal("an ordinary round ran over interrupted work")
	}
}

// CI reports a protected-path refusal as an outcome, not an error; it is
// left alone all the same.
func TestExecutor_RefusalOutcomeLeavesRestoredWork(t *testing.T) {
	database, p, run, repo := setupTest(t)
	dir := survivedWorkRepo(t)
	writeFile(t, dir, "package.lock", "locked\n")
	work, _ := CaptureSurvivedWork(dir, run.ID, recordSurvivedWork(database, run.ID))
	step := &adaptiveCallStep{name: types.StepCI, fn: func(sctx *StepContext) (*StepOutcome, error) {
		if !sctx.Fixing {
			return &StepOutcome{NeedsApproval: true, AutoFixable: true, Findings: `{"findings":[{"id":"selected","severity":"error","description":"repair","action":"auto-fix"}]}`}, nil
		}
		return ProtectedPathOutcome(&ProtectedPathError{Path: "package.lock", Rule: "*.lock"}), nil
	}}
	exec := NewExecutor(database, p, &config.Config{AutoFix: config.AutoFix{CI: 1}}, nil, []Step{step}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = exec.Execute(ctx, run, repo, dir)
	if got := readFile(t, dir, "package.lock"); got != "locked\n" {
		t.Fatalf("refused file taken from the worktree: %q", got)
	}
	if left, _ := database.GetRunSurvivedWork(run.ID); left == nil || left.Ref != work.Ref || left.State != types.SurvivedWorkRestored {
		t.Fatalf("handoff after refusal = %+v", left)
	}
}

// A resumed fix round whose restored work was already committed does not
// apply the capture again: the handoff is consumed.
func TestExecutor_ResumedRoundDoesNotReapplyCommittedWork(t *testing.T) {
	database, p, run, repo := setupTest(t)
	dir := survivedWorkRepo(t)
	writeFile(t, dir, "partial.txt", "survived\n")
	work, _ := CaptureSurvivedWork(dir, run.ID, recordSurvivedWork(database, run.ID))
	if err := RestoreSurvivedWork(dir, work); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-qm", "repair committed before the cut")
	restored := *work
	restored.State = types.SurvivedWorkRestored
	if err := database.SetRunSurvivedWork(run.ID, &restored); err != nil {
		t.Fatal(err)
	}
	exec := NewExecutor(database, p, &config.Config{}, nil, nil, nil)
	got, err := exec.restoreSurvivedWork(run.ID, dir, func(string) {})
	if err != nil || got != nil {
		t.Fatalf("restore = %+v, %v; want nothing restored", got, err)
	}
	if left, _ := database.GetRunSurvivedWork(run.ID); left != nil {
		t.Fatalf("consumed handoff kept: %+v", left)
	}
	if status := gitRun(t, dir, "status", "--porcelain"); status != "" {
		t.Fatalf("capture reapplied: %q", status)
	}
	_ = repo
}

// A HEAD file the repair untracked and ignored is not in the snapshot, and
// the reset would overwrite it: the work stays in place.
func TestCaptureSurvivedWork_KeepsAnUntrackedIgnoredHeadFile(t *testing.T) {
	dir := survivedWorkRepo(t)
	writeFile(t, dir, "generated.txt", "head version\n")
	gitRun(t, dir, "add", "generated.txt")
	gitRun(t, dir, "commit", "-qm", "track generated")
	gitRun(t, dir, "rm", "-q", "--cached", "generated.txt")
	writeFile(t, dir, ".gitignore", "generated.txt\n")
	writeFile(t, dir, "generated.txt", "repair edit\n")
	work, _ := CaptureSurvivedWork(dir, "run-19", nil)
	if work == nil || work.State != types.SurvivedWorkInPlace || !strings.Contains(work.InspectionError, "generated.txt") {
		t.Fatalf("capture = %+v", work)
	}
	if got := readFile(t, dir, "generated.txt"); got != "repair edit\n" {
		t.Fatalf("generated.txt = %q", got)
	}
}

// An ignored file under a directory that replaced a HEAD file is lost to the
// reset as well: the work stays in place.
func TestCaptureSurvivedWork_KeepsIgnoredFileWhereHeadHasAFile(t *testing.T) {
	dir := survivedWorkRepo(t)
	writeFile(t, dir, "config", "head file\n")
	gitRun(t, dir, "add", "config")
	gitRun(t, dir, "commit", "-qm", "config file")
	gitRun(t, dir, "rm", "-q", "config")
	if err := os.MkdirAll(filepath.Join(dir, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "config/default.yaml", "a: 1\n")
	gitRun(t, dir, "add", "config/default.yaml")
	writeFile(t, dir, ".gitignore", "config/local.yaml\n")
	writeFile(t, dir, "config/local.yaml", "secret: local\n")
	work, _ := CaptureSurvivedWork(dir, "run-20", nil)
	if work == nil || work.State != types.SurvivedWorkInPlace || !strings.Contains(work.InspectionError, "config/local.yaml") {
		t.Fatalf("capture = %+v", work)
	}
	if got := readFile(t, dir, "config/local.yaml"); got != "secret: local\n" {
		t.Fatalf("local file = %q", got)
	}
}
