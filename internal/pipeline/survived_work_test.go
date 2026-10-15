package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
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
	if work := CaptureSurvivedWork(dir, "run-1"); work != nil {
		t.Fatalf("clean worktree captured: %+v", work)
	}
	leavePartialRepair(t, dir)
	head := gitRun(t, dir, "rev-parse", "HEAD")

	work := CaptureSurvivedWork(dir, "run-1")
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
	work := CaptureSurvivedWork(dir, "run-2")
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
	work := CaptureSurvivedWork(dir, "run-3")
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
	work := CaptureSurvivedWork(dir, "run-4")
	writeFile(t, dir, "other.txt", "someone else's change\n")
	if err := RestoreSurvivedWork(dir, work); err == nil {
		t.Fatal("restore mixed captured work into a dirty worktree")
	}
}

func TestCaptureSurvivedWork_UnreadableStatusStaysInPlace(t *testing.T) {
	work := CaptureSurvivedWork(filepath.Join(t.TempDir(), "missing"), "run-5")
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

// A repair cut short by its deadline leaves its work captured, the worktree
// clean, and the selected and deferred findings on the gate.
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
			sr := stepResultByName(t, database, run.ID, types.StepTest)
			if sr.FindingsJSON == nil {
				t.Fatal("missing durable findings")
			}
			findings, err := types.ParseFindingsJSON(*sr.FindingsJSON)
			if err != nil || findings.SurvivedWork == nil || findings.SurvivedWork.Ref == "" || len(findings.SurvivedWork.Files) != 1 {
				t.Fatalf("survival missing: %+v %v", findings.SurvivedWork, err)
			}
			if len(findings.Items) != 2 || findings.Items[0].ID != "selected" || findings.Items[1].ID != "deferred" {
				t.Fatalf("selected and deferred findings not preserved: %+v", findings.Items)
			}
			if got := gitRun(t, dir, "show", findings.SurvivedWork.Ref+":progress.txt"); got != "survived" {
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
	work := CaptureSurvivedWork(dir, run.ID)
	prior, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := types.MarshalFindingsJSON(types.Findings{SurvivedWork: work})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetStepFindings(prior.ID, raw); err != nil {
		t.Fatal(err)
	}

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
	if left, err := LatestSurvivedWork(database, run.ID); err != nil || left != nil {
		t.Fatalf("evidence not cleared after restore: %+v %v", left, err)
	}
}

func TestExecutor_RetainsSurvivedWorkAcrossLaterOutcome(t *testing.T) {
	database, p, run, _ := setupTest(t)
	step, err := database.InsertStepResult(run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	prior, err := types.MarshalFindingsJSON(types.Findings{SurvivedWork: &types.SurvivedWork{Ref: "refs/no-mistakes/survived/x/y", BaseSHA: "abc"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetStepFindings(step.ID, prior); err != nil {
		t.Fatal(err)
	}
	next := `{"findings":[{"id":"later","severity":"error","description":"still red","action":"auto-fix"}]}`
	exec := NewExecutor(database, p, &config.Config{}, nil, nil, nil)
	retained, err := types.ParseFindingsJSON(exec.retainSurvivedWork(step.ID, next))
	if err != nil || retained.SurvivedWork == nil || len(retained.Items) != 1 || retained.Items[0].ID != "later" {
		t.Fatalf("later outcome lost survival metadata: %+v %v", retained, err)
	}
}
