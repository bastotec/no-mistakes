package pipeline

import (
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	repairBudgetFindingCategory            = "repair-budget"
	repairBudgetHardCeilingFindingCategory = "repair-budget-hard-ceiling"
	repairReconciliationFindingCategory    = "repair-reconciliation"
)

// HasRepairBudgetExhaustion identifies a control-plane gate that unattended
// drivers must not turn into new repair authority or a silent clean pass.
func HasRepairBudgetExhaustion(raw string) bool {
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return false
	}
	for _, item := range findings.Items {
		if item.Category == repairBudgetFindingCategory || item.Category == repairBudgetHardCeilingFindingCategory {
			return true
		}
	}
	return false
}

func RepairBudgetAuthorityAvailable(raw string) bool {
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

func HasRepairReconciliation(raw string) bool {
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return false
	}
	for _, item := range findings.Items {
		if item.Category == repairReconciliationFindingCategory {
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
	category := repairBudgetHardCeilingFindingCategory
	description := fmt.Sprintf("Automatic repair budget exhausted for %s: consumed %d repairs, current authority limit %d, configured automatic maximum %d. No further repair is authorized by the configured ceiling; approve or abort after reviewing the remaining findings (run %s).", step, decision.Consumed, decision.AuthorityLimit, decision.Limit, runID)
	if decision.ExplicitRepairAvailable {
		category = repairBudgetFindingCategory
		if decision.PolicyConfigured {
			description = fmt.Sprintf("Automatic repair authority exhausted for %s: consumed %d repairs, current authority limit %d, configured automatic maximum %d; requested additional repair %d. No further automatic repair is authorized. An explicit response can authorize exactly one selected repair without increasing the automatic ceiling; run `no-mistakes axi status` from this run's branch for version-matched response guidance (run %s).", step, decision.Consumed, decision.AuthorityLimit, decision.Limit, decision.Consumed+1, runID)
		} else {
			description = fmt.Sprintf("The unconfigured %s gate has no automatic repair authority. An explicit response can authorize exactly one selected repair; another response is required for every later repair (run %s).", step, runID)
		}
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

func repairReconciliationFindings(raw string, step types.StepName, pid *int) string {
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return raw
	}
	description := fmt.Sprintf("Repair execution for %s was interrupted by daemon restart before its outcome was durably observed. No original approve or skip action is available. Abort the run to stop, or explicitly retry only if the repair process never started.", step)
	if pid != nil {
		description = fmt.Sprintf("Repair execution for %s was interrupted by daemon restart after process %d was registered. The process outcome cannot be proven, so replay is refused; abort the run after inspecting the worktree.", step, *pid)
	}
	findings.Items = append(findings.Items, types.Finding{
		ID:          "repair-reconciliation-" + string(step),
		Severity:    types.FindingSeverityError,
		Action:      types.ActionAskUser,
		Category:    repairReconciliationFindingCategory,
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
		if item.Category != repairBudgetFindingCategory && item.Category != repairReconciliationFindingCategory {
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
