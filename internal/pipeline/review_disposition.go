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
	appendReason := func(reason string) {
		if f.RiskRationale != "" {
			f.RiskRationale += "; "
		}
		f.RiskRationale += reason
	}
	if len(unresolved) > 0 {
		f.Summary = strings.TrimSpace(f.Summary + fmt.Sprintf(" Carried review findings still unresolved (not fixed): %d.", len(unresolved)))
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
			appendReason(fmt.Sprintf("unresolved %s %s: %s", item.Severity, item.ID, description))
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
