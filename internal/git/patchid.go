package git

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// PatchRetention is the verdict of a Git patch-identity comparison between the
// commits exclusive to two heads: the private side (commits reachable only
// from privateHead) checked for retention in the live side (commits reachable
// only from liveHead). It is the content-identity counterpart of ancestry:
// SHA containment proves a private commit's changes were retained because the
// very commit object sits in the live history, while patch identity proves it
// for the legitimate rewrites custody operations produce (rebase,
// adopt-preserved-head), where the same change reappears under a different
// commit SHA.
type PatchRetention struct {
	// Comparable reports whether patch identity could decide the comparison at
	// all. The private range must consist entirely of unique, ordinary,
	// non-empty patches. Structural live commits are permitted only when their
	// parent diffs do not touch a path changed by the private range.
	Comparable bool
	// Retained lists private-only commits whose patch identity has one ordered,
	// unreversed match among the live-only commits.
	Retained []string
	// Unretained lists private-only commits no patch identity accounts for:
	// genuinely absent changes when Comparable, unprovable ones otherwise.
	Unretained []string
}

// RetainsAll reports whether the live side accounts for every private-only
// commit by patch identity. It is false whenever the comparison was not
// comparable, so callers get fail-closed semantics without re-deriving them.
func (r PatchRetention) RetainsAll() bool {
	return r.Comparable && len(r.Unretained) == 0
}

// ComparePatchRetention compares the commits exclusive to privateHead against
// the commits exclusive to liveHead by Git patch identity (git patch-id
// --stable, one identity per commit). Commits shared by both histories need no
// comparison: they are retained by object identity.
//
// Matching is asymmetric and fails closed. Every private-only commit must be
// an ordinary, non-empty patch with a unique identity. Empty live commits and
// live structural commits on unrelated paths do not invalidate the proof, nor
// do duplicate extra live patches. Required patches must each have one
// unambiguous match in private history order and must not subsequently be
// reversed. An error is reserved for a comparison that could not be run at all.
func ComparePatchRetention(ctx context.Context, dir, liveHead, privateHead string) (PatchRetention, error) {
	var result PatchRetention
	liveHead = strings.TrimSpace(liveHead)
	privateHead = strings.TrimSpace(privateHead)
	if liveHead == "" || privateHead == "" {
		return result, fmt.Errorf("compare patch retention: both heads are required")
	}
	privateOnly, err := exclusiveCommits(ctx, dir, liveHead, privateHead)
	if err != nil {
		return result, err
	}
	if len(privateOnly) == 0 {
		result.Comparable = true
		return result, nil
	}
	liveOnly, err := exclusiveCommits(ctx, dir, privateHead, liveHead)
	if err != nil {
		return result, err
	}

	privatePatches, requiredPaths, ok := privatePatchSeries(ctx, dir, privateOnly)
	if !ok {
		result.Unretained = privateOnly
		return result, nil
	}
	livePatches, ok := livePatchSeries(ctx, dir, liveOnly, requiredPaths)
	if !ok {
		result.Unretained = privateOnly
		return result, nil
	}

	positions := make(map[string][]int, len(livePatches))
	for i, patch := range livePatches {
		positions[patch.id] = append(positions[patch.id], i)
	}
	lastPosition := -1
	for _, patch := range privatePatches {
		matches := positions[patch.id]
		if len(matches) > 1 {
			result.Unretained = privateOnly
			return result, nil
		}
		if len(matches) == 1 {
			if matches[0] <= lastPosition {
				result.Unretained = privateOnly
				return result, nil
			}
			lastPosition = matches[0]
		}
	}

	result.Comparable = true
	for _, patch := range privatePatches {
		matches := positions[patch.id]
		if len(matches) == 0 {
			result.Unretained = append(result.Unretained, patch.commit)
			continue
		}
		matchPosition := matches[0]
		retained := true
		for _, later := range livePatches[matchPosition+1:] {
			if later.id == patch.reverseID {
				retained = false
				break
			}
		}
		if retained {
			equal, comparable := pathsEqual(ctx, dir, livePatches[matchPosition].parent, liveHead, patch.paths)
			if !comparable {
				result.Comparable = false
				result.Retained = nil
				result.Unretained = privateOnly
				return result, nil
			}
			retained = !equal
		}
		if retained {
			result.Retained = append(result.Retained, patch.commit)
		} else {
			result.Unretained = append(result.Unretained, patch.commit)
		}
	}
	return result, nil
}

type patchCommit struct {
	commit    string
	parent    string
	id        string
	reverseID string
	paths     map[string]struct{}
}

func privatePatchSeries(ctx context.Context, dir string, commits []string) ([]patchCommit, map[string]struct{}, bool) {
	patches := make([]patchCommit, 0, len(commits))
	requiredPaths := make(map[string]struct{})
	seen := make(map[string]struct{}, len(commits))
	for i := len(commits) - 1; i >= 0; i-- {
		patch, ordinary, empty := inspectPatchCommit(ctx, dir, commits[i])
		if !ordinary || empty || patch.id == "" || patch.reverseID == "" {
			return nil, nil, false
		}
		if _, duplicate := seen[patch.id]; duplicate {
			return nil, nil, false
		}
		seen[patch.id] = struct{}{}
		for path := range patch.paths {
			requiredPaths[path] = struct{}{}
		}
		patches = append(patches, patch)
	}
	return patches, requiredPaths, true
}

func livePatchSeries(ctx context.Context, dir string, commits []string, requiredPaths map[string]struct{}) ([]patchCommit, bool) {
	patches := make([]patchCommit, 0, len(commits))
	for i := len(commits) - 1; i >= 0; i-- {
		patch, ordinary, empty := inspectPatchCommit(ctx, dir, commits[i])
		if empty {
			continue
		}
		if !ordinary || patch.id == "" {
			if pathsOverlap(patch.paths, requiredPaths) {
				return nil, false
			}
			continue
		}
		patches = append(patches, patch)
	}
	return patches, true
}

func inspectPatchCommit(ctx context.Context, dir, commit string) (patchCommit, bool, bool) {
	patch := patchCommit{commit: commit}
	parents, err := Run(ctx, dir, "rev-list", "--parents", "-n", "1", commit)
	if err != nil {
		return patch, false, false
	}
	fields := strings.Fields(parents)
	paths, ok := commitChangedPaths(ctx, dir, commit)
	if !ok {
		return patch, false, false
	}
	patch.paths = paths
	if len(fields) != 2 {
		return patch, false, false
	}
	patch.parent = fields[1]
	forward, err := RunRaw(ctx, dir, "diff", "--no-ext-diff", "--binary", patch.parent, commit)
	if err != nil {
		return patch, true, false
	}
	if len(forward) == 0 {
		return patch, true, true
	}
	patch.id = stablePatchID(ctx, dir, forward)
	reverse, err := RunRaw(ctx, dir, "diff", "--no-ext-diff", "--binary", commit, patch.parent)
	if err == nil {
		patch.reverseID = stablePatchID(ctx, dir, reverse)
	}
	return patch, true, false
}

func stablePatchID(ctx context.Context, dir string, diff []byte) string {
	out, err := RunWithInput(ctx, dir, string(diff), "patch-id", "--stable")
	if err != nil {
		return ""
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

func commitChangedPaths(ctx context.Context, dir, commit string) (map[string]struct{}, bool) {
	out, err := RunRaw(ctx, dir, "diff-tree", "--no-commit-id", "--name-only", "-r", "-z", "--root", "-m", commit)
	if err != nil {
		return nil, false
	}
	paths := make(map[string]struct{})
	for _, path := range strings.Split(string(out), "\x00") {
		if path != "" {
			paths[path] = struct{}{}
		}
	}
	return paths, true
}

func pathsOverlap(left, right map[string]struct{}) bool {
	for path := range left {
		if _, ok := right[path]; ok {
			return true
		}
	}
	return false
}

func pathsEqual(ctx context.Context, dir, left, right string, paths map[string]struct{}) (bool, bool) {
	ordered := make([]string, 0, len(paths))
	for path := range paths {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)
	args := []string{"diff", "--no-ext-diff", "--binary", left, right, "--"}
	args = append(args, ordered...)
	out, err := RunRaw(ctx, dir, args...)
	if err != nil {
		return false, false
	}
	return len(out) == 0, true
}

// exclusiveCommits lists the commits reachable only from head, excluding
// everything reachable from other (the head's side of the symmetric
// difference). Order is git's default reverse-chronological.
func exclusiveCommits(ctx context.Context, dir, other, head string) ([]string, error) {
	out, err := Run(ctx, dir, "rev-list", "--right-only", other+"..."+head)
	if err != nil {
		return nil, fmt.Errorf("list commits exclusive to %s: %w", head, err)
	}
	return strings.Fields(out), nil
}
