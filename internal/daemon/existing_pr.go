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
			if active.ExistingPRURL != nil && *active.ExistingPRURL == p.URL && activeRunCarriesHead(active, p.HeadSHA) {
				return active.ID, nil
			}
			return "", fmt.Errorf("branch already has an active run; explicit PR target cannot replace it")
		}
		// Only a run that is actually about to start needs the caller's
		// account of the work. Reattaching to the run this call already proved
		// is the same one is the caller observing what is in flight, and the
		// idempotent return above must be reachable without re-supplying it.
		if strings.TrimSpace(p.Intent) == "" {
			return "", fmt.Errorf("intent is required")
		}
		if _, err := git.Run(ctx, repo.WorkingPath, "check-ref-format", "refs/heads/"+p.Branch); err != nil {
			return "", fmt.Errorf("invalid source branch")
		}
		sha, err := git.Run(ctx, repo.WorkingPath, "rev-parse", "--verify", "refs/heads/"+p.Branch+"^{commit}")
		if err != nil || sha == "" || sha != p.HeadSHA {
			return "", fmt.Errorf("submitted head does not match local source branch")
		}
		gateDir := m.paths.RepoDir(repo.ID)
		if err := git.ValidateBareRepository(ctx, gateDir); err != nil {
			return "", fmt.Errorf("validate explicit PR gate: %w", err)
		}
		if _, err := git.RunBare(ctx, gateDir, "fetch", "--no-tags", "--no-write-fetch-head", "--", repo.WorkingPath, sha); err != nil {
			return "", fmt.Errorf("transfer explicit PR submission: %w", err)
		}
		if err := bindExplicitGateBranch(ctx, gateDir, p.Branch, sha); err != nil {
			return "", err
		}
		return m.startRunWithIntentSourceLocked(ctx, repo, p.Branch, sha, sha, "existing-pr", nil, p.Intent, db.RunIntentSourceAgent, "", "", "", "", "", p.URL, "")
	})
}

// HandleRetireExistingPR drops a branch's canonical association, returning the
// pull request it published to, or "" when the branch had none. An active run
// already validated and is publishing to that pull request, so the association
// is retired between runs, not underneath one.
func (m *RunManager) HandleRetireExistingPR(repoID, branch string) (string, error) {
	repo, err := m.db.GetRepo(repoID)
	if err != nil {
		return "", err
	}
	if repo == nil {
		return "", fmt.Errorf("unknown repository")
	}
	return m.withBranchLock(repo.ID, branch, func() (string, error) {
		active, err := m.db.GetActiveRun(repo.ID, branch)
		if err != nil {
			return "", err
		}
		if active != nil {
			return "", fmt.Errorf("branch %s has an active run; finish or abort it before retiring its pull request association", branch)
		}
		return m.db.DeleteBranchPRTarget(repo.ID, branch)
	})
}

// bindExplicitGateBranch points the gate branch at the submitted head, the
// custody anchor every other launch path establishes by pushing. Without it the
// transferred commit is referenced by nothing once the run worktree is gone, so
// rerun and recovery cannot read the head the run validated. It advances the
// branch, never rewrites it: a gate head the submission does not contain is
// left in place and the launch is refused.
func bindExplicitGateBranch(ctx context.Context, gateDir, branch, head string) error {
	ref := "refs/heads/" + branch
	current, err := git.RunBare(ctx, gateDir, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	current = strings.TrimSpace(current)
	if err != nil || current == "" {
		current = strings.Repeat("0", len(head))
	} else if current == head {
		return nil
	} else if _, err := git.RunBare(ctx, gateDir, "merge-base", "--is-ancestor", current, head); err != nil {
		return fmt.Errorf("gate branch %s is at %s, which the submitted head does not contain; reconcile the branch before an explicit PR run", branch, current)
	}
	if _, err := git.RunBare(ctx, gateDir, "update-ref", "--no-deref", ref, head, current); err != nil {
		return fmt.Errorf("bind explicit PR submission to gate branch: %w", err)
	}
	return nil
}

func explicitRunTarget(run *db.Run) string {
	if run == nil || run.ExistingPRURL == nil {
		return ""
	}
	return *run.ExistingPRURL
}

// activeRunCarriesHead reports whether a head identifies this run to its own
// caller. The submitted head is frozen at launch while `head_sha` advances on
// every rebase, auto-fix commit and published repair, so an operator who
// fetched the pipeline's own commits before reattaching arrives with the
// later one. Both are the same run, and refusing the current head would refuse
// the documented reattach form.
func activeRunCarriesHead(run *db.Run, head string) bool {
	if run == nil {
		return false
	}
	return run.HeadSHA == head || (run.SubmittedHeadSHA != nil && *run.SubmittedHeadSHA == head)
}
