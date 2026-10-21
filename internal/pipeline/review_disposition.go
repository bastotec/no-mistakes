package pipeline

import (
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// appendReviewDispositionMetadata supplements a current assessment with the
// executor's final disposition. It never treats a held claim as repaired, or a
// committed but unreviewed fix as verification. Historical prose is not input.
func appendReviewDispositionMetadata(f *types.Findings, unresolved []types.Finding) {
	raiseRisk := func(level string) {
		rank := map[string]int{"": -1, "low": 0, "medium": 1, "high": 2}
		if rank[level] > rank[f.RiskLevel] {
			f.RiskLevel = level
		}
	}
	// The latest rereview's own rationale describes only what it found
	// itself. Bare beside carried findings, a clean one ("no actionable
	// finding survived verification") contradicts them, so the disposition
	// leads and that assessment follows, scoped to the rereview.
	current := strings.TrimSpace(f.RiskRationale)
	var reasons []string
	appendReason := func(reason string) { reasons = append(reasons, reason) }
	defer func() {
		if len(reasons) == 0 {
			return
		}
		rationale := strings.Join(reasons, "; ")
		switch {
		case current == "":
		case isCleanRereviewRationale(current):
			rationale += "; the latest rereview raised no new actionable finding"
		default:
			rationale += "; the latest rereview: " + current
		}
		f.RiskRationale = rationale
	}()
	if len(unresolved) > 0 {
		f.Summary = strings.TrimSpace(f.Summary + fmt.Sprintf(" Carried review findings remain unresolved in the review ledger (current validity not reverified): %d.", len(unresolved)))
		for _, item := range unresolved {
			if item.ActionOrDefault() != types.ActionNoOp {
				switch types.NormalizeFindingSeverity(item.Severity) {
				case "error":
					raiseRisk("high")
				case "warning":
					raiseRisk("medium")
				}
			}
			description, _, _ := strings.Cut(item.Description, "\n")
			runes := []rune(description)
			if len(runes) > 160 {
				description = string(runes[:160]) + "..."
			}
			// A held claim remains undispositioned, not newly verified. In
			// particular, a composite repair may address it under another ID.
			// Preserve the claim as a quotation without certifying either its
			// continued presence or its repair from selection metadata alone.
			appendReason(fmt.Sprintf("carried %s %s (current validity not reverified): %q", item.Severity, item.ID, description))
		}
	}
	if len(f.ResolvedByFix) > 0 {
		f.Summary += fmt.Sprintf(" %d findings fixed without rereview.", len(f.ResolvedByFix))
		raiseRisk("medium")
		appendReason("fixes committed without rereview; their correctness is not certified")
	}
	if f.RiskLevel == "" {
		f.RiskLevel = "low"
	}
	f.RiskScope = types.FindingsRiskScopeSourceOrExternal
}

// isCleanRereviewRationale reports a rereview rationale that says it found
// nothing actionable, as the multi-angle review writes it.
func isCleanRereviewRationale(rationale string) bool {
	return strings.EqualFold(strings.TrimSpace(rationale), types.NoActionableFindingRationale)
}
