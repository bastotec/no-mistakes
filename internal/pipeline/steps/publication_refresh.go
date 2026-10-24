package steps

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Reuse the forge's exact numbered PR/source identity proof, not branch
// discovery. Backends without that proof refuse rather than infer a head.
type publicationIdentityReader interface {
	ValidateExistingPRIdentity(context.Context, string, string, string) (*scm.PR, error)
}

const repairDispositionStart = "<!-- no-mistakes-repair-disposition:v1 -->"
const repairDispositionEnd = "<!-- /no-mistakes-repair-disposition:v1 -->"

func refreshRepairPublication(sctx *pipeline.StepContext, host scm.Host, p ipc.RefreshPublicationParams) (*ipc.RefreshPublicationResult, error) {
	if err := ValidateHistoryBase(p.HeadSHA); err != nil {
		return nil, err
	}
	reader, ok := host.(scm.PRContentReader)
	identity, proven := host.(publicationIdentityReader)
	if !ok || !proven {
		return nil, fmt.Errorf("refresh requires raw PR content and exact numbered PR/source identity support")
	}
	bind := func() (*scm.PR, error) {
		run, err := sctx.DB.GetRun(p.RunID)
		if err != nil {
			return nil, err
		}
		if run == nil || run.ID != sctx.Run.ID || run.RepoID != p.RepoID || run.Status != types.RunRunning || run.HeadSHA != p.HeadSHA || run.LastPushedSHA == nil || *run.LastPushedSHA != p.HeadSHA || run.PRURL == nil || *run.PRURL != p.PRURL || run.AwaitingAgentSince != nil {
			return nil, fmt.Errorf("refresh run/repository/PR/recorded/pushed head binding changed")
		}
		rows, err := sctx.DB.GetStepsByRun(run.ID)
		if err != nil {
			return nil, err
		}
		monitoring := false
		for _, row := range rows {
			if row.StepName == types.StepCI && row.Status == types.StepStatusRunning {
				monitoring = true
			}
		}
		if !monitoring {
			return nil, fmt.Errorf("refresh requires a running CI monitor")
		}
		remote, err := publishedBranchHead(sctx)
		if err != nil {
			return nil, err
		}
		if remote != p.HeadSHA {
			return nil, fmt.Errorf("refresh remote source head does not match recorded/pushed head")
		}
		pr, err := identity.ValidateExistingPRIdentity(sctx.Ctx, p.PRURL, scm.RepoPath(resolvePushURL(sctx)), run.Branch)
		if err != nil {
			return nil, err
		}
		if pr == nil || pr.URL != p.PRURL || pr.HeadSHA != p.HeadSHA || pr.Number == "" || pr.BaseBranch == "" {
			return nil, fmt.Errorf("refresh forge PR identity/head mismatch")
		}
		if run.PRBaseBranch != nil && *run.PRBaseBranch != pr.BaseBranch {
			return nil, fmt.Errorf("refresh PR target differs from the run pin")
		}
		return pr, nil
	}
	pr, err := bind()
	if err != nil {
		return nil, err
	}
	current, err := reader.GetPRContent(sctx.Ctx, pr)
	if err != nil {
		return nil, err
	}
	parts := prOwnedBody{before: current.Body}
	if hasPRAppendixMarkers(current.Body) {
		parts, err = parsePROwnedBody(current.Body)
		if err != nil {
			return nil, err
		}
	}
	// The ordinary non-template PR path has a legacy unowned attestation.
	// Appending a separate owned block preserves those bytes too; we never
	// regenerate, restamp, or reinterpret historical certification here.
	if !parts.managed {
		parts = prOwnedBody{before: current.Body}
	}
	start := strings.Index(current.Body, pipelineAttestationCommentPrefix)
	if start < 0 || strings.Count(current.Body, pipelineAttestationCommentPrefix) != 1 {
		return nil, fmt.Errorf("refresh requires one existing pipeline attestation")
	}
	payload, _, closed := strings.Cut(current.Body[start+len(pipelineAttestationCommentPrefix):], pipelineAttestationCommentClosingToken)
	var att pipelineAttestation
	if !closed || json.Unmarshal([]byte(payload), &att) != nil || att.HeadSHA != p.HeadSHA {
		return nil, fmt.Errorf("refresh attestation head mismatch")
	}
	checks, checkErr := host.GetChecks(sctx.Ctx, pr)
	if pr.HeadSHA != p.HeadSHA {
		return nil, fmt.Errorf("refresh check observation changed PR head")
	}
	disposition := "inconclusive: live checks could not be read"
	if checkErr != nil {
		checks = nil
	}
	if checkErr == nil {
		switch {
		case len(checks) == 0:
			disposition = "inconclusive: no live checks reported"
		case refreshChecksPassed(checks):
			disposition = "all reported live CI checks passed; this is not a review or live product validation"
		default:
			disposition = "unresolved: live CI checks are failing, pending, skipped, or inconclusive"
		}
	}
	rows, err := sctx.DB.GetStepsByRun(p.RunID)
	if err != nil {
		return nil, err
	}
	text := "### Current-head repair disposition\n\nAttributed to the CI monitor's publication refresh, not the original reviewers or Test agent.\n\nHead: `" + p.HeadSHA + "`. Live forge observation: " + disposition + ".\n\nHistorical review risk, findings, step statuses, skips, and timeouts above remain unchanged. No validation was replayed. No finding is certified fixed by green CI; finding-specific repair correctness and live product behavior remain unproven unless separately validated. This refresh grants no merge authority and changes no labels.\n"
	passed, failed, skipped, other := 0, 0, 0, 0
	for _, check := range checks {
		switch check.Bucket {
		case scm.CheckBucketPass:
			passed++
		case scm.CheckBucketFail:
			failed++
		case scm.CheckBucketSkip:
			skipped++
		default:
			other++
		}
	}
	text += fmt.Sprintf("\nObserved at: %s. Reported checks: %d passed, %d failed, %d skipped, %d pending or otherwise unresolved.\n", time.Now().UTC().Format(time.RFC3339), passed, failed, skipped, other)
	for _, row := range rows {
		if row.StepName != types.StepCI {
			continue
		}
		rounds, err := sctx.DB.GetRoundsByStep(row.ID)
		if err != nil {
			return nil, err
		}
		for _, round := range rounds {
			if round.IsFixRound() && round.RepairPublished {
				text += fmt.Sprintf("\n- Recorded published CI repair round %d", round.Round)
				if round.StartingHeadSHA != nil && ValidateHistoryBase(*round.StartingHeadSHA) == nil {
					text += " starting at `" + *round.StartingHeadSHA + "`"
				}
				text += ".\n"
			}
		}
	}
	// No headings are used to remove prose. Replace only our explicitly
	// delimited refresh block, and refuse ambiguous markers.
	base := parts.appendix
	if !parts.managed {
		base = current.Body
	}
	base, err = removeRepairDisposition(base)
	if err != nil {
		return nil, err
	}
	text = redactPRContent(prContent{Body: neutralizeAttestationMarkers(text)}).Body
	block := fmt.Sprintf("%s\n<!-- sha256=%x -->\n%s%s", repairDispositionStart, sha256.Sum256([]byte(text)), text, repairDispositionEnd)
	updated := base + "\n\n" + block
	if parts.managed {
		updated = parts.before + wrapPRAppendix(updated) + parts.after
	}
	if err := validateOwnedPRBudget(updated, scm.MaxPRBodyChars(host.Provider())); err != nil {
		return nil, err
	}
	latest, err := reader.GetPRContent(sctx.Ctx, pr)
	if err != nil {
		return nil, err
	}
	if latest.Body != current.Body {
		return nil, fmt.Errorf("PR body changed while preparing refresh; retry")
	}
	latestPR, err := bind()
	if err != nil {
		return nil, err
	}
	if latestPR.Number != pr.Number || latestPR.BaseBranch != pr.BaseBranch {
		return nil, fmt.Errorf("refresh PR target/identity changed while preparing publication")
	}
	if err := sctx.Ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := host.UpdatePR(sctx.Ctx, pr, scm.PRContent{Body: updated}); err != nil {
		return nil, err
	}
	return &ipc.RefreshPublicationResult{RunID: p.RunID, HeadSHA: p.HeadSHA, PRURL: p.PRURL, Disposition: disposition}, nil
}

func removeRepairDisposition(body string) (string, error) {
	n, m := strings.Count(body, repairDispositionStart), strings.Count(body, repairDispositionEnd)
	if n == 0 && m == 0 {
		return body, nil
	}
	if n != 1 || m != 1 {
		return "", fmt.Errorf("ambiguous repair disposition markers")
	}
	start, end := strings.Index(body, repairDispositionStart), strings.Index(body, repairDispositionEnd)
	if end < start {
		return "", fmt.Errorf("malformed repair disposition markers")
	}
	payload := body[start+len(repairDispositionStart) : end]
	digest, content, ok := strings.Cut(strings.TrimPrefix(payload, "\n"), "\n")
	if !strings.HasPrefix(payload, "\n") || !ok || digest != fmt.Sprintf("<!-- sha256=%x -->", sha256.Sum256([]byte(content))) {
		return "", fmt.Errorf("repair disposition was edited; refusing to overwrite it")
	}
	if start < 2 || body[start-2:start] != "\n\n" {
		return "", fmt.Errorf("malformed repair disposition boundary")
	}
	return body[:start-2] + body[end+len(repairDispositionEnd):], nil
}

// Skips are not passing tests, even when the monitor's readiness policy allows
// them. A publication claim deliberately uses the stricter interpretation.
func refreshChecksPassed(checks []scm.Check) bool {
	if len(checks) == 0 {
		return false
	}
	for _, check := range checks {
		if check.Bucket != scm.CheckBucketPass {
			return false
		}
	}
	return true
}
