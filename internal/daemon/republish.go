package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// republishTimeout bounds one terminal publication continuation. The work is
// one PRStep execution (agent draft + validator + forge write, with the
// existing bounded sanitize-and-retry loop inside it), so it needs the same
// order of budget as an ordinary PR step, not a quick RPC window.
const republishTimeout = 35 * time.Minute

// HandleRepublishPublication continues a TERMINAL run's publication without
// re-running validation. The motivating gap: PRStep's sanitize-and-retry loop
// only fires while an executor owns the run, so a run that ended with an
// exhausted publication refusal had no eligible command at all - the only
// supported continuation was a whole-pipeline rerun, which re-runs
// Intent/Rebase/Review/Test from scratch and can abort again for unrelated
// reasons (verified live: cancelled builder rerun 01M5QFJVN4G5M7CN5ZEATD3AKD).
//
// This path is deliberately NOT a rerun and NOT a live executor: it operates on
// the terminal run record itself (a terminal run has no executor -
// preview-terminal-executor-030), creates a throwaway detached worktree at the
// run's recorded head, and executes the ordinary PR step against the recorded
// step rows. Every guard the ordinary PR step applies still applies:
// assertPipelineHeadContinuity, explicit-target validation, the publication
// validator, redaction, and the bounded sanitize-and-retry loop.
//
// Refusals (all fail closed, none silently fall back):
//   - non-terminal run, or an active run on the branch (a live executor owns it;
//     its own PR step or a rerun is the supported path there)
//   - a run whose recorded head does not match --head, or whose head is absent
//     from the gate (the evidence to publish is gone; custody recovery owns that)
//   - a run whose PR step did not fail: continuing a green or never-attempted
//     publication re-publishes prose for no reason and can mask a real refusal
//   - a preserve-history run (its pins refuse out-of-band publication moves)
//
// It never re-runs a validation step, never invents a pass, never touches run
// status, custody or gate refs, and never pushes: the run's head is already on
// the remote (the Push step completed before PR), so only the PR text is
// written. The sole durable effect is runs.pr_url when publication succeeds and
// the run had none - the same write the ordinary PR step makes.
func (m *RunManager) HandleRepublishPublication(ctx context.Context, p ipc.RepublishPublicationParams) (*ipc.RepublishPublicationResult, error) {
	if m.shuttingDown.Load() {
		return nil, fmt.Errorf("daemon is shutting down")
	}
	run, err := m.db.GetRun(p.RunID)
	if err != nil {
		return nil, fmt.Errorf("get run: %w", err)
	}
	if run == nil || run.RepoID != p.RepoID {
		return nil, fmt.Errorf("run %s does not belong to the requested repository", p.RunID)
	}
	if !run.Status.Terminal() {
		return nil, fmt.Errorf("run %s is %s, not terminal; continue it through its own PR step or a rerun instead", run.ID, run.Status)
	}
	if run.PreserveHistoryBaseSHA != nil {
		return nil, fmt.Errorf("preserve-history run %s pins its own integration; use its documented publication path instead", run.ID)
	}
	recorded := strings.TrimSpace(run.HeadSHA)
	if recorded == "" {
		return nil, fmt.Errorf("run %s has no recorded head to publish", run.ID)
	}
	if p.HeadSHA != recorded {
		return nil, fmt.Errorf("run %s recorded head %s, not %s; refusing to publish evidence the run did not validate", run.ID, shortRepublishSHA(recorded), shortRepublishSHA(p.HeadSHA))
	}
	repo, err := m.db.GetRepo(run.RepoID)
	if err != nil {
		return nil, fmt.Errorf("get repo: %w", err)
	}
	if repo == nil {
		return nil, fmt.Errorf("run %s repository is missing", run.ID)
	}

	// A branch with a live run is owned by that run's executor; publishing its
	// predecessor's prose underneath it would misattribute evidence.
	branchMu := m.branchLock(repo.ID, run.Branch)
	branchMu.Lock()
	defer branchMu.Unlock()
	if active, err := m.db.GetActiveRun(repo.ID, run.Branch); err != nil {
		return nil, fmt.Errorf("check active run: %w", err)
	} else if active != nil {
		return nil, fmt.Errorf("branch %s has active run %s; its executor owns publication", run.Branch, active.ID)
	}

	// Only a run whose PR step actually failed is continuable. A completed PR
	// step already published; a skipped one had no surface; a pending or
	// running row on a terminal run is inconsistent state, not evidence.
	prFailed, stepRows, err := runPRStepFailed(m.db, run.ID)
	if err != nil {
		return nil, err
	}
	if !prFailed {
		return nil, fmt.Errorf("run %s has no failed PR step to continue; nothing to republish", run.ID)
	}

	opCtx, cancel := context.WithTimeout(ctx, republishTimeout)
	defer cancel()

	gateDir := m.paths.RepoDir(repo.ID)
	// The terminal run's own worktree was removed when the run finished, so
	// carve a throwaway detached worktree from the gate bare repo at the exact
	// recorded head. It is removed on every exit path; nothing is committed to
	// it, and it never becomes the run's recorded worktree.
	if _, err := git.Run(opCtx, gateDir, "cat-file", "-e", recorded+"^{commit}"); err != nil {
		return nil, fmt.Errorf("run %s head %s is absent from the gate repository; recover custody before republishing (no-mistakes axi sync --check)", run.ID, shortRepublishSHA(recorded))
	}
	wtDir := filepath.Join(m.paths.WorktreeDir(repo.ID, run.ID), "republish")
	if err := os.MkdirAll(filepath.Dir(wtDir), 0o755); err != nil {
		return nil, fmt.Errorf("prepare worktree parent: %w", err)
	}
	defer func() {
		if err := git.WorktreeRemove(context.Background(), gateDir, wtDir); err != nil {
			slog.Warn("failed to remove republish worktree", "run_id", run.ID, "path", wtDir, "error", err)
		}
	}()
	if err := git.WorktreeAdd(opCtx, gateDir, wtDir, recorded); err != nil {
		return nil, fmt.Errorf("create republish worktree at %s: %w", shortRepublishSHA(recorded), err)
	}
	if err := git.CopyLocalUserIdentity(opCtx, repo.WorkingPath, wtDir); err != nil {
		return nil, fmt.Errorf("configure worktree identity: %w", err)
	}

	// Reuse the recovered-run configuration path: same global+repo+trusted
	// merge and the run's own pinned gates. The worktree is at the recorded
	// head, so the repo-config read is the config that head carried.
	cfg, err := m.loadRecoveredConfig(opCtx, run, repo, wtDir)
	if err != nil {
		return nil, fmt.Errorf("load run configuration: %w", err)
	}
	forgeCtx, err := forgecontext.Resolve(opCtx, cfg.ForgeProfiles, repo.UpstreamURL, repo.ForkURL)
	if err != nil {
		return nil, fmt.Errorf("resolve forge profile: %w", err)
	}
	ag, err := newPipelineAgent(opCtx, cfg, m.paths.EvidenceRoot(cfg.Test.Evidence.LocalRoot), exec.LookPath, forgeEnvironment(forgeCtx))
	if err != nil {
		return nil, err
	}
	defer ag.Close()

	logDir := m.paths.RunLogDir(run.ID)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	logFile, err := os.OpenFile(filepath.Join(logDir, "republish-publication.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open republish log: %w", err)
	}
	defer logFile.Close()
	log := func(message string) {
		fmt.Fprintln(logFile, message)
	}
	log(fmt.Sprintf("terminal publication continuation for run %s at head %s (PR step failed; validation is NOT re-run)", run.ID, recorded))

	sctx := &pipeline.StepContext{
		Ctx:          opCtx,
		Run:          run,
		Repo:         repo,
		WorkDir:      wtDir,
		GateDir:      gateDir,
		Agent:        ag,
		Config:       cfg,
		ForgeContext: forgeCtx,
		DB:           m.db,
		Log:          log,
		LogChunk:     func(string) {},
		LogFile:      func(string) {},
		Shared:       &pipeline.RunShared{},
		EvidenceDir:  m.paths.RunEvidenceDir(cfg.Test.Evidence.LocalRoot, run.ID),
	}
	if run.Intent != nil {
		sctx.UserIntent = *run.Intent
	}
	if run.IntentSource != nil {
		sctx.IntentSource = *run.IntentSource
	}

	outcome, err := (&steps.PRStep{}).Execute(sctx)
	if err != nil {
		log("republish refused or failed: " + safeurl.Redact(err.Error()))
		return nil, err
	}
	result := &ipc.RepublishPublicationResult{
		RunID:   run.ID,
		HeadSHA: recorded,
	}
	for _, sr := range stepRows {
		if sr.StepName != types.StepPR && sr.StepName != types.StepCI {
			result.Steps = append(result.Steps, string(sr.StepName)+":"+string(sr.Status))
		}
	}
	if outcome != nil && outcome.PRURL != "" {
		result.PRURL = outcome.PRURL
		if run.PRURL == nil || strings.TrimSpace(*run.PRURL) == "" {
			// The same durable write the ordinary PR step makes when it creates
			// a PR. Status, custody and gate refs are deliberately untouched:
			// this is a publication continuation, not a run transition.
			if err := m.db.UpdateRunPRURL(run.ID, outcome.PRURL); err != nil {
				return nil, fmt.Errorf("persist PR URL: %w", err)
			}
		}
		log("republish published: " + outcome.PRURL)
	}
	return result, nil
}

// runPRStepFailed reports whether the run's PR step row ended in failure, and
// returns the recorded step rows for the caller's context. A missing PR row
// (older runs) is not continuable: there is no recorded failure evidence.
func runPRStepFailed(database *db.DB, runID string) (bool, []*db.StepResult, error) {
	rows, err := database.GetStepsByRun(runID)
	if err != nil {
		return false, nil, fmt.Errorf("get step results: %w", err)
	}
	for _, sr := range rows {
		if sr.StepName == types.StepPR {
			return sr.Status == types.StepStatusFailed, rows, nil
		}
	}
	return false, rows, nil
}

func shortRepublishSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
