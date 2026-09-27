package git

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
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

	matchPositions, unique := uniqueOrderedPatchMapping(privatePatches, livePatches)
	if !unique {
		result.Unretained = privateOnly
		return result, nil
	}

	result.Comparable = true
	for i, patch := range privatePatches {
		matchPosition := matchPositions[i]
		if matchPosition < 0 {
			result.Unretained = append(result.Unretained, patch.commit)
			continue
		}
		retained, comparable := patchFootprintRetained(ctx, dir, livePatches[matchPosition].parent, livePatches[matchPosition].commit, liveHead, patch.paths)
		if !comparable {
			result.Comparable = false
			result.Retained = nil
			result.Unretained = privateOnly
			return result, nil
		}
		if retained {
			result.Retained = append(result.Retained, patch.commit)
		} else {
			result.Unretained = append(result.Unretained, patch.commit)
		}
	}
	return result, nil
}

func uniqueOrderedPatchMapping(required, candidates []patchCommit) ([]int, bool) {
	type mappingResult struct {
		matched int
		ways    int
		mapping []int
	}
	type state struct {
		requiredIndex  int
		candidateIndex int
	}
	memo := make(map[state]mappingResult)
	var search func(int, int) mappingResult
	search = func(requiredIndex, candidateIndex int) mappingResult {
		if requiredIndex == len(required) {
			return mappingResult{ways: 1}
		}
		key := state{requiredIndex: requiredIndex, candidateIndex: candidateIndex}
		if cached, ok := memo[key]; ok {
			return cached
		}
		best := search(requiredIndex+1, candidateIndex)
		best.mapping = append([]int{-1}, best.mapping...)
		for i := candidateIndex; i < len(candidates); i++ {
			if candidates[i].id != required[requiredIndex].id {
				continue
			}
			candidate := search(requiredIndex+1, i+1)
			candidate.matched++
			candidate.mapping = append([]int{i}, candidate.mapping...)
			if candidate.matched > best.matched {
				best = candidate
			} else if candidate.matched == best.matched {
				best.ways += candidate.ways
				if best.ways > 2 {
					best.ways = 2
				}
			}
		}
		memo[key] = best
		return best
	}
	best := search(0, 0)
	return best.mapping, best.ways == 1
}

type patchCommit struct {
	commit string
	parent string
	id     string
	paths  map[string]struct{}
}

func privatePatchSeries(ctx context.Context, dir string, commits []string) ([]patchCommit, map[string]struct{}, bool) {
	inspected, ok := inspectPatchCommits(ctx, dir, commits)
	if !ok {
		return nil, nil, false
	}
	patches := make([]patchCommit, 0, len(commits))
	requiredPaths := make(map[string]struct{})
	seen := make(map[string]struct{}, len(commits))
	for i := len(commits) - 1; i >= 0; i-- {
		patch := inspected[i]
		if patch.parent == "" || len(patch.paths) == 0 || patch.id == "" {
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
	inspected, ok := inspectPatchCommits(ctx, dir, commits)
	if !ok {
		return nil, false
	}
	patches := make([]patchCommit, 0, len(commits))
	for i := len(commits) - 1; i >= 0; i-- {
		patch := inspected[i]
		if patch.parent != "" && len(patch.paths) == 0 {
			continue
		}
		if patch.parent == "" || patch.id == "" {
			if pathsOverlap(patch.paths, requiredPaths) {
				return nil, false
			}
			continue
		}
		patches = append(patches, patch)
	}
	return patches, true
}

func inspectPatchCommits(ctx context.Context, dir string, commits []string) ([]patchCommit, bool) {
	if len(commits) == 0 {
		return nil, true
	}
	input := strings.Join(commits, "\n") + "\n"
	metadata, err := RunWithInput(ctx, dir, input, "log", "--stdin", "--no-walk=unsorted", "-m", "--format=%x1e%H%x00%P", "-z", "--name-only")
	if err != nil {
		return nil, false
	}
	byCommit := make(map[string]patchCommit, len(commits))
	for _, record := range strings.Split(metadata, "\x1e") {
		if record == "" {
			continue
		}
		fields := strings.Split(record, "\x00")
		if len(fields) < 2 {
			return nil, false
		}
		commit := fields[0]
		patch := byCommit[commit]
		patch.commit = commit
		parents := strings.Fields(fields[1])
		if len(parents) == 1 {
			patch.parent = parents[0]
		}
		if patch.paths == nil {
			patch.paths = make(map[string]struct{})
		}
		for i, path := range fields[2:] {
			if i == 0 {
				path = strings.TrimPrefix(path, "\n")
			}
			if path != "" {
				patch.paths[path] = struct{}{}
			}
		}
		byCommit[commit] = patch
	}
	patches, err := RunWithInput(ctx, dir, input, "log", "--stdin", "--no-walk=unsorted", "--root", "--format=commit %H", "-p", "--binary", "--no-ext-diff")
	if err != nil {
		return nil, false
	}
	ids, err := RunWithInput(ctx, dir, patches, "patch-id", "--stable")
	if err != nil {
		return nil, false
	}
	for _, line := range strings.Split(ids, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			return nil, false
		}
		patch, exists := byCommit[fields[1]]
		if !exists || patch.id != "" {
			return nil, false
		}
		patch.id = fields[0]
		byCommit[fields[1]] = patch
	}
	result := make([]patchCommit, len(commits))
	for i, commit := range commits {
		patch, exists := byCommit[commit]
		if !exists {
			return nil, false
		}
		result[i] = patch
	}
	return result, true
}

func pathsOverlap(left, right map[string]struct{}) bool {
	for path := range left {
		if _, ok := right[path]; ok {
			return true
		}
	}
	return false
}

type diffFootprint struct {
	lines      map[int]struct{}
	insertions map[int]struct{}
}

var zeroContextHunk = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+`)

func patchFootprintRetained(ctx context.Context, dir, parent, matched, liveHead string, paths map[string]struct{}) (bool, bool) {
	ordered := make([]string, 0, len(paths))
	for path := range paths {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)
	for _, path := range ordered {
		matchedPresent, ok := pathPresent(ctx, dir, matched, path)
		if !ok {
			return false, false
		}
		livePresent, ok := pathPresent(ctx, dir, liveHead, path)
		if !ok {
			return false, false
		}
		if matchedPresent && !livePresent {
			return false, true
		}
		required, ok := changedFootprint(ctx, dir, parent, matched, path)
		if !ok || len(required.lines)+len(required.insertions) == 0 {
			return false, false
		}
		final, ok := changedFootprint(ctx, dir, parent, liveHead, path)
		if !ok {
			return false, false
		}
		for line := range required.lines {
			if _, retained := final.lines[line]; !retained {
				return false, true
			}
		}
		for point := range required.insertions {
			if _, retained := final.insertions[point]; !retained {
				return false, true
			}
		}
	}
	return true, true
}

func pathPresent(ctx context.Context, dir, commit, path string) (bool, bool) {
	out, err := RunRaw(ctx, dir, "ls-tree", "-z", "--name-only", commit, "--", path)
	if err != nil {
		return false, false
	}
	return string(out) == path+"\x00", true
}

func changedFootprint(ctx context.Context, dir, left, right, path string) (diffFootprint, bool) {
	footprint := diffFootprint{lines: make(map[int]struct{}), insertions: make(map[int]struct{})}
	out, err := RunRaw(ctx, dir, "diff", "--no-ext-diff", "--no-renames", "--unified=0", left, right, "--", path)
	if err != nil {
		return footprint, false
	}
	for _, line := range strings.Split(string(out), "\n") {
		match := zeroContextHunk.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		start, err := strconv.Atoi(match[1])
		if err != nil {
			return footprint, false
		}
		count := 1
		if match[2] != "" {
			count, err = strconv.Atoi(match[2])
			if err != nil {
				return footprint, false
			}
		}
		if count == 0 {
			footprint.insertions[start] = struct{}{}
			continue
		}
		for changedLine := start; changedLine < start+count; changedLine++ {
			footprint.lines[changedLine] = struct{}{}
		}
	}
	return footprint, true
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
