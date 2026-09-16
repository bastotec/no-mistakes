package steps

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// ErrHistoryConstraint must never be swallowed as a retryable CI fixer failure.
var ErrHistoryConstraint = errors.New("preserve-history")

// errHistoryUnverifiable marks a refusal produced by a read that could not be
// completed rather than by evidence that the pinned relationship was violated.
// Every pre-mutation caller still fails closed on it; only the CI step's
// read-only paths, which mutate nothing and read again shortly, retry it.
var errHistoryUnverifiable = errors.New("unverifiable")

// historyReadUnverifiable reports whether err is an incomplete read rather than
// a violated pin, logging why the caller is going to read again.
func historyReadUnverifiable(sctx *pipeline.StepContext, err error) bool {
	if !errors.Is(err, errHistoryUnverifiable) {
		return false
	}
	if sctx.Log != nil {
		sctx.Log(fmt.Sprintf("warning: %v", err))
	}
	return true
}

func historyRefusal(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrHistoryConstraint, fmt.Sprintf(format, args...))
}

var fullCommitSHA = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// ValidateHistoryBase accepts immutable object IDs only, never moving refs or
// abbreviated SHAs. The caller also supplies an explicit integration branch.
func ValidateHistoryBase(sha string) error {
	if !fullCommitSHA.MatchString(sha) || strings.Trim(sha, "0") == "" {
		return fmt.Errorf("preserve-history requires a full nonzero commit SHA, not a ref or abbreviation")
	}
	return nil
}

func preservesHistory(sctx *pipeline.StepContext) bool {
	return sctx.Run != nil && sctx.Run.PreserveHistoryBaseSHA != nil
}

// historyConstraintRefusal is the CI step's single reading of whether a
// preserve-history run can still be honored, shared by the poll loop and the
// approval-override verifier so both answer the same question the same way.
// Only the GitHub host reads a live PR base, so an empty value is an unknown
// base, not a mismatched one, and an unreadable one is left for the next read
// exactly as the monitor treats the PR state. It mutates nothing, so a refusal
// here is a decision for the operator, not a reason to end the run.
func historyConstraintRefusal(sctx *pipeline.StepContext, host scm.Host, pr *scm.PR) error {
	if !preservesHistory(sctx) {
		return nil
	}
	if reader, ok := host.(scm.PRBaseBranchReader); ok {
		actual, readErr := reader.GetPRBaseBranch(sctx.Ctx, pr)
		if readErr != nil {
			if sctx.Log != nil {
				sctx.Log(fmt.Sprintf("warning: could not verify live PR base: %v", readErr))
			}
		} else {
			pr.BaseBranch = actual
		}
	}
	if pr.BaseBranch != "" && (sctx.Run.PRBaseBranch == nil || pr.BaseBranch != *sctx.Run.PRBaseBranch) {
		return historyRefusal("the live PR base %q is not the pinned integration branch", pr.BaseBranch)
	}
	return AssertHistoryPolicy(sctx)
}

// AssertHistoryPolicy is the pre-operation guard shared by initial integration,
// repair, and publication. It never integrates a moved base: the exact source
// head and base/branch relationship were chosen by the operator. Only additive
// descendants may proceed. A resumed run reads these pins from its durable row.
// This is deliberately independent of auto-fix budgets and rebase.strategy.
func AssertHistoryPolicy(sctx *pipeline.StepContext) error {
	if !preservesHistory(sctx) {
		return nil
	}
	// One bounded read, without changing the caller's invocation context.
	ctx, cancel := context.WithTimeout(sctx.Ctx, fetchUpstreamTimeout)
	defer cancel()
	guard := *sctx
	guard.Ctx = ctx
	sctx = &guard
	run := sctx.Run
	fail := func(reason string) error { return historyRefusal("%s; no rebase or force update is permitted", reason) }
	unverifiable := func(reason string) error { return fmt.Errorf("%w: %w", errHistoryUnverifiable, fail(reason)) }
	if run.SubmittedHeadSHA == nil || ValidateHistoryBase(*run.SubmittedHeadSHA) != nil ||
		ValidateHistoryBase(*run.PreserveHistoryBaseSHA) != nil || run.PRBaseBranch == nil {
		return fail("missing or invalid durable integration pins")
	}
	branch, err := ValidateRunPRBaseBranchName(*run.PRBaseBranch)
	if err != nil || branch == "" || branch == run.Branch {
		return fail("invalid pinned integration branch")
	}
	if _, err := stepGitRun(sctx, "merge-base", "--is-ancestor", *run.PreserveHistoryBaseSHA, *run.SubmittedHeadSHA); err != nil {
		return fail("the pinned base is not an ancestor of the submitted integration head")
	}
	if _, err := stepGitRun(sctx, "merge-base", "--is-ancestor", *run.SubmittedHeadSHA, "HEAD"); err != nil {
		return fail("the working head no longer contains the exact submitted integration head " + *run.SubmittedHeadSHA)
	}
	gitRun := func(args ...string) (string, error) { return stepGitRun(sctx, args...) }
	live, err := lsRemoteSHA(gitRun, resolveUpstreamURL(sctx), "refs/heads/"+branch)
	if err != nil {
		return unverifiable(fmt.Sprintf("cannot verify pinned base %s: %v", branch, err))
	}
	if live != *run.PreserveHistoryBaseSHA {
		return fail(fmt.Sprintf("integration base %s moved: pinned %s, live %s; create and validate a new integration explicitly", branch, *run.PreserveHistoryBaseSHA, live))
	}
	return nil
}

// historyPushDecision never even considers lease or patch-equivalence escape
// paths. The server enforces append-only publication with an ordinary push,
// including a race after this observation. Missing ancestry evidence refuses.
func historyPushDecision(sctx *pipeline.StepContext, url, ref, head string) (forcePushDecision, error) {
	ctx, cancel := context.WithTimeout(sctx.Ctx, fetchUpstreamTimeout)
	defer cancel()
	guard := *sctx
	guard.Ctx = ctx
	sctx = &guard
	gitRun := func(args ...string) (string, error) { return stepGitRun(sctx, args...) }
	remote, err := lsRemoteSHA(gitRun, url, ref)
	if err != nil {
		return forcePushDecision{}, fmt.Errorf("%w: %w", errHistoryUnverifiable, historyRefusal("verify publication target: %v", err))
	}
	if remote == "" {
		return forcePushDecision{newBranch: true}, nil
	}
	if remote == head {
		return forcePushDecision{remoteSHA: remote, upToDate: true}, nil
	}
	if _, err := gitRun("merge-base", "--is-ancestor", remote, head); err != nil {
		return forcePushDecision{}, historyRefusal("remote head %s is not a verified ancestor of %s; refusing a force or force-with-lease update", remote, head)
	}
	return forcePushDecision{remoteSHA: remote, fastForward: true}, nil
}
