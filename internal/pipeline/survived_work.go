package pipeline

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type WorktreeSnapshot map[string]string

func SnapshotWorktree(workDir string) (WorktreeSnapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	status, err := git.Run(ctx, workDir, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames", "--ignore-submodules=none")
	if err != nil {
		return nil, err
	}
	paths, err := statusPaths(status)
	if err != nil {
		return nil, err
	}
	snapshot := make(WorktreeSnapshot, len(paths))
	for _, path := range paths {
		signature, err := worktreePathSignature(ctx, workDir, path)
		if err != nil {
			return nil, err
		}
		snapshot[path] = signature
	}
	return snapshot, nil
}

func ChangedPathsSince(workDir string, before WorktreeSnapshot) ([]string, error) {
	after, err := SnapshotWorktree(workDir)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(before)+len(after))
	seen := make(map[string]bool, len(before)+len(after))
	for path, signature := range before {
		seen[path] = true
		if after[path] != signature {
			paths = append(paths, path)
		}
	}
	for path, signature := range after {
		if !seen[path] && before[path] != signature {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func statusPaths(status string) ([]string, error) {
	entries := strings.Split(strings.TrimSuffix(status, "\x00"), "\x00")
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry == "" {
			continue
		}
		if len(entry) < 4 || entry[2] != ' ' {
			return nil, fmt.Errorf("invalid git status entry %q", entry)
		}
		paths = append(paths, entry[3:])
	}
	return paths, nil
}

func worktreePathSignature(ctx context.Context, workDir, path string) (string, error) {
	index, err := git.Run(ctx, workDir, "ls-files", "--stage", "-z", "--", path)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	_, _ = io.WriteString(hash, index)
	fullPath := filepath.Join(workDir, filepath.FromSlash(path))
	info, err := os.Lstat(fullPath)
	if os.IsNotExist(err) {
		_, _ = io.WriteString(hash, "\x00missing")
		return fmt.Sprintf("%x", hash.Sum(nil)), nil
	}
	if err != nil {
		return "", err
	}
	_, _ = fmt.Fprintf(hash, "\x00%d\x00", info.Mode())
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(fullPath)
		if err != nil {
			return "", err
		}
		_, _ = io.WriteString(hash, target)
	} else if info.Mode().IsRegular() {
		file, err := os.Open(fullPath)
		if err != nil {
			return "", err
		}
		_, copyErr := io.Copy(hash, file)
		closeErr := file.Close()
		if copyErr != nil {
			return "", copyErr
		}
		if closeErr != nil {
			return "", closeErr
		}
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

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
