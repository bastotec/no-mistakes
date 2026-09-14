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
func ValidateExistingPR(sctx *pipeline.StepContext, head string) (*scm.PR, error) {
	raw := existingPRURL(sctx)
	if raw == "" {
		return nil, nil
	}
	if sctx.Run.PRURL == nil || *sctx.Run.PRURL != raw {
		return nil, fmt.Errorf("explicit PR differs from persisted publication identity")
	}
	host, reason := buildHost(sctx, resolvedProvider(sctx))
	if host == nil {
		return nil, fmt.Errorf("validate explicit PR: %s", reason)
	}
	gh, ok := host.(*github.Host)
	if !ok {
		return nil, fmt.Errorf("explicit PR is supported only for GitHub")
	}
	if err := host.Available(sctx.Ctx); err != nil {
		return nil, fmt.Errorf("validate explicit PR: %w", err)
	}
	pushURL := resolvePushURL(sctx)
	if scm.ResolveHost(sctx.Ctx, pushURL) != "github.com" {
		return nil, fmt.Errorf("explicit PR source must be a github.com push repository")
	}
	return gh.ValidateExistingPR(sctx.Ctx, raw, github.RepoSlug(pushURL), sctx.Run.Branch, head)
}
