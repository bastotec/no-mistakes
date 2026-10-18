package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
)

// HandleRecoverPublication owns the same branch lock as launches. It does not
// revive a terminal run or spend repair authority. The only run write belongs
// to ordinary PR publication (its PR URL); validation evidence remains immutable.
func (m *RunManager) HandleRecoverPublication(ctx context.Context, p *ipc.RecoverPublicationParams) (*ipc.RecoverPublicationResult, error) {
	run, err := m.db.GetRun(p.RunID)
	if err != nil {
		return nil, err
	}
	if run == nil || run.RepoID != p.RepoID {
		return nil, fmt.Errorf("publication recovery refused: run not found in this repository")
	}
	var result *ipc.RecoverPublicationResult
	_, err = m.withBranchLock(run.RepoID, run.Branch, func() (runID string, returnErr error) {
		run, err = m.db.GetRun(p.RunID)
		if err != nil {
			return "", err
		}
		repo, err := m.db.GetRepo(run.RepoID)
		if err != nil {
			return "", err
		}
		if repo == nil {
			return "", fmt.Errorf("publication recovery refused: repository missing")
		}
		sctx := &pipeline.StepContext{Ctx: ctx, Run: run, Repo: repo, DB: m.db, Log: func(string) {}, LogChunk: func(string) {}}
		if _, err := steps.ValidatePublicationRecoveryRecords(sctx); err != nil {
			return "", err
		}
		active, err := m.db.GetActiveRun(repo.ID, run.Branch)
		if err != nil {
			return "", err
		}
		if active != nil {
			return "", fmt.Errorf("publication recovery refused: branch has an active run")
		}
		if m.shuttingDown.Load() {
			return "", fmt.Errorf("daemon is shutting down")
		}
		gateDir := m.paths.RepoDir(repo.ID)
		head, err := git.ResolveRef(ctx, gateDir, run.HeadSHA)
		if err != nil || head != run.HeadSHA {
			return "", fmt.Errorf("publication recovery refused: recorded source head is missing")
		}
		// Disposable detached checkout: never move the caller, private branch, or
		// original run's worktree. No push, receive hook, or new full run is launched.
		parent, err := os.MkdirTemp(m.paths.Root(), "publication-recovery-")
		if err != nil {
			return "", err
		}
		dir := filepath.Join(parent, "worktree")
		if err := git.WorktreeAdd(ctx, gateDir, dir, run.HeadSHA); err != nil {
			return "", errors.Join(err, os.RemoveAll(parent))
		}
		defer func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := git.WorktreeRemove(cleanup, gateDir, dir); err != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("remove publication recovery worktree %s: %w", dir, err))
				return
			}
			returnErr = errors.Join(returnErr, os.RemoveAll(parent))
		}()
		sctx.WorkDir, sctx.GateDir = dir, gateDir
		cfg, err := m.loadRecoveredConfig(ctx, run, repo, dir)
		if err != nil {
			return "", err
		}
		sctx.Config = cfg
		forge, err := forgecontext.Resolve(ctx, cfg.ForgeProfiles, repo.UpstreamURL, repo.ForkURL)
		if err != nil {
			return "", err
		}
		sctx.ForgeContext = forge
		ag, err := newPipelineAgent(ctx, cfg, m.paths.EvidenceRoot(cfg.Test.Evidence.LocalRoot), exec.LookPath, forgeEnvironment(forge))
		if err != nil {
			return "", err
		}
		defer ag.Close()
		sctx.Agent = ag
		sctx.EvidenceDir = m.paths.RunEvidenceDir(cfg.Test.Evidence.LocalRoot, run.ID)
		if run.Intent != nil {
			sctx.UserIntent = *run.Intent
		}
		if run.IntentSource != nil {
			sctx.IntentSource = *run.IntentSource
		}
		outcome, err := steps.RecoverPublication(sctx)
		if err != nil {
			return "", err
		}
		if outcome == nil || outcome.Skipped || outcome.PRURL == "" {
			return "", fmt.Errorf("publication recovery refused: ordinary publication did not return a PR")
		}
		result = &ipc.RecoverPublicationResult{RunID: run.ID, PRURL: outcome.PRURL}
		return run.ID, nil
	})
	return result, err
}
