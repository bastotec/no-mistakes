package daemon

import (
	"context"
	"fmt"
	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/scm/github"
	"strings"
)

// A distinct RPC fails closed on an old daemon; no gate push or ordinary-run
// fallback can accidentally launch an unconstrained run.
func (m *RunManager) HandleStartContinuationRun(ctx context.Context, p *ipc.StartContinuationRunParams) (string, error) {
	if _, _, err := github.ExistingPRTarget(p.URL); err != nil {
		return "", err
	}
	repo, err := m.db.GetRepo(p.RepoID)
	if err != nil || repo == nil {
		return "", fmt.Errorf("continuation: registered repository unavailable")
	}
	return m.withBranchLock(repo.ID, p.Branch, func() (string, error) {
		active, err := m.db.GetActiveRun(repo.ID, p.Branch)
		if err != nil {
			return "", err
		}
		if active != nil {
			pins, err := active.ContinuationPins()
			if err == nil && pins != nil && pins.URL == p.URL && activeRunCarriesHead(active, p.HeadSHA) {
				return active.ID, nil
			}
			return "", fmt.Errorf("continuation: branch already has a different active run")
		}
		if strings.TrimSpace(p.Intent) == "" {
			return "", fmt.Errorf("intent is required")
		}
		caller := &ipc.StartHistoryRunParams{StartFreshRunParams: ipc.StartFreshRunParams{Branch: p.Branch, HeadSHA: p.HeadSHA}, WorkDir: p.WorkDir}
		if err := validateHistoryCaller(ctx, repo, caller); err != nil {
			return "", err
		}
		gateDir := m.paths.RepoDir(repo.ID)
		svc := branchsync.Service{DB: m.db, Repo: repo, WorkDir: p.WorkDir, GateDir: gateDir, Paths: m.paths}
		state := svc.InspectCached(ctx)
		if state.State == branchsync.StatePipelineOwned || state.State == branchsync.StatePushInProgress {
			return "", fmt.Errorf("continuation: branch custody is not available: %s", state.Safety)
		}
		if err := git.ValidateBareRepository(ctx, gateDir); err != nil {
			return "", err
		}
		if _, err := git.RunBare(ctx, gateDir, "fetch", "--no-tags", "--no-write-fetch-head", "--", p.WorkDir, p.HeadSHA); err != nil {
			return "", err
		}
		if err := validateHistoryCaller(ctx, repo, caller); err != nil {
			return "", err
		}
		return m.startRunWithIntentSourceLocked(ctx, repo, p.Branch, p.HeadSHA, p.HeadSHA, "continuation", nil, p.Intent, db.RunIntentSourceAgent, "", "", "", "", "", p.URL, "")
	})
}
