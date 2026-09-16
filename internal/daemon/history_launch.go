package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
)

// HandleStartHistoryRun uses the same branch lock, executor and durable run
// lifecycle as ordinary launches, but imports an exact local commit rather than
// pushing through a potentially older post-receive hook. The distinct RPC is
// mandatory: silently ignoring a new JSON field on an old daemon is unsafe.
func (m *RunManager) HandleStartHistoryRun(ctx context.Context, p *ipc.StartHistoryRunParams) (ipc.LaunchReceipt, error) {
	if err := steps.ValidateHistoryBase(p.PreserveHistoryBaseSHA); err != nil {
		return ipc.LaunchReceipt{}, err
	}
	if err := steps.ValidateHistoryBase(p.HeadSHA); err != nil {
		return ipc.LaunchReceipt{}, err
	}
	base, err := normalizeRunPRBaseBranch(p.PRBaseBranch)
	if err != nil || base == "" || base == p.Branch {
		return ipc.LaunchReceipt{}, fmt.Errorf("preserve-history requires an explicit, distinct --base-branch")
	}
	if len(p.SkipSteps) != 0 {
		return ipc.LaunchReceipt{}, fmt.Errorf("preserve-history does not permit skipped stages")
	}
	if err := validateLaunchNonce(p.LaunchNonce); err != nil {
		return ipc.LaunchReceipt{}, err
	}
	if err := validateValidationGeneration(p.ValidationGeneration); err != nil {
		return ipc.LaunchReceipt{}, err
	}
	repo, err := m.db.GetRepo(p.RepoID)
	if err != nil || repo == nil {
		return ipc.LaunchReceipt{}, fmt.Errorf("preserve-history: registered repository unavailable")
	}
	var receipt ipc.LaunchReceipt
	_, err = m.withBranchLock(repo.ID, p.Branch, func() (string, error) {
		// Replay never re-pins the submitted head, even after additive fixes.
		prior, err := m.db.GetRunByLaunchNonce(repo.ID, p.Branch, p.LaunchNonce)
		if err != nil {
			return "", err
		}
		if prior != nil {
			if !historyRequestMatches(prior, p, base) || prior.Intent == nil || *prior.Intent != p.Intent ||
				prior.LaunchValidationGeneration == nil || *prior.LaunchValidationGeneration != p.ValidationGeneration ||
				prior.SubmittedHeadSHA == nil || *prior.SubmittedHeadSHA != p.HeadSHA {
				return "", fmt.Errorf("preserve-history: conflicting launch receipt; immutable pins or intent differ")
			}
			receipt, err = receiptForRun(prior, false)
			return prior.ID, err
		}
		active, err := m.db.GetActiveRun(repo.ID, p.Branch)
		if err != nil {
			return "", err
		}
		if active != nil {
			if !p.ReuseActive {
				return "", fmt.Errorf("preserve-history: branch already has an active run; a new strict launch nonce cannot supersede it")
			}
			if !historyRequestMatches(active, p, base) || (strings.TrimSpace(p.Intent) != "" && (active.Intent == nil || *active.Intent != p.Intent)) {
				return "", fmt.Errorf("preserve-history: branch already has an active run with different integration pins; no run superseded")
			}
			receipt, err = receiptForRun(active, false)
			return active.ID, err
		}
		if strings.TrimSpace(p.Intent) == "" {
			return "", fmt.Errorf("intent is required for a new preserve-history run")
		}
		if err := validateHistoryCaller(ctx, repo, p); err != nil {
			return "", err
		}
		gateDir := m.paths.RepoDir(repo.ID)
		svc := branchsync.Service{DB: m.db, Repo: repo, WorkDir: p.WorkDir, GateDir: gateDir, Paths: m.paths}
		state := svc.InspectCached(ctx)
		if state.State == branchsync.StatePipelineOwned || state.State == branchsync.StatePushInProgress {
			return "", fmt.Errorf("preserve-history: branch custody is not available: %s", state.Safety)
		}
		pinned := &db.Run{Branch: p.Branch, SubmittedHeadSHA: &p.HeadSHA, PRBaseBranch: &base, PreserveHistoryBaseSHA: &p.PreserveHistoryBaseSHA}
		if err := steps.AssertHistoryPolicy(&pipeline.StepContext{Ctx: ctx, Repo: repo, Run: pinned, WorkDir: p.WorkDir}); err != nil {
			return "", err
		}
		// Fetching objects from this verified local checkout invokes no receive
		// hook and moves no branch. The new run worktree immediately roots them;
		// terminalization's ordinary recovery anchor preserves failures as well.
		fetchCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		if _, err := git.Run(fetchCtx, gateDir, "fetch", "--no-tags", "--no-write-fetch-head", p.WorkDir, p.HeadSHA); err != nil {
			return "", fmt.Errorf("preserve-history: import submitted commit: %w", err)
		}
		if err := validateHistoryCaller(ctx, repo, p); err != nil {
			return "", err
		}
		if gateHead, exists, err := git.ExactRefTarget(ctx, gateDir, "refs/heads/"+p.Branch); err != nil {
			return "", err
		} else if exists {
			if _, err := git.Run(ctx, gateDir, "merge-base", "--is-ancestor", gateHead, p.HeadSHA); err != nil {
				return "", fmt.Errorf("preserve-history: private gate branch contains work absent from the submitted head; reconcile custody first")
			}
		}
		id, err := m.startRunWithIntentSourceLocked(ctx, repo, p.Branch, p.HeadSHA, p.HeadSHA, "history", nil,
			p.Intent, db.RunIntentSourceAgent, p.LaunchNonce, p.ValidationGeneration, digestIntent(p.Intent), base, "", p.PreserveHistoryBaseSHA)
		if err != nil {
			return "", err
		}
		run, err := m.db.GetRun(id)
		if err != nil {
			return "", err
		}
		receipt, err = receiptForRun(run, true)
		return id, err
	})
	return receipt, err
}

func historyRequestMatches(run *db.Run, p *ipc.StartHistoryRunParams, base string) bool {
	return run.PreserveHistoryBaseSHA != nil && *run.PreserveHistoryBaseSHA == p.PreserveHistoryBaseSHA &&
		run.PRBaseBranch != nil && *run.PRBaseBranch == base && run.SubmittedHeadSHA != nil &&
		*run.SubmittedHeadSHA == p.HeadSHA
}

func validateHistoryCaller(ctx context.Context, repo *db.Repo, p *ipc.StartHistoryRunParams) error {
	if !filepath.IsAbs(p.WorkDir) {
		return fmt.Errorf("preserve-history: invoking worktree must be absolute")
	}
	root, err := git.FindMainRepoRoot(p.WorkDir)
	if err != nil {
		return fmt.Errorf("preserve-history: cannot resolve invoking worktree: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	registered, pathErr := filepath.EvalSymlinks(repo.WorkingPath)
	if err != nil || pathErr != nil || root != registered {
		return fmt.Errorf("preserve-history: invoking worktree is not part of the registered repository")
	}
	branch, err := git.CurrentBranch(ctx, p.WorkDir)
	if err != nil || branch != p.Branch || branch == repo.DefaultBranch || branch == "HEAD" {
		return fmt.Errorf("preserve-history: invoking feature branch changed")
	}
	head, err := git.HeadSHA(ctx, p.WorkDir)
	if err != nil || head != p.HeadSHA {
		return fmt.Errorf("preserve-history: invoking head changed")
	}
	status, err := git.Run(ctx, p.WorkDir, "status", "--porcelain", "--untracked-files=all")
	if err != nil || status != "" {
		return fmt.Errorf("preserve-history: invoking worktree must be clean")
	}
	return nil
}
