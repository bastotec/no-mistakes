package pipeline

import (
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const repairBudgetFindingCategory = "repair-budget"

// HasRepairBudgetExhaustion identifies a control-plane gate that unattended
// drivers must not turn into new repair authority or a silent clean pass.
func HasRepairBudgetExhaustion(raw string) bool {
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return false
	}
	for _, item := range findings.Items {
		if item.Category == repairBudgetFindingCategory {
			return true
		}
	}
	return false
}

func exhaustedRepairFindings(raw string, step types.StepName, runID string, decision db.RepairBudgetDecision) string {
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return raw
	} // Caller already normalized the payload.
	findings.Items = append(findings.Items, types.Finding{
		ID:          "repair-budget-" + string(step),
		Severity:    types.FindingSeverityWarning,
		Action:      types.ActionAskUser,
		Category:    repairBudgetFindingCategory,
		Description: fmt.Sprintf("Repair budget exhausted for %s findings %s: consumed %d repairs, current authority limit %d, configured maximum %d; requested additional repair %d. No further repair is authorized. From this run's branch, authorize exactly one additional repair of the selected findings with `no-mistakes axi respond --step %s --action fix` (run %s). That response extends authority by one without exceeding the configured maximum.", step, findingIDsJSON(raw), decision.Consumed, decision.AuthorityLimit, decision.Limit, decision.Consumed+1, step, runID),
	})
	result, err := types.MarshalFindingsJSON(findings)
	if err != nil {
		return raw
	}
	return result
}

// The budget decision is control-plane metadata, not work for a code fixer or
// a deferred finding that should follow its next validation pass.
func repairWorkFindings(raw string) string {
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return raw
	}
	items := findings.Items[:0]
	for _, item := range findings.Items {
		if item.Category != repairBudgetFindingCategory {
			items = append(items, item)
		}
	}
	findings.Items = items
	result, err := types.MarshalFindingsJSON(findings)
	if err != nil {
		return raw
	}
	return result
}
