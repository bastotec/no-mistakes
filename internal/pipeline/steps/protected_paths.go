package steps

import (
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// stagePipelineChanges guards every pipeline-owned catch-all staging path,
// including Push's leftover commit. Refusal preserves the index and worktree.
func stagePipelineChanges(sctx *pipeline.StepContext) error {
	if sctx.RoundStartWorktree == nil && sctx.DB != nil && sctx.Run != nil {
		survived, err := pipeline.HasSurvivedWorkEvidence(sctx.DB, sctx.Run.ID)
		if err != nil {
			return fmt.Errorf("inspect survived work: %w", err)
		}
		if survived && pipeline.InspectSurvivedWork(sctx.WorkDir) != nil {
			return fmt.Errorf("survived work requires a scoped worktree snapshot")
		}
	}
	var paths []string
	if sctx.RoundStartWorktree != nil {
		var err error
		paths, err = pipeline.ChangedPathsSince(sctx.WorkDir, sctx.RoundStartWorktree)
		if err != nil {
			return fmt.Errorf("compare round-start worktree: %w", err)
		}
	}
	selected := make(map[string]bool, len(paths))
	for _, path := range paths {
		selected[path] = true
	}
	if len(sctx.Config.ProtectedPaths) > 0 {
		// Disable renames so both source and destination are checked, and list
		// individual untracked files so a protected path inside a new directory
		// cannot hide behind the directory entry. NULs preserve unusual names.
		status, err := stepGitRunRaw(sctx, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames", "--ignore-submodules=none")
		if err != nil {
			return fmt.Errorf("check protected_paths: %w", err)
		}
		for _, entry := range strings.Split(strings.TrimSuffix(status, "\x00"), "\x00") {
			if entry == "" {
				continue
			}
			if len(entry) < 4 || entry[2] != ' ' {
				return fmt.Errorf("check protected_paths: invalid git status entry %q", entry)
			}
			file := entry[3:]
			if sctx.RoundStartWorktree != nil && !selected[file] {
				continue
			}
			for _, pattern := range sctx.Config.ProtectedPaths {
				if matchIgnorePattern(file, pattern) {
					return &pipeline.ProtectedPathError{Path: file, Rule: pattern}
				}
			}
		}
	}
	if sctx.RoundStartWorktree == nil {
		_, err := stepGitRun(sctx, "add", "-A")
		return err
	}
	if _, err := stepGitRun(sctx, "reset", "-q", "HEAD", "--", "."); err != nil {
		return err
	}
	if len(paths) == 0 {
		return nil
	}
	args := append([]string{"add", "-A", "--"}, paths...)
	_, err := stepGitRun(sctx, args...)
	return err
}
