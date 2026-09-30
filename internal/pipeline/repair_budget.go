package pipeline

import (
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	repairBudgetFindingCategory = "repair-budget"
	repairLimitFindingCategory  = "repair-limit"
)

// HasRepairBudgetExhaustion identifies a control-plane gate that unattended
// drivers must not turn into new repair authority or a silent clean pass.
func HasRepairBudgetExhaustion(raw string) bool {
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return false
	}
	for _, item := range findings.Items {
		if item.Category == repairBudgetFindingCategory || item.Category == repairLimitFindingCategory {
			return true
		}
	}
	return false
}

func HasConfiguredRepairLimitExhaustion(raw string) bool {
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return false
	}
	for _, item := range findings.Items {
		if item.Category == repairLimitFindingCategory {
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
	category := repairBudgetFindingCategory
	description := fmt.Sprintf("Repair budget exhausted for %s findings %s: consumed %d repairs, current authority limit %d, configured maximum %d; requested additional repair %d. No further repair is authorized. From this run's branch, authorize exactly one additional repair of the selected findings with `no-mistakes axi respond --step %s --action fix` (run %s). That response extends authority by one without exceeding the configured maximum.", step, findingIDsJSON(raw), decision.Consumed, decision.AuthorityLimit, decision.Limit, decision.Consumed+1, step, runID)
	if decision.AuthorityLimit >= decision.Limit {
		category = repairLimitFindingCategory
		description = fmt.Sprintf("Configured repair maximum reached for %s findings %s: consumed %d repairs out of %d. No additional repair can be authorized for this step lifecycle; approve to accept the unresolved findings, or skip or abort the step.", step, findingIDsJSON(raw), decision.Consumed, decision.Limit)
	}
	findings.Items = append(findings.Items, types.Finding{
		ID:          "repair-budget-" + string(step),
		Severity:    types.FindingSeverityWarning,
		Action:      types.ActionAskUser,
		Category:    category,
		Description: description,
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
		if item.Category != repairBudgetFindingCategory && item.Category != repairLimitFindingCategory {
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
