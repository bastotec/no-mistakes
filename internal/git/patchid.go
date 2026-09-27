package git

import (
	"context"
	"fmt"
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
	// all. It is false - and every private commit reported Unretained - when
	// either exclusive range contains a merge or root commit, an empty commit,
	// a commit whose patch identity cannot be computed, or a patch identity
	// that occurs more than once within its range (an ambiguous duplicate: two
	// commits share one patch identity, so a match cannot identify which one
	// the other side carried). Those cases fail closed and leave the caller's
	// other proofs - or its refusal - in charge.
	Comparable bool
	// Retained lists private-only commits whose patch identity occurs exactly
	// once among the live-only commits.
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
// Matching is per commit and fails closed. A private commit is retained only
// when its patch identity matches exactly one live-only commit's; anything
// ambiguous, structural (merge/root commits, whose combined diffs no single
// patch describes), empty, or uncomputable declines the whole comparison
// rather than guessing, so a genuine content loss can never read as retained.
// An error is reserved for a comparison that could not be run at all.
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
	privateIDs, ok := commitPatchIDs(ctx, dir, privateOnly)
	if !ok {
		result.Unretained = privateOnly
		return result, nil
	}
	liveIDs, ok := commitPatchIDs(ctx, dir, liveOnly)
	if !ok {
		result.Unretained = privateOnly
		return result, nil
	}
	livePatchIDs := make(map[string]struct{}, len(liveIDs))
	for _, id := range liveIDs {
		livePatchIDs[id] = struct{}{}
	}
	result.Comparable = true
	for _, commit := range privateOnly {
		if _, matched := livePatchIDs[privateIDs[commit]]; matched {
			result.Retained = append(result.Retained, commit)
		} else {
			result.Unretained = append(result.Unretained, commit)
		}
	}
	return result, nil
}

// exclusiveCommits lists the commits reachable only from head, excluding
// everything reachable from other (the head's side of the symmetric
// difference). Order is git's default reverse-chronological, which callers use
// only for stable error listings.
func exclusiveCommits(ctx context.Context, dir, other, head string) ([]string, error) {
	out, err := Run(ctx, dir, "rev-list", "--right-only", other+"..."+head)
	if err != nil {
		return nil, fmt.Errorf("list commits exclusive to %s: %w", head, err)
	}
	return strings.Fields(out), nil
}

// commitPatchIDs maps each commit to its stable patch identity. The boolean is
// false when the range cannot be accounted for patch by patch: a non-ordinary
// commit (merge, root), an empty commit, a patch identity computation failure,
// or a duplicate identity within the range. It never guesses past one of those.
func commitPatchIDs(ctx context.Context, dir string, commits []string) (map[string]string, bool) {
	ids := make(map[string]string, len(commits))
	seen := make(map[string]string, len(commits))
	for _, commit := range commits {
		id, ok := commitPatchID(ctx, dir, commit)
		if !ok {
			return nil, false
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, false
		}
		seen[id] = commit
		ids[commit] = id
	}
	return ids, true
}

// commitPatchID computes one commit's stable patch identity. Only an ordinary
// single-parent commit with a non-empty diff has one: a merge or root commit's
// combined change is not a patch, and an empty commit has no change at all, so
// both report ok=false and leave the comparison unprovable.
func commitPatchID(ctx context.Context, dir, commit string) (string, bool) {
	parents, err := Run(ctx, dir, "rev-list", "--parents", "-n", "1", commit)
	if err != nil {
		return "", false
	}
	if fields := strings.Fields(parents); len(fields) != 2 {
		return "", false
	}
	diff, err := RunRaw(ctx, dir, "diff-tree", "--no-commit-id", "-p", commit)
	if err != nil {
		return "", false
	}
	if len(diff) == 0 {
		return "", false
	}
	out, err := RunWithInput(ctx, dir, string(diff), "patch-id", "--stable")
	if err != nil {
		return "", false
	}
	fields := strings.Fields(out)
	if len(fields) == 0 || fields[0] == "" {
		return "", false
	}
	return fields[0], true
}
