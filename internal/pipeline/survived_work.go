package pipeline

import (
	"context"
	"fmt"
	"os"
	pathpkg "path"
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

type survivedWorkGit struct {
	noCallbacks bool
}

func (g survivedWorkGit) args(args []string) []string {
	if g.noCallbacks {
		return append([]string{"-c", "core.hooksPath=" + os.DevNull, "-c", "core.fsmonitor=false"}, args...)
	}
	return args
}

func (g survivedWorkGit) Run(ctx context.Context, dir string, args ...string) (string, error) {
	return git.Run(ctx, dir, g.args(args)...)
}

func (g survivedWorkGit) RunRaw(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return git.RunRaw(ctx, dir, g.args(args)...)
}

func (g survivedWorkGit) RunWithEnv(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	return git.RunWithEnv(ctx, dir, env, g.args(args)...)
}

func (g survivedWorkGit) RunWithInput(ctx context.Context, dir, input string, args ...string) (string, error) {
	return git.RunWithInput(ctx, dir, input, g.args(args)...)
}

func (e *Executor) survivedWorkGit(runID string) (survivedWorkGit, error) {
	isolated, err := e.db.TestIsolationEnabled(runID)
	return survivedWorkGit{noCallbacks: isolated}, err
}

func CaptureSurvivedWork(workDir, runID string, record func(*types.SurvivedWork) error) (*types.SurvivedWork, error) {
	return (survivedWorkGit{}).CaptureSurvivedWork(workDir, runID, record)
}

func RestoreSurvivedWork(workDir string, work *types.SurvivedWork) error {
	return (survivedWorkGit{}).RestoreSurvivedWork(workDir, work)
}

func SurvivedWorkRecorder(database *db.DB, runID, workDir string) func(*types.SurvivedWork) error {
	return (survivedWorkGit{}).SurvivedWorkRecorder(database, runID, workDir)
}

func ClearSurvivedWork(database *db.DB, runID, workDir string) error {
	return (survivedWorkGit{}).ClearSurvivedWork(database, runID, workDir)
}

// CaptureSurvivedWork moves a cut-short repair's uncommitted work into a
// run-owned ref and returns the worktree to HEAD. It returns nil for a clean
// worktree. record makes the evidence durable and is called before the
// worktree is touched, so a crash in between leaves the work in both places,
// never in neither. When the work cannot be captured, recorded or removed it
// stays where it is, marked in_place, and cleanup refuses to remove the
// worktree (see the daemon) rather than lose it. The error reports in-place
// work whose handoff could not be recorded either: a marker file in the
// worktree's git directory then stands in for it, best effort.
func (g survivedWorkGit) CaptureSurvivedWork(workDir, runID string, record func(*types.SurvivedWork) error) (*types.SurvivedWork, error) {
	if record == nil {
		record = func(*types.SurvivedWork) error { return nil }
	}
	work := g.captureSurvivedWork(workDir, runID, record)
	if work == nil || work.State != types.SurvivedWorkInPlace {
		return work, nil
	}
	if err := record(work); err != nil {
		g.writeSurvivedWorkMarker(workDir)
		return work, fmt.Errorf("record interrupted repair work left in %s: %w", workDir, err)
	}
	return work, nil
}

// survivedWorkMarker names the file in a worktree's git directory that keeps
// cleanup away from in-place work whose handoff could not be recorded.
const survivedWorkMarker = "no-mistakes-survived-work"

func (g survivedWorkGit) survivedWorkMarkerPath(workDir string) string {
	ctx, cancel := context.WithTimeout(context.Background(), survivedWorkGitTimeout)
	defer cancel()
	dir, err := g.Run(ctx, workDir, "rev-parse", "--path-format=absolute", "--git-dir")
	if err != nil || strings.TrimSpace(dir) == "" {
		return ""
	}
	return filepath.Join(strings.TrimSpace(dir), survivedWorkMarker)
}

func (g survivedWorkGit) writeSurvivedWorkMarker(workDir string) {
	if path := g.survivedWorkMarkerPath(workDir); path != "" {
		_ = os.WriteFile(path, []byte("interrupted repair work left in this worktree\n"), 0o600)
	}
}

// SurvivedWorkMarked reports whether in-place work whose handoff could not be
// recorded marked this worktree, and drops the marker once the worktree is
// clean again.
func SurvivedWorkMarked(workDir string) bool {
	g := survivedWorkGit{}
	path := g.survivedWorkMarkerPath(workDir)
	if path == "" {
		return false
	}
	if _, err := os.Stat(path); err != nil {
		return false
	}
	if g.survivedWorkUnclean(workDir) {
		return true
	}
	_ = os.Remove(path)
	return false
}

// SurvivedWorkRecorder returns the record callback that stores a run's
// handoff and then deletes the capture ref it replaced, so only the latest
// capture is kept. A replaced in-place handoff has no ref; a new in-place one
// keeps the old ref, since that is all that is known to hold the work.
func (g survivedWorkGit) SurvivedWorkRecorder(database *db.DB, runID, workDir string) func(*types.SurvivedWork) error {
	return func(work *types.SurvivedWork) error {
		previous, err := database.GetRunSurvivedWork(runID)
		if err != nil {
			return err
		}
		if err := database.SetRunSurvivedWork(runID, work); err != nil {
			return err
		}
		if previous != nil && previous.Ref != "" && work.Ref != "" && previous.Ref != work.Ref {
			// The new capture holds the old one's work: retire the old ref and
			// every rerun's copy of it.
			if inUse, err := database.ClearSurvivedWorkRef(previous.Ref); err != nil {
				return err
			} else if !inUse {
				g.deleteSurvivedWorkRef(workDir, previous.Ref)
			}
		}
		return nil
	}
}

// ClearSurvivedWork drops the run's handoff once a repair committed the work,
// and the capture ref with it.
func (g survivedWorkGit) ClearSurvivedWork(database *db.DB, runID, workDir string) error {
	previous, err := database.GetRunSurvivedWork(runID)
	if err != nil {
		return err
	}
	if err := database.ClearRunSurvivedWork(runID); err != nil {
		return err
	}
	if previous != nil && previous.Ref != "" {
		if inUse, err := database.ClearSurvivedWorkRef(previous.Ref); err != nil {
			return err
		} else if !inUse {
			g.deleteSurvivedWorkRef(workDir, previous.Ref)
		}
	}
	return nil
}

func (g survivedWorkGit) deleteSurvivedWorkRef(workDir, ref string) {
	if !strings.HasPrefix(ref, survivedWorkRefPrefix) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), survivedWorkGitTimeout)
	defer cancel()
	_, _ = g.Run(ctx, workDir, "update-ref", "-d", ref)
}

func (g survivedWorkGit) captureSurvivedWork(workDir, runID string, record func(*types.SurvivedWork) error) *types.SurvivedWork {
	ctx, cancel := context.WithTimeout(context.Background(), survivedWorkGitTimeout)
	defer cancel()
	// Raw output: a porcelain line's leading status column is significant.
	statusRaw, err := g.RunRaw(ctx, workDir, "status", "--porcelain", "--untracked-files=all", "--ignore-submodules=none")
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
	if dirty, err := g.dirtySubmodule(ctx, workDir); err != nil || dirty != "" {
		if err != nil {
			work.InspectionError = "could not check submodules: " + err.Error()
		} else {
			work.InspectionError = "submodule " + dirty + " has uncommitted changes the capture cannot hold"
		}
		work.Next = survivedWorkInPlaceNext
		return work
	}
	// A nested repository (an untracked directory with its own .git) is
	// snapshotted only as a gitlink, and HEAD's ignore rules can hide it
	// once the worktree is reset, so it is caught here, before anything moves.
	statusZ, err := g.RunRaw(ctx, workDir, "status", "--porcelain", "-z", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		work.InspectionError = "worktree status unreadable"
		work.Next = survivedWorkInPlaceNext
		return work
	}
	if nested := nestedRepository(string(statusZ)); nested != "" {
		work.InspectionError = "nested repository " + nested + " holds changes the capture cannot hold"
		work.Next = survivedWorkInPlaceNext
		return work
	}
	// The snapshot skips ignored files, and the reset rewrites every file
	// HEAD tracks: a HEAD file the repair untracked and ignored would be
	// overwritten with nothing holding its edits.
	if path, err := g.ignoredTrackedFile(ctx, workDir); err != nil || path != "" {
		if err != nil {
			work.InspectionError = "could not check ignored files: " + err.Error()
		} else {
			work.InspectionError = "ignored file " + path + " is tracked in HEAD, so the capture cannot hold its edits"
		}
		work.Next = survivedWorkInPlaceNext
		return work
	}
	base, commit, err := g.snapshotSurvivedWork(ctx, workDir)
	if err != nil {
		work.InspectionError = "could not capture the uncommitted work: " + err.Error()
		work.Next = survivedWorkInPlaceNext
		return work
	}
	ref := survivedWorkRefPrefix + runID + "/" + commit[:12]
	if _, err := g.Run(ctx, workDir, "update-ref", ref, commit); err != nil {
		work.InspectionError = "could not record the uncommitted work: " + err.Error()
		work.Next = survivedWorkInPlaceNext
		return work
	}
	// Recorded in place first: until the worktree is verified clean below,
	// the worktree may still hold work the ref does not (a nested
	// repository), and only in_place keeps cleanup away from it.
	work.State, work.Ref, work.BaseSHA = types.SurvivedWorkInPlace, ref, base
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
	// Untracked files are cleaned once, before the reset, under the repair's
	// own ignore rules - the rules the snapshot was taken under, so only
	// files it holds go. After the reset, HEAD's rules can expose a file the
	// repair had ignored, which the snapshot does not hold: the status check
	// below then keeps the work in place rather than cleaning it away. git
	// clean never follows symlinks.
	if _, err := g.Run(ctx, workDir, "clean", "-fdq"); err != nil {
		work.InspectionError = "captured, but its untracked files could not be removed: " + err.Error()
	} else if _, err := g.Run(ctx, workDir, "reset", "-q", "--hard", "HEAD"); err != nil {
		work.InspectionError = "captured, but the worktree could not be reset: " + err.Error()
	}
	// git clean skips nested repositories, and the snapshot holds only a
	// gitlink for one, so a clean exit does not prove the worktree is clean.
	if work.InspectionError == "" {
		if rest, err := g.RunRaw(ctx, workDir, "status", "--porcelain", "--untracked-files=all", "--ignore-submodules=none"); err != nil {
			work.InspectionError = "captured, but the worktree could not be checked afterwards: " + err.Error()
		} else if strings.TrimSpace(string(rest)) != "" {
			work.InspectionError = "captured, but changes the capture cannot hold (such as a nested repository) are still in the worktree"
		}
	}
	if work.InspectionError == "" {
		work.State = types.SurvivedWorkCaptured
		if err := record(work); err != nil {
			// The ref holds everything and the worktree is clean; the in_place
			// row still points at the ref, so nothing is lost.
			work.State = types.SurvivedWorkInPlace
		}
	}
	if work.InspectionError != "" {
		// The work is still (partly) in the worktree: it stays in place,
		// and the ref is kept for recovery. The caller records this.
		work.Next = fmt.Sprintf("Captured in %s, but also still in the run worktree, which cleanup keeps while it holds changes. Recover it with git diff %s %s, or from the worktree.", ref, base, ref)
	}
	return work
}

const survivedWorkInPlaceNext = "Left uncommitted in the run worktree and not pushed. Cleanup refuses to remove the worktree while it holds changes; recover the files from it by hand before abandoning the run."

// snapshotSurvivedWork writes the index and the full working tree as two
// commits on HEAD without touching the real index or the worktree.
func (g survivedWorkGit) snapshotSurvivedWork(ctx context.Context, workDir string) (base, commit string, err error) {
	base, err = g.Run(ctx, workDir, "rev-parse", "HEAD")
	if err != nil {
		return "", "", err
	}
	base = strings.TrimSpace(base)
	indexTree, err := g.Run(ctx, workDir, "write-tree")
	if err != nil {
		return "", "", fmt.Errorf("write index tree: %w", err)
	}
	indexPath, err := g.Run(ctx, workDir, "rev-parse", "--path-format=absolute", "--git-path", "index")
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
	if _, err := g.RunWithEnv(ctx, workDir, env, "add", "-A"); err != nil {
		return "", "", fmt.Errorf("stage working tree: %w", err)
	}
	worktreeTree, err := g.RunWithEnv(ctx, workDir, env, "write-tree")
	if err != nil {
		return "", "", fmt.Errorf("write working tree: %w", err)
	}
	indexCommit, err := g.Run(ctx, workDir, "commit-tree", strings.TrimSpace(indexTree), "-p", base, "-m", "no-mistakes: interrupted repair, index")
	if err != nil {
		return "", "", err
	}
	commit, err = g.Run(ctx, workDir, "commit-tree", strings.TrimSpace(worktreeTree), "-p", strings.TrimSpace(indexCommit), "-m", "no-mistakes: interrupted repair, working tree")
	if err != nil {
		return "", "", err
	}
	return base, strings.TrimSpace(commit), nil
}

// RestoreSurvivedWork puts captured work back into a clean worktree before an
// authorized repair runs. On the HEAD it was captured from, the index and the
// working tree come back exactly; on a moved HEAD the staged layer and then
// the unstaged layer are each merged three-way, preserving their index split.
// A restore that cannot apply cleanly leaves the worktree as it was and
// returns an error; the ref and its evidence stay.
func (g survivedWorkGit) RestoreSurvivedWork(workDir string, work *types.SurvivedWork) error {
	if work == nil || work.Ref == "" {
		return fmt.Errorf("no captured work to restore")
	}
	ctx, cancel := context.WithTimeout(context.Background(), survivedWorkGitTimeout)
	defer cancel()
	status, err := g.Run(ctx, workDir, "status", "--porcelain", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return fmt.Errorf("read worktree status: %w", err)
	}
	if strings.TrimSpace(status) != "" {
		return fmt.Errorf("worktree is not clean, so restoring would mix in other changes")
	}
	head, err := g.Run(ctx, workDir, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(head) == work.BaseSHA {
		if _, err := g.Run(ctx, workDir, "read-tree", "-m", "-u", "HEAD", work.Ref+"^{tree}"); err != nil {
			return fmt.Errorf("restore working tree: %w", err)
		}
		if _, err := g.Run(ctx, workDir, "read-tree", work.Ref+"^^{tree}"); err != nil {
			return fmt.Errorf("restore index: %w", err)
		}
		return nil
	}
	// Both layers go back, each merged three-way: the index commit onto the
	// moved HEAD into the index, then the working-tree commit on top of it,
	// so staged-only work survives too and the index keeps only what was
	// staged.
	undo := func() {
		_, _ = g.Run(ctx, workDir, "reset", "-q", "--hard", "HEAD")
		_, _ = g.Run(ctx, workDir, "clean", "-fdq")
	}
	if err := g.applySurvivedLayer(ctx, workDir, work.BaseSHA, work.Ref+"^", "--3way", "--index"); err != nil {
		undo()
		return fmt.Errorf("captured staged work does not apply to the moved HEAD: %w", err)
	}
	// The unstaged layer is merged three-way as well, which needs the
	// index; the index then goes back to the staged layer alone.
	stagedTree, err := g.Run(ctx, workDir, "write-tree")
	if err != nil {
		undo()
		return fmt.Errorf("record restored staged work: %w", err)
	}
	if err := g.applySurvivedLayer(ctx, workDir, work.Ref+"^", work.Ref, "--3way", "--index"); err != nil {
		undo()
		return fmt.Errorf("captured unstaged work does not apply to the moved HEAD: %w", err)
	}
	if _, err := g.Run(ctx, workDir, "read-tree", strings.TrimSpace(stagedTree)); err != nil {
		undo()
		return fmt.Errorf("restore the staged layer: %w", err)
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
func (g survivedWorkGit) survivedWorkUnclean(workDir string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), survivedWorkGitTimeout)
	defer cancel()
	status, err := g.Run(ctx, workDir, "status", "--porcelain", "--untracked-files=all", "--ignore-submodules=none")
	return err != nil || strings.TrimSpace(status) != ""
}

// SurvivedWorkHoldsWorktree reports whether cleanup must keep a worktree:
// when the run's survived work is in it - left in place, or restored for a
// repair that never settled (a shutdown mid-round) and may have added to it -
// and the worktree still holds changes. Captured work lives in its ref and
// never holds the worktree.
func SurvivedWorkHoldsWorktree(work *types.SurvivedWork, workDir string) bool {
	g := survivedWorkGit{}
	return work != nil && work.State != types.SurvivedWorkCaptured && g.survivedWorkUnclean(workDir)
}

// dirtySubmodule returns the first changed path in status that is a
// submodule (a gitlink in the index), or "".
// Both listings are read NUL-delimited, so paths compare exactly whatever
// characters they hold.
func (g survivedWorkGit) dirtySubmodule(ctx context.Context, workDir string) (string, error) {
	stage, err := g.RunRaw(ctx, workDir, "ls-files", "--stage", "-z")
	if err != nil {
		return "", err
	}
	gitlinks := map[string]bool{}
	for _, entry := range strings.Split(string(stage), "\x00") {
		if strings.HasPrefix(entry, "160000 ") {
			if tab := strings.IndexByte(entry, '\t'); tab >= 0 {
				gitlinks[entry[tab+1:]] = true
			}
		}
	}
	if len(gitlinks) == 0 {
		return "", nil
	}
	status, err := g.RunRaw(ctx, workDir, "status", "--porcelain", "-z", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return "", err
	}
	for _, e := range porcelainZPaths(string(status)) {
		if gitlinks[e[1]] {
			return e[1], nil
		}
	}
	return "", nil
}

// ignoredTrackedFile returns the first ignored, untracked file the reset to
// HEAD would overwrite - HEAD tracks it, or a file or directory in its
// place - or "".
func (g survivedWorkGit) ignoredTrackedFile(ctx context.Context, workDir string) (string, error) {
	ignored, err := g.RunRaw(ctx, workDir, "ls-files", "-z", "--others", "--ignored", "--exclude-standard")
	if err != nil {
		return "", err
	}
	if len(ignored) == 0 {
		return "", nil
	}
	tracked, err := g.RunRaw(ctx, workDir, "ls-tree", "-r", "-z", "--name-only", "HEAD")
	if err != nil {
		return "", err
	}
	// The reset also replaces a HEAD file where an ignored file now sits
	// under a directory of that name, and a HEAD directory where an
	// ignored file now has its name.
	headFiles, headDirs := map[string]bool{}, map[string]bool{}
	for _, path := range strings.Split(string(tracked), "\x00") {
		if path == "" {
			continue
		}
		headFiles[path] = true
		for dir := pathpkg.Dir(path); dir != "."; dir = pathpkg.Dir(dir) {
			headDirs[dir] = true
		}
	}
	for _, path := range strings.Split(string(ignored), "\x00") {
		if path == "" {
			continue
		}
		if headFiles[path] || headDirs[path] {
			return path, nil
		}
		for dir := pathpkg.Dir(path); dir != "."; dir = pathpkg.Dir(dir) {
			if headFiles[dir] {
				return path, nil
			}
		}
	}
	return "", nil
}

// porcelainZPaths returns the paths of a `status --porcelain -z` listing,
// each with its two-letter status. A rename or copy record carries its
// source as the next field, which is skipped, never read as an entry.
func porcelainZPaths(raw string) [][2]string {
	var out [][2]string
	fields := strings.Split(raw, "\x00")
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if len(f) < 4 {
			continue
		}
		xy, path := f[:2], f[3:]
		out = append(out, [2]string{xy, path})
		if xy[0] == 'R' || xy[0] == 'C' {
			i++
		}
	}
	return out
}

// nestedRepository returns the first untracked directory in a porcelain -z
// listing taken with --untracked-files=all, which lists files, so a
// directory entry is a nested repository; or "".
func nestedRepository(statusZ string) string {
	for _, e := range porcelainZPaths(statusZ) {
		if e[0] == "??" && strings.HasSuffix(e[1], "/") {
			return strings.TrimSuffix(e[1], "/")
		}
	}
	return ""
}

// applySurvivedLayer applies the change from one captured commit to another.
// It is a machine-read patch: raw bytes (no textconv, no external diff),
// fixed a/ b/ prefixes whatever diff.noprefix or diff.mnemonicPrefix say,
// applied with the matching -p1.
func (g survivedWorkGit) applySurvivedLayer(ctx context.Context, workDir, from, to string, applyArgs ...string) error {
	patch, err := g.RunRaw(ctx, workDir, "-c", "diff.noprefix=false", "-c", "diff.mnemonicPrefix=false", "diff", "--binary", "--no-ext-diff", "--no-textconv", "--no-color", "--src-prefix=a/", "--dst-prefix=b/", from, to)
	if err != nil {
		return fmt.Errorf("read captured changes: %w", err)
	}
	if len(patch) == 0 {
		return nil
	}
	_, err = g.RunWithInput(ctx, workDir, string(patch), append([]string{"apply", "-p1"}, applyArgs...)...)
	return err
}
