package daemon

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

type documentTimeoutLaunchKey struct{}

// HandleStartDocumentRun launches only after supported branch custody return.
// The separate RPC refuses older daemons without a gate push or fallback.
func (m *RunManager) HandleStartDocumentRun(ctx context.Context, request *ipc.StartDocumentRunParams) (ipc.LaunchReceipt, error) {
	timeout, err := time.ParseDuration(request.DocumentTimeout)
	if err != nil || timeout <= 0 {
		return ipc.LaunchReceipt{}, fmt.Errorf("document-launch: timeout must be a positive Go duration")
	}
	p := &request.StartHistoryRunParams
	if p.PreserveHistoryBaseSHA != "" || len(p.SkipSteps) != 0 {
		return ipc.LaunchReceipt{}, fmt.Errorf("document-launch: history pins and skipped stages are not supported")
	}
	if err := validateLaunchNonce(p.LaunchNonce); err != nil {
		return ipc.LaunchReceipt{}, err
	}
	if err := validateValidationGeneration(p.ValidationGeneration); err != nil {
		return ipc.LaunchReceipt{}, err
	}
	base, err := normalizeRunPRBaseBranch(p.PRBaseBranch)
	if err != nil {
		return ipc.LaunchReceipt{}, err
	}
	repo, err := m.db.GetRepo(p.RepoID)
	if err != nil || repo == nil {
		return ipc.LaunchReceipt{}, fmt.Errorf("document-launch: registered repository unavailable")
	}
	var receipt ipc.LaunchReceipt
	_, err = m.withBranchLock(repo.ID, p.Branch, func() (string, error) {
		prior, err := m.db.GetRunByLaunchNonce(repo.ID, p.Branch, p.LaunchNonce)
		if err != nil {
			return "", err
		}
		active, err := m.db.GetActiveRun(repo.ID, p.Branch)
		if err != nil {
			return "", err
		}
		existing := prior
		if existing == nil && p.ReuseActive {
			existing = active
		}
		if existing != nil {
			pinned, err := m.db.DocumentTimeout(existing.ID)
			if err != nil {
				return "", err
			}
			matches := pinned == timeout && existing.SubmittedHeadSHA != nil && *existing.SubmittedHeadSHA == p.HeadSHA && existing.Intent != nil && *existing.Intent == p.Intent && launchPRBaseBranchMatches(existing, base)
			if prior != nil {
				matches = matches && prior.LaunchValidationGeneration != nil && *prior.LaunchValidationGeneration == p.ValidationGeneration
			}
			if !matches {
				return "", fmt.Errorf("document-launch: existing receipt/run has different Document timeout, head, intent or base")
			}
			receipt, err = receiptForRun(existing, false)
			return existing.ID, err
		}
		if active != nil {
			return "", fmt.Errorf("document-launch: branch already has an active run; none superseded")
		}
		if strings.TrimSpace(p.Intent) == "" {
			return "", fmt.Errorf("document-launch: intent is required")
		}
		if err := validateHistoryCaller(ctx, repo, p); err != nil {
			return "", fmt.Errorf("document-launch: %w", err)
		}
		gateDir := m.paths.RepoDir(repo.ID)
		svc := branchsync.Service{DB: m.db, Repo: repo, WorkDir: p.WorkDir, GateDir: gateDir, Paths: m.paths}
		state := svc.InspectCached(ctx)
		if state.State == branchsync.StatePipelineOwned || state.State == branchsync.StatePushInProgress {
			return "", fmt.Errorf("document-launch: branch custody unavailable: %s", state.Safety)
		}
		fetchCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		if _, err := git.Run(fetchCtx, gateDir, "fetch", "--no-tags", "--no-write-fetch-head", p.WorkDir, p.HeadSHA); err != nil {
			return "", fmt.Errorf("document-launch: import exact head: %w", err)
		}
		if err := validateHistoryCaller(ctx, repo, p); err != nil {
			return "", err
		}
		if gateHead, exists, err := git.ExactRefTarget(ctx, gateDir, "refs/heads/"+p.Branch); err != nil {
			return "", err
		} else if exists {
			if _, err := git.Run(ctx, gateDir, "merge-base", "--is-ancestor", gateHead, p.HeadSHA); err != nil {
				return "", fmt.Errorf("document-launch: reconcile private gate custody before launch")
			}
		}
		launchCtx := context.WithValue(ctx, documentTimeoutLaunchKey{}, timeout)
		id, err := m.startRunWithIntentSourceLocked(launchCtx, repo, p.Branch, p.HeadSHA, p.HeadSHA, "document-timeout", nil, p.Intent, db.RunIntentSourceAgent, p.LaunchNonce, p.ValidationGeneration, digestIntent(p.Intent), base, "", "", "")
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
