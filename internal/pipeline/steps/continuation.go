package steps

import (
	"context"
	"fmt"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

func (s *RebaseStep) CheckRunPolicy(sctx *pipeline.StepContext) error {
	return AssertContinuationPolicy(sctx)
}

// AssertContinuationPolicy revalidates external authority at each step and
// agent boundary, on repair and on publication. It never incorporates a moved
// base or treats a patch-equivalent history as the pinned author history.
func AssertContinuationPolicy(sctx *pipeline.StepContext) error {
	if sctx.Run == nil || sctx.Run.ContinuationJSON == nil {
		return nil
	}
	if err := pipeline.AssertContinuationHistory(sctx); err != nil {
		return historyRefusal("%v", err)
	}
	pins, err := sctx.Run.ContinuationPins()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(sctx.Ctx, fetchUpstreamTimeout)
	defer cancel()
	guard := *sctx
	guard.Ctx = ctx
	sctx = &guard
	_, pr, err := ValidateExistingPRIdentity(sctx)
	if err != nil {
		return historyRefusal("continuation: verify same PR: %v", err)
	}
	expected := pins.PRHead
	if sctx.Run.LastPushedSHA != nil {
		expected = *sctx.Run.LastPushedSHA
	}
	if pr == nil || pr.HeadSHA != expected {
		return historyRefusal("continuation: external PR head changed")
	}
	run := func(args ...string) (string, error) { return stepGitRun(sctx, args...) }
	head, err := lsRemoteSHA(run, resolvePushURL(sctx), normalizedBranchRef(sctx.Run.Branch))
	if err != nil || head != expected {
		return historyRefusal("continuation: source remote differs from the exact run-owned head")
	}
	base, err := lsRemoteSHA(run, "https://github.com/"+explicitTargetRepo(sctx)+".git", "refs/heads/"+pins.BaseBranch)
	if err != nil || base != pins.BaseTip {
		return historyRefusal("continuation: external base tip changed from %s", pins.BaseTip)
	}
	return nil
}

// PrepareContinuation proves admission after the explicit PR and its base
// objects have been read. Main may already be ahead: pin the original merge
// base, not today's main as an integration request.
func PrepareContinuation(sctx *pipeline.StepContext, prHead string) error {
	if prHead == sctx.Run.HeadSHA {
		return fmt.Errorf("continuation requires an unpublished strict descendant")
	}
	if _, err := stepGitRun(sctx, "merge-base", "--is-ancestor", prHead, sctx.Run.HeadSHA); err != nil {
		return fmt.Errorf("continuation requires exact PR-head ancestry: %w", err)
	}
	return nil
}

// ContinuationBaseTip resolves only the just-fetched authoritative PR base.
func ContinuationBaseTip(sctx *pipeline.StepContext, branch string) (string, error) {
	return stepGitRun(sctx, "rev-parse", "--verify", runIntegrationRef(sctx, branch)+"^{commit}")
}
