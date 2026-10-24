package branchsync

import (
	"context"
	"os"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// supersededPreflightRun is selection evidence, NEVER certification of the old
// run. Unlike ordinary retained-work supersession, it permits only a known
// producer's pre-executor explicit-source refusal. In that producer,
// HandleStartExistingPRRun imports/binds the submitted anchor; shared setup may
// create scratch and fetch trusted policy before validation. SetRunGates and
// executor launch are strictly AFTER validation. The producer binding matters:
// an error string and absent execution rows alone do not prove this ordering.
// Unknown producers fail closed rather than inheriting this historical proof.
func (s *Service) supersededPreflightRun(ctx context.Context, older, newer *db.Run, branch string) bool {
	if s.DB == nil || s.Repo == nil || older == nil || newer == nil ||
		older.RepoID != s.Repo.ID || newer.RepoID != s.Repo.ID || older.Branch != branch || newer.Branch != branch ||
		!(older.CreatedAt < newer.CreatedAt || older.CreatedAt == newer.CreatedAt && older.ID < newer.ID) ||
		older.Status != types.RunFailed || !runHeadUnmoved(older) || strings.TrimSpace(older.HeadSHA) == "" ||
		!terminalRunStatus(newer.Status) || newer.TerminalHeadVerifiedAt == nil ||
		!exactPushedBinding(s.Repo, newer, branch) || older.PushActive || older.LastPushedSHA != nil || older.LastPushedAt != nil ||
		older.PushTargetKind != nil || older.PushTargetFingerprint != nil || older.PushRef != nil || older.PushGeneration != nil ||
		older.CustodyReturnedAt != nil || older.TerminalHeadVerifiedAt != nil || older.ReviewApprovedHeadSHA != nil ||
		older.LaunchNonce != nil || older.LaunchValidationGeneration != nil || older.LaunchIntentDigest != nil || older.LaunchReceiptClaimedAt != nil ||
		older.ExistingPRURL != nil || older.PRURL != nil || older.AwaitingAgentSince != nil || older.WorktreeDir == nil || strings.TrimSpace(*older.WorktreeDir) == "" {
		return false
	}
	switch ptr(older.NoMistakesBuildSHA) {
	case "e6624fb", "e6624fb0", "e6624fb072e9e029d727948640e441e9474ac418":
	default:
		return false
	}
	const refusal = "explicit PR source repository/ref does not match the configured push target and run branch; "
	if !strings.HasPrefix(ptr(older.Error), refusal+branch+" publishes to https://github.com/") {
		return false
	}
	gates, err := s.DB.GetRunGates(older.ID)
	if err != nil || gates != "" {
		return false
	}
	steps, err := s.DB.GetStepsByRun(older.ID)
	if err != nil || len(steps) != 0 {
		return false
	}
	invocations, err := s.DB.GetAgentInvocationsByRun(older.ID)
	if err != nil || len(invocations) != 0 {
		return false
	}
	sessions, err := s.DB.GetRunAgentSessions(older.ID)
	if err != nil || len(sessions) != 0 {
		return false
	}
	// carrySurvivedWork can copy a rerun's unlanded handoff before validation.
	// Reject ANY handoff, even if no step or invocation has been recorded.
	work, err := s.DB.GetRunSurvivedWork(older.ID)
	if err != nil || work != nil {
		return false
	}
	archives, err := s.DB.GetRecoveryArchivesByRun(older.ID)
	if err != nil || len(archives) != 0 {
		return false
	}
	// A remaining managed scratch must not hide uncommitted or advanced work.
	if _, err := os.Stat(*older.WorktreeDir); err == nil {
		head, err := git.HeadSHA(ctx, *older.WorktreeDir)
		if err != nil || head != older.HeadSHA {
			return false
		}
		if clean, _ := worktreeClean(ctx, *older.WorktreeDir); !clean {
			return false
		}
	} else if !os.IsNotExist(err) {
		return false
	}
	if strings.TrimSpace(s.GateDir) == "" || pushStepRunning(s.DB, newer.ID) {
		return false
	}
	gateRef := "refs/heads/" + branch
	gateBinding, err := git.Run(ctx, s.GateDir, "for-each-ref", "--format=%(refname) %(objectname) %(symref)", gateRef)
	if err != nil || gateBinding != gateRef+" "+ptr(newer.LastPushedSHA) {
		return false
	}
	// Only the ordinary submitted anchor is accounted for by the same-submitted
	// successor. Any independent recovery ref remains custody, not obsolete setup.
	for _, dir := range []string{s.workDir(), s.GateDir} {
		for _, ref := range []string{recoverAnchorRef(older.ID), recoverLocalAnchorRef(older.ID)} {
			binding, err := git.Run(ctx, dir, "for-each-ref", "--format=%(refname) %(objectname) %(symref)", ref)
			if err != nil || binding != "" && binding != ref+" "+older.HeadSHA {
				return false
			}
		}
	}
	if older.HeadSHA == ptr(newer.SubmittedHeadSHA) {
		return true
	}
	// A(S) -> B(S->P) -> C(P->Q): C need not have resubmitted S. Keep
	// B's exact same-submitted publication as the witness, but only if its
	// published work is provably retained by the current, gate-bound C. This
	// is not transitive run supersession: every old preflight check above
	// still applies, and neither an unverified witness nor chronology alone
	// can replace that publication/content proof.
	runs, err := s.DB.GetRunsByRepo(s.Repo.ID)
	if err != nil {
		return false
	}
	for _, witness := range runs {
		if witness.RepoID != s.Repo.ID || witness.Branch != branch ||
			ptr(witness.SubmittedHeadSHA) != older.HeadSHA ||
			!(older.CreatedAt < witness.CreatedAt || older.CreatedAt == witness.CreatedAt && older.ID < witness.ID) ||
			!(witness.CreatedAt < newer.CreatedAt || witness.CreatedAt == newer.CreatedAt && witness.ID < newer.ID) ||
			!terminalRunStatus(witness.Status) || witness.TerminalHeadVerifiedAt == nil ||
			!exactPushedBinding(s.Repo, witness, branch) || pushStepRunning(s.DB, witness.ID) {
			continue
		}
		if isAncestor(ctx, s.GateDir, witness.HeadSHA, newer.HeadSHA) {
			return true
		}
		retention, err := git.ComparePatchRetention(ctx, s.GateDir, newer.HeadSHA, witness.HeadSHA)
		if err == nil && retention.RetainsAll() {
			return true
		}
	}
	return false
}
