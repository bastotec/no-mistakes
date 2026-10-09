package pipeline

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// InspectSurvivedWork runs outside the expired agent context. An unreadable
// index is not evidence of a clean worktree and must retain the checkout too.
func InspectSurvivedWork(workDir string) *types.SurvivedWork {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	status, err := git.Run(ctx, workDir, "status", "--porcelain", "--untracked-files=all")
	if err == nil && strings.TrimSpace(status) == "" {
		return nil
	}
	work := &types.SurvivedWork{Worktree: workDir, Next: "Retained uncommitted, not pushed. The next authorized repair reuses this worktree and must inspect and continue this work within its selected findings; only a successful repair reaches the pipeline commit boundary. Cleanup refuses while changes remain or status is unreadable. Recover these files manually before abandoning the run."}
	if err != nil {
		work.InspectionError = "worktree status unreadable"
	} else {
		work.Files = strings.Split(strings.TrimRight(status, "\r\n"), "\n")
	}
	return work
}

// SurvivedWorkPrompt does not grant new repair scope or certify partial edits.
func SurvivedWorkPrompt(workDir string) string {
	work := InspectSurvivedWork(workDir)
	if work == nil {
		return ""
	}
	return fmt.Sprintf("\n\nExisting uncommitted repair work:\n%s\nInspect git status and the staged and unstaged diffs before editing. Preserve and continue relevant existing edits, but do not expand beyond the selected findings. Do not reset or clean away prior work. The pipeline, not you, owns the commit boundary.\n", work.Next)
}
