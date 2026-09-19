package gate

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
)

// StaleBranchReconciliation reports a private gate branch that was archived
// and removed so the caller can submit the live head with an ordinary push.
type StaleBranchReconciliation struct {
	Reconciled   bool
	PreviousHead string
	ArchivedTag  string
}

// StaleBranchPlan is the verdict of a non-mutating stale-branch inspection.
// Planning checks containment or the exact submitted-head policy exception
// without touching any ref, so a caller can decide before it publishes anything;
// applying the plan is the only step that archives and removes the branch.
type StaleBranchPlan struct {
	PreserveDescendantOf string
	Reconcile            bool
	Branch               string
	BranchRef            string
	PreviousHead         string
	ArchiveTag           string
}

// ReconcileStaleBranch plans and immediately applies stale private gate branch
// reconciliation. It removes the branch only after Git proves the live head
// contains all of its content, or under the exact submitted-head exception
// described in docs/src/content/docs/concepts/gate-model.md.
func ReconcileStaleBranch(ctx context.Context, gateDir, workDir, branch, liveHead, runOwnedHead string) (StaleBranchReconciliation, error) {
	plan, err := PlanStaleBranchReconciliation(ctx, gateDir, workDir, branch, liveHead, runOwnedHead)
	if err != nil || !plan.Reconcile {
		return StaleBranchReconciliation{}, err
	}
	return ApplyStaleBranchReconciliation(ctx, gateDir, plan)
}

// PlanStaleBranchReconciliation inspects a private gate branch and reports
// whether it must be archived and removed before the live head can enter
// through an ordinary push. It mutates no ref: outside the run-owned
// exception, an unproven private head is refused before publication.
//
// Rewritten histories require final tree survival (a mechanical 3-way of the
// private head with the live head reproduces the live tree). runOwnedHead is
// a policy exception, not containment evidence: callers must supply only
// heads the pipeline durably recorded for this run, and fresh submissions
// must leave it empty. The contract and rationale are owned by
// docs/src/content/docs/concepts/gate-model.md (Private mirror reconciliation).
func PlanStaleBranchReconciliation(ctx context.Context, gateDir, workDir, branch, liveHead, runOwnedHead string) (StaleBranchPlan, error) {
	return planStaleBranchReconciliation(ctx, gateDir, workDir, branch, liveHead, []string{runOwnedHead}, false)
}

// PlanMirrorPublicationReconciliation is the publication variant: in addition
// to the submitted head, every head this run durably recorded as one of its
// own successful publications (Run.LastPushedSHA) is run-owned. A rebase
// inside the same run legitimately leaves the gate mirror at the run's own
// previously published head while the new head carries the same changes
// conflict-resolved (defect 4, 2026-09-17); the archive tag still preserves
// the superseded head before the mirror moves.
func PlanMirrorPublicationReconciliation(ctx context.Context, gateDir, workDir, branch, liveHead string, runOwnedHeads ...string) (StaleBranchPlan, error) {
	return planStaleBranchReconciliation(ctx, gateDir, workDir, branch, liveHead, runOwnedHeads, true)
}

func planStaleBranchReconciliation(ctx context.Context, gateDir, workDir, branch, liveHead string, runOwnedHeads []string, preserveDescendants bool) (StaleBranchPlan, error) {
	var plan StaleBranchPlan
	branch = strings.TrimSpace(branch)
	liveHead = strings.TrimSpace(liveHead)
	if branch == "" || liveHead == "" {
		return plan, fmt.Errorf("reconcile stale gate branch: branch and live head are required")
	}
	if err := git.ValidateBareRepository(ctx, gateDir); err != nil {
		return plan, fmt.Errorf("reconcile stale gate branch: %w", err)
	}
	if _, err := git.Run(ctx, workDir, "check-ref-format", "--branch", branch); err != nil {
		return plan, fmt.Errorf("reconcile stale gate branch %q: invalid branch name: %w", branch, err)
	}
	resolvedLive, err := git.Run(ctx, workDir, "rev-parse", "--verify", liveHead+"^{commit}")
	if err != nil || resolvedLive != liveHead {
		return plan, fmt.Errorf("reconcile stale gate branch %s: live head %s is not an exact commit", branch, liveHead)
	}
	workDir, err = filepath.Abs(workDir)
	if err != nil {
		return plan, fmt.Errorf("reconcile stale gate branch %s: resolve worktree path: %w", branch, err)
	}
	branchRef := "refs/heads/" + branch
	gateHead, exists, err := git.DirectRefTarget(ctx, gateDir, branchRef)
	if err != nil {
		return plan, fmt.Errorf("inspect private mirror ref %s: %w", branchRef, err)
	}
	if !exists {
		return plan, nil
	}
	archiveTag := "refs/tags/no-mistakes-abandoned/" + branch + "/" + gateHead
	archivedHead, archived, err := git.DirectRefTarget(ctx, gateDir, archiveTag)
	if err != nil {
		return plan, fmt.Errorf("inspect private mirror archive tag %s: %w", archiveTag, err)
	}
	if archived && archivedHead != gateHead {
		return plan, fmt.Errorf("private mirror archive tag %s already points at %s, not %s", archiveTag, archivedHead, gateHead)
	}
	if gateHead == liveHead {
		return plan, nil
	}
	if objectType, err := git.Run(ctx, gateDir, "cat-file", "-t", gateHead); err != nil || objectType != "commit" {
		return plan, fmt.Errorf("private mirror ref %s does not point at a commit", branchRef)
	}
	if err := git.FetchRemoteRef(ctx, gateDir, workDir, liveHead, liveHead); err != nil {
		return plan, fmt.Errorf("stage live head for private mirror reconciliation: %w", err)
	}

	// An ancestor needs no reconciliation: the caller's ordinary push is
	// already a fast-forward and preserves the private head by ancestry.
	if _, err := git.Run(ctx, gateDir, "merge-base", "--is-ancestor", gateHead, liveHead); err == nil {
		return plan, nil
	}
	if preserveDescendants {
		plan.PreserveDescendantOf = liveHead
		if _, err := git.Run(ctx, gateDir, "merge-base", "--is-ancestor", liveHead, gateHead); err == nil {
			return plan, nil
		}
	}
	if !isRunOwnedHead(gateHead, runOwnedHeads) {
		atRiskCommits, err := privateCommitsAbsentFromLive(ctx, gateDir, liveHead, gateHead)
		if err != nil {
			return plan, fmt.Errorf("compare private mirror content for %s: %w", branchRef, err)
		}
		if len(atRiskCommits) > 0 {
			atRisk := make([]string, 0, len(atRiskCommits))
			for _, commit := range atRiskCommits {
				description, describeErr := git.Run(ctx, gateDir, "show", "-s", "--format=%H %s", commit)
				if describeErr != nil {
					return plan, fmt.Errorf("describe at-risk private mirror commit %s: %w", commit, describeErr)
				}
				atRisk = append(atRisk, description)
			}
			return plan, fmt.Errorf(
				"refusing to reconcile private mirror ref %s: %d at-risk commit(s) contain content absent from live head %s: %s",
				branchRef, len(atRisk), liveHead, strings.Join(atRisk, "; "),
			)
		}
	}

	return StaleBranchPlan{
		PreserveDescendantOf: plan.PreserveDescendantOf,
		Reconcile:            true,
		Branch:               branch,
		BranchRef:            branchRef,
		PreviousHead:         gateHead,
		ArchiveTag:           archiveTag,
	}, nil
}

// ApplyStaleBranchReconciliation archives the planned head and then removes the
// branch ref. It revalidates that the branch still points at the exact head the
// plan proved, so a private head that appeared after planning is never deleted.
func ApplyStaleBranchReconciliation(ctx context.Context, gateDir string, plan StaleBranchPlan) (StaleBranchReconciliation, error) {
	var result StaleBranchReconciliation
	if !plan.Reconcile {
		return result, nil
	}
	if plan.BranchRef == "" || plan.PreviousHead == "" || plan.ArchiveTag == "" {
		return result, fmt.Errorf("apply private mirror reconciliation: incomplete plan for %q", plan.Branch)
	}
	if err := git.ValidateBareRepository(ctx, gateDir); err != nil {
		return result, fmt.Errorf("apply private mirror reconciliation: %w", err)
	}
	currentHead, exists, err := git.DirectRefTarget(ctx, gateDir, plan.BranchRef)
	if err != nil {
		return result, fmt.Errorf("inspect private mirror ref %s: %w", plan.BranchRef, err)
	}
	if !exists {
		return result, nil
	}
	if currentHead != plan.PreviousHead {
		if plan.PreserveDescendantOf != "" {
			if _, err := git.Run(ctx, gateDir, "merge-base", "--is-ancestor", plan.PreserveDescendantOf, currentHead); err == nil {
				return result, nil
			}
		}
		return result, fmt.Errorf(
			"private mirror ref %s moved to %s after it was proven stale at %s",
			plan.BranchRef, currentHead, plan.PreviousHead,
		)
	}
	archivedHead, archived, err := git.DirectRefTarget(ctx, gateDir, plan.ArchiveTag)
	if err != nil {
		return result, fmt.Errorf("inspect private mirror archive tag %s: %w", plan.ArchiveTag, err)
	}
	if archived && archivedHead != plan.PreviousHead {
		return result, fmt.Errorf("private mirror archive tag %s already points at %s, not %s", plan.ArchiveTag, archivedHead, plan.PreviousHead)
	}
	if !archived {
		if _, err := git.Run(ctx, gateDir, "update-ref", "--no-deref", plan.ArchiveTag, plan.PreviousHead, strings.Repeat("0", len(plan.PreviousHead))); err != nil {
			return result, fmt.Errorf("archive stale private mirror head %s at %s: %w", plan.PreviousHead, plan.ArchiveTag, err)
		}
	}
	if _, err := git.Run(ctx, gateDir, "update-ref", "--no-deref", "-d", plan.BranchRef, plan.PreviousHead); err != nil {
		return result, fmt.Errorf("delete archived stale private mirror ref %s at %s: %w", plan.BranchRef, plan.PreviousHead, err)
	}
	return StaleBranchReconciliation{Reconciled: true, PreviousHead: plan.PreviousHead, ArchivedTag: plan.ArchiveTag}, nil
}

func RestoreReconciledBranch(ctx context.Context, gateDir, branch string, result StaleBranchReconciliation) error {
	if !result.Reconciled {
		return nil
	}
	if err := git.ValidateBareRepository(ctx, gateDir); err != nil {
		return err
	}
	if !ArchivedHeadRecorded(ctx, gateDir, branch, result.PreviousHead) {
		return fmt.Errorf("restore private mirror %q: archived head %s is unavailable", branch, result.PreviousHead)
	}
	ref := "refs/heads/" + branch
	if _, exists, err := git.DirectRefTarget(ctx, gateDir, ref); err != nil || exists {
		return err
	}
	if _, err := git.Run(ctx, gateDir, "update-ref", "--no-deref", ref, result.PreviousHead, strings.Repeat("0", len(result.PreviousHead))); err != nil {
		if _, exists, readErr := git.DirectRefTarget(ctx, gateDir, ref); readErr == nil && exists {
			return nil
		}
		return fmt.Errorf("restore private mirror %s: %w", ref, err)
	}
	return nil
}

// ArchivedHeadRecorded reports whether head is the exact commit archived for
// branch by a prior reconciliation. It is the gate's own evidence that a
// caller-reported pre-reconciliation head is genuine.
func ArchivedHeadRecorded(ctx context.Context, gateDir, branch, head string) bool {
	branch = strings.TrimSpace(branch)
	head = strings.TrimSpace(head)
	if branch == "" || head == "" {
		return false
	}
	if _, err := git.Run(ctx, gateDir, "check-ref-format", "--branch", branch); err != nil {
		return false
	}
	tag := "refs/tags/no-mistakes-abandoned/" + branch + "/" + head
	archivedHead, archived, err := git.DirectRefTarget(ctx, gateDir, tag)
	if err != nil || !archived || archivedHead != head {
		return false
	}
	objectType, err := git.Run(ctx, gateDir, "cat-file", "-t", head)
	return err == nil && objectType == "commit"
}

// isRunOwnedHead reports whether the mirror head is one of the heads the
// calling pipeline durably recorded for this run (submitted head, or one of
// its own successful publications). An empty candidate never matches.
func isRunOwnedHead(head string, owned []string) bool {
	for _, candidate := range owned {
		if candidate = strings.TrimSpace(candidate); candidate != "" && candidate == head {
			return true
		}
	}
	return false
}

// privateCommitsAbsentFromLive names private-only commits whose content is
// absent from the live head, or the entire private-only range when survival
// cannot be proven. The private-only range is computed once; every later
// question is answered from whole-tree and per-path evidence over exactly
// the paths that range touches, so a rebased live head carrying the whole
// default branch since the merge base never costs a per-commit scan of that
// history.
//
// Two distinct proofs can clear the private side, in order:
//
//  1. Final-tree survival: a mechanical 3-way of liveHead with privateHead
//     completes and reproduces the live tree exactly (merging the private
//     head back in would change nothing). Per-file patch identity is not
//     additionally required: patch ids drift across a rebase whose base
//     moved the context lines around a private hunk even when the replay is
//     clean and the final content identical.
//
// A mirror whose mechanical 3-way conflicts or differs from the live tree
// fails closed with the whole private-only range named at risk. A rebase
// that conflict-resolved the same hunks the live side moved genuinely
// produces that conflict with no content missing - that staleness is not
// provable from content, which is why the pipeline refreshes its own mirror
// after every integration (refreshGateMirrorAfterIntegration) and why
// publication extends the Decision 41-A exception to the head this run
// durably recorded as its own last publication (PlanMirrorPublicationReconciliation).
// A patch-equivalent commit whose content was then discarded (an `-s ours`
// twin) still fails: its live file matches the merge base, so the change is
// provably absent.
func privateCommitsAbsentFromLive(ctx context.Context, repoDir, liveHead, privateHead string) ([]string, error) {
	privateOnly, err := commitList(ctx, repoDir, "--right-only", liveHead+"..."+privateHead)
	if err != nil {
		return nil, err
	}
	if len(privateOnly) == 0 {
		return nil, nil
	}
	mergedTree, mergeErr := git.Run(ctx, repoDir, "merge-tree", "--write-tree", liveHead, privateHead)
	if mergeErr == nil {
		liveTree, err := git.Run(ctx, repoDir, "rev-parse", "--verify", liveHead+"^{tree}")
		if err != nil {
			return nil, err
		}
		if mergedTree == liveTree {
			// Whole-tree survival: merging the private head back into the
			// live head reproduces the live tree exactly, so no private
			// content can be absent.
			return nil, nil
		}
	}
	return privateOnly, nil
}

func commitList(ctx context.Context, repoDir string, args ...string) ([]string, error) {
	out, err := git.Run(ctx, repoDir, append([]string{"rev-list"}, args...)...)
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}
