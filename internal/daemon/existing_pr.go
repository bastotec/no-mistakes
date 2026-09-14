package daemon

import (
	"context"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/scm/github"
)

// HandleStartExistingPRRun transfers the caller's exact committed branch into
// the private object store without firing the ordinary discovery hook. No ref
// is replaced; publication still uses the shared mirror/lease safety protocol.
func (m *RunManager) HandleStartExistingPRRun(ctx context.Context, p *ipc.StartExistingPRRunParams) (string, error) {
	if _, _, err := github.ExistingPRTarget(p.URL); err != nil {
		return "", err
	}
	if strings.TrimSpace(p.Intent) == "" {
		return "", fmt.Errorf("intent is required")
	}
	repo, err := m.db.GetRepo(p.RepoID)
	if err != nil {
		return "", err
	}
	if repo == nil {
		return "", fmt.Errorf("unknown repository")
	}
	if p.Branch == repo.DefaultBranch {
		return "", fmt.Errorf("explicit PR submission requires a non-default source branch")
	}
	return m.withBranchLock(repo.ID, p.Branch, func() (string, error) {
		active, err := m.db.GetActiveRun(repo.ID, p.Branch)
		if err != nil {
			return "", err
		}
		if active != nil {
			if active.ExistingPRURL != nil && *active.ExistingPRURL == p.URL && active.SubmittedHeadSHA != nil && *active.SubmittedHeadSHA == p.HeadSHA {
				return active.ID, nil
			}
			return "", fmt.Errorf("branch already has an active run; explicit PR target cannot replace it")
		}
		if _, err := git.Run(ctx, repo.WorkingPath, "check-ref-format", "refs/heads/"+p.Branch); err != nil {
			return "", fmt.Errorf("invalid source branch")
		}
		sha, err := git.Run(ctx, repo.WorkingPath, "rev-parse", "--verify", "refs/heads/"+p.Branch+"^{commit}")
		if err != nil || sha == "" || sha != p.HeadSHA {
			return "", fmt.Errorf("submitted head does not match local source branch")
		}
		if _, err := git.Run(ctx, m.paths.RepoDir(repo.ID), "fetch", "--no-tags", "--no-write-fetch-head", "--", repo.WorkingPath, sha); err != nil {
			return "", fmt.Errorf("transfer explicit PR submission: %w", err)
		}
		return m.startRunWithIntentSourceLocked(ctx, repo, p.Branch, sha, "", "existing-pr", nil, p.Intent, db.RunIntentSourceAgent, "", "", "", "", "", p.URL)
	})
}

func explicitRunTarget(run *db.Run) string {
	if run == nil || run.ExistingPRURL == nil {
		return ""
	}
	return *run.ExistingPRURL
}
