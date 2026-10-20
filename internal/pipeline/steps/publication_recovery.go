package steps

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// publicationPins capture the resolved publication intent BEFORE drafting. A
// later configuration/base/remote change must never silently retarget recovery.
// Digests contain neither credentials nor agent prose.
type publicationPins struct {
	HeadSHA        string
	Branch         string
	BaseBranch     string
	BaseSHA        string
	PolicySHA      string
	Provider       scm.Provider
	PushTarget     string
	PRTarget       string
	ForgeIdentity  string
	ConfigDigest   string
	EvidenceDigest string
}

func publicationDigest(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(raw)), nil
}

func publicationEvidence(sctx *pipeline.StepContext) ([]*db.StepResult, map[string][]*db.StepRound, string, error) {
	rows, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		return nil, nil, "", err
	}
	var validation []*db.StepResult
	rounds := make(map[string][]*db.StepRound)
	for _, row := range rows {
		if row.StepName.Order() >= types.StepPR.Order() {
			continue
		}
		validation = append(validation, row)
		rounds[row.ID], err = sctx.DB.GetRoundsByStep(row.ID)
		if err != nil {
			return nil, nil, "", err
		}
	}
	gates, err := sctx.DB.GetRunGates(sctx.Run.ID)
	if err != nil {
		return nil, nil, "", err
	}
	digest, err := publicationDigest(struct {
		Head, Base, Branch   string
		Review               *string
		Intent, IntentSource *string
		Gates                string
		Steps                []*db.StepResult
		Rounds               map[string][]*db.StepRound
	}{
		sctx.Run.HeadSHA, sctx.Run.BaseSHA, sctx.Run.Branch, sctx.Run.ReviewApprovedHeadSHA, sctx.Run.Intent, sctx.Run.IntentSource, gates, validation, rounds,
	})
	return validation, rounds, digest, err
}

func currentPublicationPins(sctx *pipeline.StepContext, branch, base, baseSHA, policy string, provider scm.Provider, host scm.Host) (publicationPins, error) {
	_, _, evidence, err := publicationEvidence(sctx)
	if err != nil {
		return publicationPins{}, err
	}
	// Only publication-affecting configuration is bound. Agent routing/timeouts
	// may change; neither changes what was validated or the intended destination.
	cfg, err := publicationDigest(struct {
		PR        any
		Test      any
		Commit    any
		Providers any
	}{sctx.Config.PR, sctx.Config.Test, sctx.Config.Commit, sctx.Config.Providers})
	if err != nil {
		return publicationPins{}, err
	}
	identity := ""
	if target, ok := host.(interface{ PublicationIdentity() string }); ok {
		if value := target.PublicationIdentity(); value != "" {
			identity, err = publicationDigest(value)
			if err != nil {
				return publicationPins{}, err
			}
		}
	}
	return publicationPins{
		HeadSHA: sctx.Run.HeadSHA, Branch: branch, BaseBranch: base,
		BaseSHA: baseSHA, PolicySHA: policy, Provider: provider,
		PushTarget:    branchsync.TargetFingerprint(resolvePushURL(sctx)),
		PRTarget:      branchsync.TargetFingerprint(sctx.Repo.UpstreamURL),
		ForgeIdentity: identity, ConfigDigest: cfg, EvidenceDigest: evidence,
	}, nil
}

func pinPublication(sctx *pipeline.StepContext, branch, base, baseSHA, policy string, provider scm.Provider, host scm.Host) error {
	pins, err := currentPublicationPins(sctx, branch, base, baseSHA, policy, provider, host)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(pins)
	if err != nil {
		return err
	}
	return sctx.DB.PinPublicationIntent(sctx.Run.ID, sctx.Run.HeadSHA, string(payload))
}

func recoveryRefusal(clause string) error {
	return fmt.Errorf("publication recovery refused: %s", clause)
}

// ValidatePublicationRecoveryRecords is read-only, including for historical
// runs. It deliberately admits only this run's terminal final-content refusal,
// never an earlier failed stage, an approval waiver, or missing evidence.
func ValidatePublicationRecoveryRecords(sctx *pipeline.StepContext) (*publicationPins, error) {
	if sctx.Run.Status != types.RunFailed {
		return nil, recoveryRefusal("run must be terminal and failed")
	}
	eligible, err := sctx.DB.PublicationRecoveryEligible(sctx.Run.ID)
	if err != nil {
		return nil, err
	}
	if !eligible {
		return nil, recoveryRefusal("historical run predates publication-intent pinning")
	}
	pin, err := sctx.DB.GetPublicationIntent(sctx.Run.ID, sctx.Run.HeadSHA)
	if err != nil {
		return nil, err
	}
	if pin == nil || pin.Refusal == nil {
		return nil, recoveryRefusal("missing pinned publication intent or ordinary guard refusal")
	}
	if sctx.Run.Error == nil || *sctx.Run.Error != "step pr failed: "+*pin.Refusal {
		return nil, recoveryRefusal("terminal run failure does not match the recorded publication refusal")
	}
	var pins publicationPins
	if json.Unmarshal([]byte(pin.JSON), &pins) != nil || pins.PolicySHA == "" || pins.BaseBranch == "" || pins.ForgeIdentity == "" {
		return nil, recoveryRefusal("invalid publication intent")
	}
	rows, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		return nil, err
	}
	seen := make(map[types.StepName]bool)
	for _, row := range rows {
		if seen[row.StepName] {
			return nil, recoveryRefusal("duplicate step evidence")
		}
		seen[row.StepName] = true
		switch {
		case row.StepName == types.StepPR:
			if row.Status != types.StepStatusFailed || row.Error == nil || *row.Error != *pin.Refusal || !strings.HasPrefix(*row.Error, "refusing non-compliant PR publication:") {
				return nil, recoveryRefusal("run did not fail at the recorded publication guard")
			}
		case row.StepName.Order() < types.StepPR.Order():
			optionalSkipped := (row.StepName == types.StepJev || row.StepName == types.StepIntent) && row.Status == types.StepStatusSkipped
			if row.Status != types.StepStatusCompleted && !optionalSkipped {
				return nil, recoveryRefusal("incomplete validation step " + string(row.StepName))
			}
			if row.OverrideReason != nil {
				return nil, recoveryRefusal("validation override is not clean evidence: " + string(row.StepName))
			}
		default:
			if row.Status != types.StepStatusPending {
				return nil, recoveryRefusal("run already proceeded beyond publication")
			}
		}
	}
	for _, name := range []types.StepName{types.StepRebase, types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.StepPush, types.StepPR} {
		if !seen[name] {
			return nil, recoveryRefusal("missing validation step " + string(name))
		}
	}
	gatesJSON, err := sctx.DB.GetRunGates(sctx.Run.ID)
	if err != nil {
		return nil, err
	}
	gates, err := config.ParseGates(gatesJSON)
	if err != nil {
		return nil, recoveryRefusal("invalid pinned gate list")
	}
	for _, gate := range gates {
		if !seen[gate.StepName()] {
			return nil, recoveryRefusal("missing validation step " + string(gate.StepName()))
		}
	}
	validation, rounds, digest, err := publicationEvidence(sctx)
	if err != nil {
		return nil, err
	}
	if digest != pins.EvidenceDigest {
		return nil, recoveryRefusal("recorded head or validation evidence differs from publication pins")
	}
	for _, row := range validation {
		for _, r := range rounds[row.ID] {
			if r.FindingsJSON != nil {
				if _, err := types.ParseFindingsJSON(*r.FindingsJSON); err != nil {
					return nil, recoveryRefusal("unreadable round evidence: " + string(row.StepName))
				}
			}
		}
		if row.FindingsJSON != nil {
			if _, err := types.ParseFindingsJSON(*row.FindingsJSON); err != nil {
				return nil, recoveryRefusal("unreadable final evidence: " + string(row.StepName))
			}
		}
		if row.StepName == types.StepTest {
			raw := testingEvidenceFindingsJSON(row, rounds[row.ID])
			if len(raw) == 0 || raw[0] == nil {
				return nil, recoveryRefusal("missing Test scenario evidence")
			}
			var f Findings
			if err := unmarshalRequiredTestFindings([]byte(*raw[0]), &f); err != nil {
				return nil, recoveryRefusal("invalid Test scenario evidence: " + err.Error())
			}
		}
	}
	if sctx.Run.ReviewApprovedHeadSHA == nil || *sctx.Run.ReviewApprovedHeadSHA == "" {
		return nil, recoveryRefusal("missing recorded review-approved head")
	}
	live := attestedLiveValidation(validation, rounds, sctx.Run.HeadSHA)
	if live == nil || live.Verdict != types.TestVerdictGo {
		return nil, recoveryRefusal("missing exact-head go verdict for Test")
	}
	var repaired bool
	for _, row := range validation {
		if row.StepName == types.StepReview {
			f := reviewStepFindings(row, rounds[row.ID])
			if f == nil {
				return nil, recoveryRefusal("unreadable review dispositions")
			}
			repaired = len(f.ResolvedByFix) > 0
		}
	}
	if !repaired {
		return nil, recoveryRefusal("no recorded repaired review dispositions to reconcile")
	}
	if sctx.Run.LastPushedSHA == nil || *sctx.Run.LastPushedSHA != sctx.Run.HeadSHA || sctx.Run.PushTargetFingerprint == nil || *sctx.Run.PushTargetFingerprint != pins.PushTarget || sctx.Run.PushRef == nil || *sctx.Run.PushRef != "refs/heads/"+pins.Branch || sctx.Run.PushActive {
		return nil, recoveryRefusal("missing exact source-head publication binding")
	}
	return &pins, nil
}

// RecoverPublication calls the ordinary PR step, not Push or a new validation
// pipeline. No validation record is edited. The caller serializes the branch;
// the step repeats live head/target checks immediately before CreatePR.
func RecoverPublication(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	pins, err := ValidatePublicationRecoveryRecords(sctx)
	if err != nil {
		return nil, err
	}
	return (&PRStep{recovery: pins}).Execute(sctx)
}

func (s *PRStep) verifyRecoveryPublication(sctx *pipeline.StepContext, draftedBase, draftedPolicy string, provider scm.Provider, host scm.Host) error {
	pins := s.recovery
	if pins == nil {
		return nil
	}
	if draftedBase != pins.BaseSHA || draftedPolicy != pins.PolicySHA {
		return recoveryRefusal("target branch/base or policy differs from original publication intent")
	}
	ctx, cancel := contextWithPublicationTimeout(sctx)
	defer cancel()
	baseBranch := effectivePRBaseBranch(sctx)
	policy, err := resolvePRPolicySHA(ctx, sctx, baseBranch)
	if err != nil {
		return recoveryRefusal("target branch/base revision could not be verified")
	}
	baseSHA, err := resolveBranchBaseSHA(ctx, sctx, sctx.Run.BaseSHA, baseBranch)
	if err != nil {
		return err
	}
	current, err := currentPublicationPins(sctx, strings.TrimPrefix(sctx.Run.Branch, "refs/heads/"), baseBranch, baseSHA, policy, provider, host)
	if err != nil {
		return err
	}
	if current != *pins {
		return recoveryRefusal("target branch/base, policy, configuration, or evidence differs from original publication intent")
	}
	head, err := git.HeadSHA(sctx.Ctx, sctx.WorkDir)
	if err != nil || head != sctx.Run.HeadSHA {
		return recoveryRefusal("recorded source head is missing or worktree head differs")
	}
	dirty, err := git.HasUncommittedChanges(sctx.Ctx, sctx.WorkDir)
	if err != nil || dirty {
		return recoveryRefusal("publication worktree is not clean")
	}
	if err := AssertHistoryPolicy(sctx); err != nil {
		return err
	}
	remote, err := git.LsRemote(ctx, sctx.WorkDir, resolvePushURL(sctx), "refs/heads/"+pins.Branch)
	if err != nil || remote != sctx.Run.HeadSHA {
		return recoveryRefusal("live source branch does not equal the recorded head")
	}
	return nil
}

// Each live read has its own budget; this is not a force-push lease.
func contextWithPublicationTimeout(sctx *pipeline.StepContext) (context.Context, context.CancelFunc) {
	return context.WithTimeout(sctx.Ctx, 60*time.Second)
}

// recoveryRiskLine replaces stale free-form rationale with explicit recorded
// dispositions. It never upgrades a fix without rereview to a reviewed fix, nor
// drops remaining notes or the original risk rating. Ordinary rendering stays
// unchanged, and the ordinary final-content guard judges the resulting prose.
func recoveryRiskLine(rows []*db.StepResult, rounds map[string][]*db.StepRound, priorDecisions ...*db.BranchDecisionRound) string {
	for _, row := range rows {
		if row.StepName != types.StepReview {
			continue
		}
		f := reviewStepFindings(row, rounds[row.ID])
		if f == nil {
			return ""
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%s %s (recorded review rating).", riskEmoji(f.RiskLevel), capitalizeRisk(f.RiskLevel))
		for _, item := range f.ResolvedByFix {
			fmt.Fprintf(&b, "\n\n- Fixed without rereview (recorded disposition; not independently revalidated): %s", item.Description)
		}
		for _, item := range f.UnresolvedNotes {
			fmt.Fprintf(&b, "\n\n- Unresolved review note (not fixed): %s", item.Description)
		}
		for _, item := range f.Items {
			fmt.Fprintf(&b, "\n\n- Remaining finding: %s", item.Description)
		}
		if scope := strings.Join(reviewScopeDecisions(rounds[row.ID], priorDecisions...), "; "); scope != "" {
			fmt.Fprintf(&b, "\n\nRecorded repair scope: %s", scope)
		}
		return b.String()
	}
	return ""
}

// Fail closed on providers that cannot discover both open and closed PRs with
// exact source identity. Ordinary publication support is not changed.
func findRecoveryPR(sctx *pipeline.StepContext, host scm.Host) (*scm.PR, error) {
	reader, ok := host.(interface {
		FindPublicationPR(context.Context, string) (*scm.PR, error)
	})
	if !ok {
		return nil, recoveryRefusal("provider lacks complete exact-head publication discovery")
	}
	return reader.FindPublicationPR(sctx.Ctx, strings.TrimPrefix(sctx.Run.Branch, "refs/heads/"))
}

func (s *PRStep) recoveredExistingPR(sctx *pipeline.StepContext, pr *scm.PR) (*pipeline.StepOutcome, error) {
	if sctx.Run.PRURL != nil && strings.TrimSpace(*sctx.Run.PRURL) != "" && *sctx.Run.PRURL != pr.URL {
		return nil, recoveryRefusal("existing PR identity differs from recorded publication")
	}
	if pr.URL == "" || pr.HeadSHA == "" || pr.HeadSHA != s.recovery.HeadSHA || pr.BaseBranch != s.recovery.BaseBranch {
		return nil, recoveryRefusal("existing PR head or target/base differs from recorded publication")
	}
	return &pipeline.StepOutcome{PRURL: pr.URL}, nil
}

// The validator sees authoritative dispositions, not just generated prose. Its
// structured mismatch classification records eligibility separately; every
// non-compliance verdict still refuses through exactly the same guard.
func recordedPRDispositions(sctx *pipeline.StepContext) ([]byte, bool, error) {
	if sctx.DB == nil {
		return []byte("null"), false, nil
	}
	rows, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		return nil, false, err
	}
	for _, row := range rows {
		if row.StepName != types.StepReview {
			continue
		}
		rounds, err := sctx.DB.GetRoundsByStep(row.ID)
		if err != nil {
			return nil, false, err
		}
		f := reviewStepFindings(row, rounds)
		if f == nil {
			return []byte("null"), false, nil
		}
		raw, err := json.Marshal(struct {
			FixedWithoutRereview []types.Finding `json:"fixed_without_rereview"`
			UnresolvedNotes      []types.Finding `json:"unresolved_notes"`
			CurrentFindings      []types.Finding `json:"current_findings"`
		}{f.ResolvedByFix, f.UnresolvedNotes, f.Items})
		return raw, len(f.ResolvedByFix) > 0, err
	}
	return []byte("null"), false, nil
}
