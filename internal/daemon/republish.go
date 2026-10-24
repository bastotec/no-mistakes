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

	"github.com/kunchenguid/no-mistakes/internal/config"
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

// republishPrivateRefPrefix namespaces the operation-private refs a
// continuation fetches into. They live in the gate repository's ref store but
// outside its shared tracking, branch and recovery refs. Cleanup attempts to
// delete each before the operation returns, so fetching a default or target
// branch that advanced cannot move the gate's tracking refs.
const republishPrivateRefPrefix = "refs/no-mistakes/republish/"

// HandleRepublishPublication continues a TERMINAL run's publication without
// re-running validation. PRStep's bounded sanitize-and-retry loop normally
// requires a live executor; terminal publication needs a separate entry point
// so retrying prose does not require repeating Intent/Rebase/Review/Test.
//
// This path is deliberately NOT a rerun and NOT a live executor: it operates on
// the terminal run record itself, creates a throwaway detached worktree at the
// run's recorded head, and executes the ordinary PR step against the recorded
// step rows. Every guard the ordinary PR step applies still applies:
// assertPipelineHeadContinuity, explicit-target validation, the publication
// validator, redaction, and the bounded sanitize-and-retry loop.
//
// Refusals (all fail closed, none silently fall back):
//   - non-terminal run, or an active run on the branch (a live executor owns it;
//     its own PR step or a rerun is the supported path there)
//   - a run whose recorded head does not match --head, whose head is absent
//     from the gate, or whose live remote source branch no longer equals the
//     recorded head (a newer run published a different head on the same branch;
//     custody recovery owns reconciling that, never this continuation)
//   - a run whose PR step did not fail: continuing a green or never-attempted
//     publication re-publishes prose for no reason and can mask a real refusal
//   - a preserve-history run (its pins refuse out-of-band publication moves)
//   - a run with no recorded PR target: the publication target is pinned to
//     what the run recorded (runs.pr_base_branch), never re-resolved from the
//     live trusted config, so a later config change cannot redirect it
//
// It never re-runs a validation step, never invents a pass, never pushes
// source or evidence (the run's head is already on the remote, evidence
// publication and media attachment are disabled for the continuation so only
// already-persisted evidence is reused), never writes identity into shared
// gate config, never moves the gate's tracking refs, and never touches run
// status, custody or gate branches. The sole durable effects are the PR write
// itself and runs.pr_url when publication succeeds and the run had none - the
// same write the ordinary PR step makes.
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
	// R7: pin the publication target to what the run itself recorded. The
	// per-run pr_base_branch pin is written at launch from the PR the run
	// targets (or the operator's explicit override) and is never repinned, so
	// it is the only target this continuation may publish to. A run without
	// one cannot prove its original target, and re-resolving the target from
	// the live trusted config would let a later config change redirect an
	// already-validated head onto a branch it was never validated against.
	targetBranch := ""
	if run.PRBaseBranch != nil {
		targetBranch = strings.TrimSpace(*run.PRBaseBranch)
	}
	if targetBranch == "" {
		return nil, fmt.Errorf("run %s recorded no PR target branch; refusing to republish against a target the run never pinned (add new persistence for historical targets is out of scope)", run.ID)
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

	m.mu.Lock()
	done := m.dones[run.ID]
	m.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-opCtx.Done():
			return nil, fmt.Errorf("wait for run %s cleanup: %w", run.ID, opCtx.Err())
		}
	}

	gateDir := m.paths.RepoDir(repo.ID)
	// The terminal run's own worktree was removed when the run finished, so
	// carve a throwaway detached worktree from the gate bare repo at the exact
	// recorded head. It is removed on every exit path; nothing is committed to
	// it, and it never becomes the run's recorded worktree.
	if _, err := git.Run(opCtx, gateDir, "cat-file", "-e", recorded+"^{commit}"); err != nil {
		return nil, fmt.Errorf("run %s head %s is absent from the gate repository; recover custody before republishing (no-mistakes axi sync --check)", run.ID, shortRepublishSHA(recorded))
	}

	// R1: prove the live remote source branch still equals the recorded head
	// BEFORE any PR write. If a later run published a different head on this
	// branch, this run's attestation must not overwrite the newer PR body;
	// custody recovery owns reconciling that, never this continuation. A fetch
	// cannot answer this: it only mutates local refs, so read the remote tip
	// directly.
	branch := strings.TrimPrefix(run.Branch, "refs/heads/")
	pushURL := republishPushURL(repo)
	remoteHead, err := git.LsRemote(opCtx, gateDir, pushURL, "refs/heads/"+branch)
	if err != nil {
		return nil, fmt.Errorf("verify remote source branch %s: %w (no publication was attempted)", branch, err)
	}
	if remoteHead != recorded {
		return nil, fmt.Errorf("remote branch %s is at %s, not the recorded head %s; a newer publication owns this branch (recover custody with no-mistakes axi sync --check instead)", branch, shortRepublishSHA(remoteHead), shortRepublishSHA(recorded))
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

	// R3: continuation config reads fetch through operation-private refs that
	// are deleted before returning, so the gate's shared tracking refs stay
	// exactly where the run left them even when the default or target branch
	// advanced in the meantime.
	privateRefs := republishPrivateRefs(run.ID)
	defer func() {
		// R3: delete every ref under this operation's private namespace (the
		// integration prefix holds per-branch refs appended by the step's
		// fetches) plus the trusted ref itself, leaving the gate's ordinary
		// tracking refs exactly where the run left them. A missing ref deletes
		// cleanly; a failure here is logged, never propagated over a
		// successful publication.
		nsPrefix := republishPrivateRefPrefix + run.ID + "/"
		if listing, err := git.Run(context.Background(), wtDir, "for-each-ref", "--format=%(refname)", nsPrefix); err == nil {
			for _, ref := range strings.Fields(listing) {
				if _, err := git.Run(context.Background(), wtDir, "update-ref", "-d", ref); err != nil {
					slog.Warn("failed to delete republish private ref", "run_id", run.ID, "ref", ref, "error", err)
				}
			}
		}
		for _, ref := range privateRefs {
			if _, err := git.Run(context.Background(), wtDir, "update-ref", "-d", ref); err != nil {
				slog.Warn("failed to delete republish private ref", "run_id", run.ID, "ref", ref, "error", err)
			}
		}
	}()

	// Reuse the recovered-run configuration path: same global+repo+trusted
	// merge and the run's own pinned gates. The worktree is at the recorded
	// head, so the repo-config read is the config that head carried.
	cfg, err := m.loadRepublishConfig(opCtx, run, repo, wtDir, privateRefs)
	if err != nil {
		return nil, fmt.Errorf("load run configuration: %w", err)
	}
	// R2: the continuation reuses already-persisted evidence and never pushes
	// fresh evidence or writes attachment caches. An ordinary run may publish
	// an orphan evidence branch or upload media at PR render time; a
	// publication-only continuation's single durable write is the PR URL.
	cfg.Test.Evidence.StoreInRepo = false
	cfg.Test.Evidence.AttachMedia = false
	// R7: pin the PR target to the run's recorded pin for this execution. The
	// config's pr.base_branch (re-read above from live trusted state) only
	// feeds steps that lack the per-run pin; setting it to the recorded pin
	// makes every downstream read agree with what the run targeted.
	cfg.PR.BaseBranch = targetBranch
	run.PRBaseBranch = &targetBranch

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
	log(fmt.Sprintf("terminal publication continuation for run %s at head %s (PR step failed; validation is NOT re-run; target pinned to %s)", run.ID, recorded, targetBranch))

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
		// R3: the PR-format policy fetch inside PRStep routes through the
		// operation-private target ref, never the gate's origin/ tracking ref.
		IntegrationRefPrefix: republishPrivateRefPrefix + run.ID + "/integration/",
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
	if outcome == nil {
		return nil, fmt.Errorf("republish returned no publication outcome")
	}
	if outcome.Skipped {
		reason := strings.TrimSpace(outcome.SkipReason)
		if reason == "" {
			reason = "PR step was skipped"
		}
		return nil, fmt.Errorf("republish did not publish: %s", reason)
	}
	if strings.TrimSpace(outcome.PRURL) == "" {
		return nil, fmt.Errorf("republish returned no PR URL")
	}
	result := &ipc.RepublishPublicationResult{
		RunID:   run.ID,
		HeadSHA: recorded,
		PRURL:   outcome.PRURL,
	}
	for _, sr := range stepRows {
		if sr.StepName != types.StepPR && sr.StepName != types.StepCI {
			result.Steps = append(result.Steps, string(sr.StepName)+":"+string(sr.Status))
		}
	}
	if run.PRURL == nil || strings.TrimSpace(*run.PRURL) == "" {
		// The same durable write the ordinary PR step makes when it creates
		// a PR. Status, custody and gate refs are deliberately untouched:
		// this is a publication continuation, not a run transition.
		if err := m.db.UpdateRunPRURL(run.ID, outcome.PRURL); err != nil {
			return nil, fmt.Errorf("persist PR URL: %w", err)
		}
	}
	log("republish published: " + outcome.PRURL)
	return result, nil
}

// republishPrivateRefs names the operation-private refs one continuation
// fetches into: the trusted default branch (for trusted-config selection) and
// the integration namespace prefix (for PR-format policy, which appends the
// target branch). Both are under republishPrivateRefPrefix and every ref in
// the namespace is deleted before the operation returns.
func republishPrivateRefs(runID string) []string {
	return []string{
		republishPrivateRefPrefix + runID + "/trusted-default",
		republishPrivateRefPrefix + runID + "/integration",
	}
}

// loadRepublishConfig is loadRecoveredConfig with every remote fetch routed
// through operation-private refs (R3). It reads the same global + pushed-head
// repo + trusted-default-branch merge and the run's pinned gates, but a
// continuation must not move the gate's ordinary tracking refs: they belong to
// live runs and recovery, and the default or target branch may legitimately
// have advanced since this run failed.
func (m *RunManager) loadRepublishConfig(ctx context.Context, run *db.Run, repo *db.Repo, workDir string, privateRefs []string) (*config.Config, error) {
	globalCfg, err := config.LoadGlobal(m.paths.ConfigFile())
	if err != nil {
		return nil, fmt.Errorf("load global config: %w", err)
	}
	repoCfg, err := config.LoadRepo(workDir)
	if err != nil {
		return nil, fmt.Errorf("load repo config: %w", err)
	}
	var trustedSHA string
	if repo.DefaultBranch != "" {
		fetchCtx, cancel := context.WithTimeout(ctx, recoveredConfigFetchTimeout)
		defer cancel()
		// Spell the source as a URL, not a remote name: a named remote's
		// fetch opportunistically updates its tracking ref (observed with git
		// 2.53 even with --no-write-fetch-head and a private destination),
		// which would move the gate's refs/remotes/origin/<default> that live
		// runs and recovery share (R3). A URL fetch writes only the private
		// destination ref.
		trustedURL := repo.UpstreamURL
		if url, err := git.GetRemoteURL(ctx, workDir, "origin"); err == nil && strings.TrimSpace(url) != "" {
			trustedURL = url
		}
		if err := git.FetchURLBranchToPrivateRef(fetchCtx, workDir, trustedURL, repo.DefaultBranch, privateRefs[0]); err != nil {
			slog.Warn("failed to fetch default branch for republish; trusted config disabled", "run_id", run.ID, "branch", repo.DefaultBranch, "error", err)
		} else if sha, err := git.ResolveRef(ctx, workDir, privateRefs[0]); err != nil {
			slog.Warn("failed to resolve default branch for republish; trusted config disabled", "run_id", run.ID, "branch", repo.DefaultBranch, "error", err)
		} else {
			trustedSHA = sha
		}
	}
	// SECURITY: a trusted-config fetch failure must abort, not silently disable
	// the disable_project_settings opt-out (see assertGateTrustedConfigReadable).
	if err := assertGateTrustedConfigReadable(ctx, workDir, repo.DefaultBranch, trustedSHA); err != nil {
		return nil, err
	}
	trustedRepoCfg := loadTrustedRepoConfig(ctx, workDir, trustedSHA, run.ID)
	allowRepoCommands := trustedRepoCfg != nil && trustedRepoCfg.AllowRepoCommands
	effectiveRepoCfg := config.EffectiveRepoConfig(repoCfg, trustedRepoCfg, allowRepoCommands)
	cfg := config.Merge(globalCfg, effectiveRepoCfg)
	// Gates are read back from the run, never re-resolved: the run's recorded
	// steps are what this continuation renders.
	gates, err := m.pinnedRunGates(run.ID)
	if err != nil {
		return nil, err
	}
	cfg.Gates = gates
	if err := m.paths.ValidateEvidenceRoot(cfg.Test.Evidence.LocalRoot); err != nil {
		return nil, err
	}
	cfg.TrustedConfigSHA = trustedSHA
	if globalCfg.Eval.CaptureProvenance {
		if err := cfg.EnableEvalProvenance(globalCfg, effectiveRepoCfg); err != nil {
			return nil, err
		}
	}
	return cfg, nil
}

// republishPushURL mirrors steps.resolvePushURL for the pre-worktree guard:
// the fork when one is configured, else the upstream URL. It answers only
// "which remote does this run's branch live on", before any step context
// exists.
func republishPushURL(repo *db.Repo) string {
	if repo != nil && strings.TrimSpace(repo.ForkURL) != "" {
		return repo.ForkURL
	}
	if repo != nil {
		return repo.UpstreamURL
	}
	return ""
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
