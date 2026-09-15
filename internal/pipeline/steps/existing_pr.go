package steps

import (
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/github"
)

func existingPRURL(sctx *pipeline.StepContext) string {
	if sctx == nil || sctx.Run == nil || sctx.Run.ExistingPRURL == nil {
		return ""
	}
	return *sctx.Run.ExistingPRURL
}

// ValidateExistingPR is shared by launch, pre-push and publication. An explicit
// target is a constraint, not a suggestion: unavailable validation never skips.
// The returned host is the validated one, so callers never rebuild it and can
// never reach it without the validation that proved the target.
func ValidateExistingPR(sctx *pipeline.StepContext, head string) (scm.Host, *scm.PR, error) {
	host, pr, err := ValidateExistingPRIdentity(sctx)
	if err != nil || pr == nil {
		return host, pr, err
	}
	if head == "" || pr.HeadSHA != head {
		return nil, nil, fmt.Errorf("explicit PR head does not match expected head %s", head)
	}
	return host, pr, nil
}

// ValidateExistingPRIdentity proves the association alone: the target is an
// open pull request in its repository whose source is this run's push
// repository and branch. A run that is about to publish a new head has nothing
// to compare the live head against yet, so only the identity is required.
func ValidateExistingPRIdentity(sctx *pipeline.StepContext) (scm.Host, *scm.PR, error) {
	raw := existingPRURL(sctx)
	if raw == "" {
		return nil, nil, nil
	}
	if sctx.Run.PRURL == nil || *sctx.Run.PRURL != raw {
		return nil, nil, fmt.Errorf("explicit PR differs from persisted publication identity")
	}
	host, reason := buildHost(sctx, resolvedProvider(sctx))
	if host == nil {
		return nil, nil, fmt.Errorf("validate explicit PR: %s", reason)
	}
	gh, ok := host.(*github.Host)
	if !ok {
		return nil, nil, fmt.Errorf("explicit PR is supported only for GitHub")
	}
	if err := host.Available(sctx.Ctx); err != nil {
		return nil, nil, fmt.Errorf("validate explicit PR: %w", err)
	}
	pushURL := resolvePushURL(sctx)
	if scm.ResolveHost(sctx.Ctx, pushURL) != "github.com" {
		return nil, nil, fmt.Errorf("explicit PR source must be a github.com push repository")
	}
	pr, err := gh.ValidateExistingPRIdentity(sctx.Ctx, raw, github.RepoSlug(pushURL), sctx.Run.Branch)
	if err != nil {
		return nil, nil, err
	}
	return host, pr, nil
}

// explicitTargetRepo is the owner/repo the run's pull request lives in, or ""
// for a run with no association.
func explicitTargetRepo(sctx *pipeline.StepContext) string {
	target := existingPRURL(sctx)
	if target == "" {
		return ""
	}
	repo, _, err := github.ExistingPRTarget(target)
	if err != nil {
		return ""
	}
	return repo
}

// integrationBranchLabel names the branch a run integrates with the way an
// operator reads it: an associated run integrates with a branch in the pull
// request's repository, which is not the fork "origin" points at.
func integrationBranchLabel(sctx *pipeline.StepContext, branch string) string {
	if repo := explicitTargetRepo(sctx); repo != "" {
		return repo + ":" + branch
	}
	return "origin/" + branch
}
