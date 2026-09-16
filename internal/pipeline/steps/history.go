package steps

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// ErrHistoryConstraint must never be swallowed as a retryable CI fixer failure.
var ErrHistoryConstraint = errors.New("preserve-history")

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
		return fail(fmt.Sprintf("cannot verify pinned base %s: %v", branch, err))
	}
	if live != *run.PreserveHistoryBaseSHA {
		return fail(fmt.Sprintf("integration base %s moved: pinned %s, live %s; create and validate a new integration explicitly", branch, *run.PreserveHistoryBaseSHA, live))
	}
	head, err := stepGitHeadSHA(sctx)
	if err != nil {
		return fail(fmt.Sprintf("cannot read the current head: %v", err))
	}
	_, err = historyPushDecision(sctx, resolvePushURL(sctx), normalizedBranchRef(run.Branch), head)
	return err
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
		return forcePushDecision{}, historyRefusal("verify publication target: %v", err)
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
