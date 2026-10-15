package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
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
// worktree. When the work cannot be captured it is left where it is, the
// evidence says why, and Ref is empty: cleanup then refuses to remove the
// worktree (see the daemon) rather than lose it.
func CaptureSurvivedWork(workDir, runID string) *types.SurvivedWork {
	ctx, cancel := context.WithTimeout(context.Background(), survivedWorkGitTimeout)
	defer cancel()
	status, err := git.Run(ctx, workDir, "status", "--porcelain", "--untracked-files=all")
	if err == nil && strings.TrimSpace(status) == "" {
		return nil
	}
	work := &types.SurvivedWork{Worktree: workDir}
	if err != nil {
		work.InspectionError = "worktree status unreadable"
		work.Next = survivedWorkInPlaceNext
		return work
	}
	work.Files = strings.Split(strings.TrimRight(status, "\r\n"), "\n")
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
	// Only once the ref holds the work is the worktree returned to HEAD.
	if _, err := git.Run(ctx, workDir, "reset", "-q", "--hard", "HEAD"); err == nil {
		_, err = git.Run(ctx, workDir, "clean", "-fdq")
		if err != nil {
			work.InspectionError = "captured, but the worktree could not be cleaned: " + err.Error()
		}
	} else {
		work.InspectionError = "captured, but the worktree could not be reset: " + err.Error()
	}
	work.Ref, work.BaseSHA = ref, base
	work.Next = fmt.Sprintf("Captured in %s and removed from the worktree, so nothing commits or pushes it. The next authorized repair restores it before it runs and continues it within its selected findings; only that repair's commit publishes it. To recover it by hand: git diff %s %s.", ref, base, ref)
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

// LatestSurvivedWork returns the survived-work evidence the run carries, if any.
func LatestSurvivedWork(database *db.DB, runID string) (*types.SurvivedWork, error) {
	steps, err := database.GetStepsByRun(runID)
	if err != nil {
		return nil, err
	}
	for _, step := range steps {
		if step.FindingsJSON == nil {
			continue
		}
		findings, err := types.ParseFindingsJSON(*step.FindingsJSON)
		if err != nil {
			return nil, err
		}
		if findings.SurvivedWork != nil {
			return findings.SurvivedWork, nil
		}
	}
	return nil, nil
}

// ClearSurvivedWork drops the evidence once its work is back in the worktree:
// from then on it is ordinary repair work, and a repair that is cut short again
// captures it afresh.
func ClearSurvivedWork(database *db.DB, runID string) error {
	steps, err := database.GetStepsByRun(runID)
	if err != nil {
		return err
	}
	for _, step := range steps {
		if step.FindingsJSON == nil {
			continue
		}
		findings, err := types.ParseFindingsJSON(*step.FindingsJSON)
		if err != nil {
			return err
		}
		if findings.SurvivedWork == nil {
			continue
		}
		findings.SurvivedWork = nil
		raw, err := types.MarshalFindingsJSON(findings)
		if err != nil {
			return err
		}
		if err := database.SetStepFindings(step.ID, raw); err != nil {
			return err
		}
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

// SurvivedWorkHoldsWorktree reports whether cleanup must keep a worktree: only
// when the run's survived work could not be captured into a ref and the
// worktree still holds changes.
func SurvivedWorkHoldsWorktree(work *types.SurvivedWork, workDir string) bool {
	return work != nil && work.Ref == "" && survivedWorkUnclean(workDir)
}
