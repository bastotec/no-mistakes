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
	"github.com/kunchenguid/no-mistakes/internal/testisolation"
)

type isolatedTestLaunchKey struct{}

type confinedRunLaunchKey struct{}

// HandleStartIsolatedTestRun imports the exact invoking head without firing a
// receive hook. Old daemons reject this separate RPC before any unconstrained
// run can be launched. Reusing StartHistoryRunParams is wire structure only;
// this launch does not opt into preserve-history or its integration policies.
func (m *RunManager) HandleStartIsolatedTestRun(ctx context.Context, p *ipc.StartHistoryRunParams) (ipc.LaunchReceipt, error) {
	return m.startConfinedLaunch(ctx, p, false)
}

// HandleStartConfinedRun launches a run pinned to the whole-run confinement
// policy: every confinable step executes inside the isolation worker under one
// versioned policy digest bound to the run and its submitted head, and the
// offline policy's forbidden steps (push/pr/ci, jev) skip with an explicit
// reason instead of using daemon credentials. Distinct RPC for the same reason
// as Test isolation: an older daemon must refuse rather than launch an
// unconfined run.
func (m *RunManager) HandleStartConfinedRun(ctx context.Context, p *ipc.StartHistoryRunParams) (ipc.LaunchReceipt, error) {
	return m.startConfinedLaunch(ctx, p, true)
}

// startConfinedLaunch is the shared body of the two isolation launches. The
// boolean selects which pin the run gets; everything else - head import,
// custody preflight, intent requirement, nonce handling - is deliberately
// identical so the two launches can never drift into different safety shapes.
func (m *RunManager) startConfinedLaunch(ctx context.Context, p *ipc.StartHistoryRunParams, wholeRun bool) (ipc.LaunchReceipt, error) {
	refusalPrefix := "test-isolation-launch"
	if wholeRun {
		refusalPrefix = "run-confinement-launch"
	}
	if err := testisolation.Check(); err != nil {
		return ipc.LaunchReceipt{}, fmt.Errorf("%s: %w", refusalPrefix, err)
	}
	if _, err := testisolation.PostgresRoot(); err != nil {
		return ipc.LaunchReceipt{}, fmt.Errorf("%s: %w", refusalPrefix, err)
	}
	if err := testisolation.ModelConfigFromEnvironment().Validate(); err != nil {
		return ipc.LaunchReceipt{}, fmt.Errorf("%s: %w", refusalPrefix, err)
	}
	if p.PreserveHistoryBaseSHA != "" || len(p.SkipSteps) != 0 {
		return ipc.LaunchReceipt{}, fmt.Errorf("%s: history pins and skipped stages are not supported", refusalPrefix)
	}
	if err := validateLaunchNonce(p.LaunchNonce); err != nil {
		return ipc.LaunchReceipt{}, fmt.Errorf("%s: %w", refusalPrefix, err)
	}
	if err := validateValidationGeneration(p.ValidationGeneration); err != nil {
		return ipc.LaunchReceipt{}, fmt.Errorf("%s: %w", refusalPrefix, err)
	}
	base, err := normalizeRunPRBaseBranch(p.PRBaseBranch)
	if err != nil {
		return ipc.LaunchReceipt{}, err
	}
	repo, err := m.db.GetRepo(p.RepoID)
	if err != nil || repo == nil {
		return ipc.LaunchReceipt{}, fmt.Errorf("%s: registered repository unavailable", refusalPrefix)
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
			enabled, err := m.db.RunConfinementEnabled(existing.ID)
			if err != nil {
				return "", err
			}
			isolated, err := m.db.TestIsolationEnabled(existing.ID)
			if err != nil {
				return "", err
			}
			pinned := isolated
			if wholeRun {
				pinned = enabled
			}
			matches := pinned && existing.SubmittedHeadSHA != nil && *existing.SubmittedHeadSHA == p.HeadSHA && existing.Intent != nil && *existing.Intent == p.Intent && launchPRBaseBranchMatches(existing, base)
			if prior != nil {
				matches = matches && prior.LaunchValidationGeneration != nil && *prior.LaunchValidationGeneration == p.ValidationGeneration
			}
			if !matches {
				return "", fmt.Errorf("%s: existing receipt/run has different isolation, head, intent or base", refusalPrefix)
			}
			receipt, err = receiptForRun(existing, false)
			return existing.ID, err
		}
		if active != nil {
			return "", fmt.Errorf("%s: branch already has an active run; none superseded", refusalPrefix)
		}
		if strings.TrimSpace(p.Intent) == "" {
			return "", fmt.Errorf("%s: intent is required", refusalPrefix)
		}
		if err := validateHistoryCaller(ctx, repo, p); err != nil {
			return "", fmt.Errorf("%s: %w", refusalPrefix, err)
		}
		gateDir := m.paths.RepoDir(repo.ID)
		svc := branchsync.Service{DB: m.db, Repo: repo, WorkDir: p.WorkDir, GateDir: gateDir, Paths: m.paths}
		state := svc.InspectCached(ctx)
		if state.State == branchsync.StatePipelineOwned || state.State == branchsync.StatePushInProgress {
			return "", fmt.Errorf("%s: branch custody unavailable: %s", refusalPrefix, state.Safety)
		}
		fetchCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		if _, err := git.Run(fetchCtx, gateDir, "fetch", "--no-tags", "--no-write-fetch-head", p.WorkDir, p.HeadSHA); err != nil {
			return "", fmt.Errorf("%s: import exact head: %w", refusalPrefix, err)
		}
		if err := validateHistoryCaller(ctx, repo, p); err != nil {
			return "", err
		}
		if gateHead, exists, err := git.ExactRefTarget(ctx, gateDir, "refs/heads/"+p.Branch); err != nil {
			return "", err
		} else if exists {
			if _, err := git.Run(ctx, gateDir, "merge-base", "--is-ancestor", gateHead, p.HeadSHA); err != nil {
				return "", fmt.Errorf("%s: reconcile private gate custody before launch", refusalPrefix)
			}
		}
		launchCtx := ctx
		source := "isolated-test"
		if wholeRun {
			launchCtx = context.WithValue(ctx, confinedRunLaunchKey{}, true)
			source = "confined-run"
		} else {
			launchCtx = context.WithValue(ctx, isolatedTestLaunchKey{}, true)
		}
		id, err := m.startRunWithIntentSourceLocked(launchCtx, repo, p.Branch, p.HeadSHA, p.HeadSHA, source, nil, p.Intent, db.RunIntentSourceAgent, p.LaunchNonce, p.ValidationGeneration, digestIntent(p.Intent), base, "", "", "")
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
