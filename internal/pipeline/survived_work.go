package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Work a repair agent leaves uncommitted when its round is cut short is moved
// out of the run worktree into a run-owned ref instead of being left in place.
// Nothing that runs before the next authorized repair - formatting, review,
// push, a protected-path retry, a replayed repair - can then stage or publish
// it, because it is not in the worktree. The next authorized fix round puts it
// back before its agent runs and continues it; that repair's own commit is the
// only way it reaches the branch.
//
// The ref holds two commits on top of the round's HEAD: the index as the agent
// left it, then the full working tree (untracked files included, ignored files
// not) on top of that, so a restore brings back both what was staged and what
// was not.

// survivedWorkRefPrefix lives under refs/no-mistakes, which the gate never
// pushes, so a captured repair stays local.
const survivedWorkRefPrefix = "refs/no-mistakes/survived/"

const survivedWorkGitTimeout = 30 * time.Second

// CaptureSurvivedWork moves a cut-short repair's uncommitted work into a
// run-owned ref and returns the worktree to HEAD. It returns nil for a clean
// worktree. record makes the evidence durable and is called before the
// worktree is touched, so a crash in between leaves the work in both places,
// never in neither. When the work cannot be captured or recorded it is left
// where it is, the evidence says why, and Ref is empty: cleanup then refuses
// to remove the worktree (see the daemon) rather than lose it.
func CaptureSurvivedWork(workDir, runID string, record func(*types.SurvivedWork) error) *types.SurvivedWork {
	if record == nil {
		record = func(*types.SurvivedWork) error { return nil }
	}
	work := captureSurvivedWork(workDir, runID, record)
	if work != nil && work.Ref == "" {
		// Best effort: in-place evidence is what keeps cleanup away.
		_ = record(work)
	}
	return work
}

func captureSurvivedWork(workDir, runID string, record func(*types.SurvivedWork) error) *types.SurvivedWork {
	ctx, cancel := context.WithTimeout(context.Background(), survivedWorkGitTimeout)
	defer cancel()
	// Raw output: a porcelain line's leading status column is significant.
	statusRaw, err := git.RunRaw(ctx, workDir, "status", "--porcelain", "--untracked-files=all")
	status := string(statusRaw)
	if err == nil && strings.TrimSpace(status) == "" {
		return nil
	}
	work := &types.SurvivedWork{State: types.SurvivedWorkInPlace, Worktree: workDir}
	if err != nil {
		work.InspectionError = "worktree status unreadable"
		work.Next = survivedWorkInPlaceNext
		return work
	}
	work.Files = types.BoundSurvivedWorkFiles(strings.Split(strings.TrimRight(status, "\r\n"), "\n"))
	// A dirty submodule's own edits live in its repository, not in anything
	// the superproject can write as a tree: capture would record a gitlink
	// and lose them. Leave such work in place.
	if dirty, err := dirtySubmodule(ctx, workDir, status); err != nil || dirty != "" {
		if err != nil {
			work.InspectionError = "could not check submodules: " + err.Error()
		} else {
			work.InspectionError = "submodule " + dirty + " has uncommitted changes the capture cannot hold"
		}
		work.Next = survivedWorkInPlaceNext
		return work
	}
	base, commit, err := snapshotSurvivedWork(ctx, workDir)
	if err != nil {
		work.InspectionError = "could not capture the uncommitted work: " + err.Error()
		work.Next = survivedWorkInPlaceNext
		return work
	}
	ref := survivedWorkRefPrefix + runID + "/" + commit[:12]
	if _, err := git.Run(ctx, workDir, "update-ref", ref, commit); err != nil {
		work.InspectionError = "could not record the uncommitted work: " + err.Error()
		work.Next = survivedWorkInPlaceNext
		return work
	}
	work.State, work.Ref, work.BaseSHA = types.SurvivedWorkCaptured, ref, base
	work.Next = fmt.Sprintf("Captured in %s and removed from the worktree, so nothing commits or pushes it. The next authorized repair restores it before it runs and continues it within its selected findings; only that repair's commit publishes it. To recover it by hand: git diff %s %s.", ref, base, ref)
	// Only once the ref holds the work and the evidence is durable is the
	// worktree returned to HEAD.
	if err := record(work); err != nil {
		inPlace := *work
		inPlace.State, inPlace.Ref, inPlace.BaseSHA = types.SurvivedWorkInPlace, "", ""
		inPlace.InspectionError = fmt.Sprintf("captured in %s, but the handoff could not be recorded (%v), so it was also left in the worktree", ref, err)
		inPlace.Next = survivedWorkInPlaceNext
		return &inPlace
	}
	if _, err := git.Run(ctx, workDir, "reset", "-q", "--hard", "HEAD"); err == nil {
		if _, err := git.Run(ctx, workDir, "clean", "-fdq"); err != nil {
			work.InspectionError = "captured, but the worktree could not be cleaned: " + err.Error()
		}
	} else {
		work.InspectionError = "captured, but the worktree could not be reset: " + err.Error()
	}
	if work.InspectionError != "" {
		_ = record(work)
	}
	return work
}

const survivedWorkInPlaceNext = "Left uncommitted in the run worktree and not pushed. Cleanup refuses to remove the worktree while it holds changes; recover the files from it by hand before abandoning the run."

// snapshotSurvivedWork writes the index and the full working tree as two
// commits on HEAD without touching the real index or the worktree.
func snapshotSurvivedWork(ctx context.Context, workDir string) (base, commit string, err error) {
	base, err = git.Run(ctx, workDir, "rev-parse", "HEAD")
	if err != nil {
		return "", "", err
	}
	base = strings.TrimSpace(base)
	indexTree, err := git.Run(ctx, workDir, "write-tree")
	if err != nil {
		return "", "", fmt.Errorf("write index tree: %w", err)
	}
	indexPath, err := git.Run(ctx, workDir, "rev-parse", "--path-format=absolute", "--git-path", "index")
	if err != nil {
		return "", "", err
	}
	tmp, err := os.MkdirTemp("", "nm-survived-")
	if err != nil {
		return "", "", err
	}
	defer os.RemoveAll(tmp)
	tmpIndex := filepath.Join(tmp, "index")
	data, err := os.ReadFile(strings.TrimSpace(indexPath))
	if err != nil {
		return "", "", fmt.Errorf("copy index: %w", err)
	}
	if err := os.WriteFile(tmpIndex, data, 0o600); err != nil {
		return "", "", err
	}
	env := []string{"GIT_INDEX_FILE=" + tmpIndex}
	if _, err := git.RunWithEnv(ctx, workDir, env, "add", "-A"); err != nil {
		return "", "", fmt.Errorf("stage working tree: %w", err)
	}
	worktreeTree, err := git.RunWithEnv(ctx, workDir, env, "write-tree")
	if err != nil {
		return "", "", fmt.Errorf("write working tree: %w", err)
	}
	indexCommit, err := git.Run(ctx, workDir, "commit-tree", strings.TrimSpace(indexTree), "-p", base, "-m", "no-mistakes: interrupted repair, index")
	if err != nil {
		return "", "", err
	}
	commit, err = git.Run(ctx, workDir, "commit-tree", strings.TrimSpace(worktreeTree), "-p", strings.TrimSpace(indexCommit), "-m", "no-mistakes: interrupted repair, working tree")
	if err != nil {
		return "", "", err
	}
	return base, strings.TrimSpace(commit), nil
}

// RestoreSurvivedWork puts captured work back into a clean worktree before an
// authorized repair runs. On the HEAD it was captured from, the index and the
// working tree come back exactly; on a moved HEAD the working-tree changes are
// three-way applied, unstaged. A restore that cannot apply cleanly leaves the
// worktree as it was and returns an error; the ref and its evidence stay.
func RestoreSurvivedWork(workDir string, work *types.SurvivedWork) error {
	if work == nil || work.Ref == "" {
		return fmt.Errorf("no captured work to restore")
	}
	ctx, cancel := context.WithTimeout(context.Background(), survivedWorkGitTimeout)
	defer cancel()
	status, err := git.Run(ctx, workDir, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return fmt.Errorf("read worktree status: %w", err)
	}
	if strings.TrimSpace(status) != "" {
		return fmt.Errorf("worktree is not clean, so restoring would mix in other changes")
	}
	head, err := git.Run(ctx, workDir, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(head) == work.BaseSHA {
		if _, err := git.Run(ctx, workDir, "read-tree", "-m", "-u", "HEAD", work.Ref+"^{tree}"); err != nil {
			return fmt.Errorf("restore working tree: %w", err)
		}
		if _, err := git.Run(ctx, workDir, "read-tree", work.Ref+"^^{tree}"); err != nil {
			return fmt.Errorf("restore index: %w", err)
		}
		return nil
	}
	patch, err := git.RunRaw(ctx, workDir, "diff", "--binary", work.BaseSHA, work.Ref)
	if err != nil {
		return fmt.Errorf("read captured changes: %w", err)
	}
	if _, err := git.RunWithInput(ctx, workDir, string(patch), "apply", "--3way", "--index"); err != nil {
		_, _ = git.Run(ctx, workDir, "reset", "-q", "--hard", "HEAD")
		_, _ = git.Run(ctx, workDir, "clean", "-fdq")
		return fmt.Errorf("captured work does not apply to the moved HEAD: %w", err)
	}
	if _, err := git.Run(ctx, workDir, "reset", "-q"); err != nil {
		return err
	}
	return nil
}

// SurvivedWorkPrompt tells a repair whose worktree holds restored work what it
// is. It grants no new repair scope and certifies nothing.
func SurvivedWorkPrompt(restored *types.SurvivedWork) string {
	if restored == nil {
		return ""
	}
	return fmt.Sprintf("\n\nThis worktree holds uncommitted work an earlier repair of this run left when it was cut short, restored before you started:\n%s\nInspect git status and the staged and unstaged diffs before editing. Preserve and continue the relevant edits, but do not expand beyond the selected findings. Do not reset or clean away that work. The pipeline, not you, owns the commit boundary.\n", strings.Join(restored.Files, "\n"))
}

// survivedWorkUnclean reports whether a worktree still holds changes, treating
// an unreadable status as unclean.
func survivedWorkUnclean(workDir string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), survivedWorkGitTimeout)
	defer cancel()
	status, err := git.Run(ctx, workDir, "status", "--porcelain", "--untracked-files=all")
	return err != nil || strings.TrimSpace(status) != ""
}

// SurvivedWorkHoldsWorktree reports whether cleanup must keep a worktree:
// when the run's survived work is in it - left in place, or restored for a
// repair that never settled (a shutdown mid-round) and may have added to it -
// and the worktree still holds changes. Captured work lives in its ref and
// never holds the worktree.
func SurvivedWorkHoldsWorktree(work *types.SurvivedWork, workDir string) bool {
	return work != nil && work.State != types.SurvivedWorkCaptured && survivedWorkUnclean(workDir)
}

// dirtySubmodule returns the first changed path in status that is a
// submodule (a gitlink in the index), or "".
func dirtySubmodule(ctx context.Context, workDir, status string) (string, error) {
	stage, err := git.Run(ctx, workDir, "ls-files", "--stage")
	if err != nil {
		return "", err
	}
	gitlinks := map[string]bool{}
	for _, line := range strings.Split(stage, "\n") {
		if strings.HasPrefix(line, "160000 ") {
			if tab := strings.IndexByte(line, '\t'); tab >= 0 {
				gitlinks[line[tab+1:]] = true
			}
		}
	}
	if len(gitlinks) == 0 {
		return "", nil
	}
	for _, line := range strings.Split(status, "\n") {
		if len(line) > 3 && gitlinks[strings.TrimSpace(line[3:])] {
			return strings.TrimSpace(line[3:]), nil
		}
	}
	return "", nil
}
