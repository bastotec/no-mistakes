package pipeline

import (
	"fmt"
	"github.com/kunchenguid/no-mistakes/internal/git"
)

// RunPolicyGuard supplies provider-aware checks without making the executor
// depend on a forge adapter. The core Rebase step owns integration policy.
type RunPolicyGuard interface {
	CheckRunPolicy(*StepContext, bool) error
}

// AssertContinuationHistory proves exact ancestry, never patch equivalence.
// Agent tools are trusted, not sandboxed; reject their rewritten result before
// it can become a recorded head or acquire publication authority.
func AssertContinuationHistory(s *StepContext) error {
	p, err := s.Run.ContinuationPins()
	if err != nil || p == nil {
		return err
	}
	if s.Run.SubmittedHeadSHA == nil || s.Run.ExistingPRURL == nil || *s.Run.ExistingPRURL != p.URL || s.Run.PRBaseBranch == nil || *s.Run.PRBaseBranch != p.BaseBranch {
		return fmt.Errorf("continuation: missing or changed durable identity")
	}
	for _, sha := range []string{p.PRHead, p.BaseTip, p.MergeBase, *s.Run.SubmittedHeadSHA} {
		if len(sha) != 40 && len(sha) != 64 {
			return fmt.Errorf("continuation: invalid commit pin")
		}
		if _, err := git.Run(s.Ctx, s.WorkDir, "rev-parse", "--verify", sha+"^{commit}"); err != nil {
			return fmt.Errorf("continuation: unreadable commit pin: %w", err)
		}
	}
	for _, pair := range [][2]string{{p.PRHead, *s.Run.SubmittedHeadSHA}, {*s.Run.SubmittedHeadSHA, "HEAD"}, {s.Run.HeadSHA, "HEAD"}} {
		if _, err := git.Run(s.Ctx, s.WorkDir, "merge-base", "--is-ancestor", pair[0], pair[1]); err != nil {
			return fmt.Errorf("continuation: exact pinned history was rewritten: %w", err)
		}
	}
	if p.PRHead == *s.Run.SubmittedHeadSHA {
		return fmt.Errorf("continuation: submitted head must be an unpublished strict descendant")
	}
	base, err := git.Run(s.Ctx, s.WorkDir, "merge-base", "--all", p.BaseTip, "HEAD")
	if err != nil || base != p.MergeBase {
		return fmt.Errorf("continuation: original base relationship changed")
	}
	return nil
}

func (e *Executor) checkRunPolicy(s *StepContext, allowTerminal bool) error {
	if err := AssertContinuationHistory(s); err != nil {
		return err
	}
	for _, step := range e.steps {
		if guard, ok := step.(RunPolicyGuard); ok {
			if err := guard.CheckRunPolicy(s, allowTerminal); err != nil {
				return err
			}
		}
	}
	return nil
}
