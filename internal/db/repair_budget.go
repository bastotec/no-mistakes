package db

import (
	"database/sql"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// RepairBudgetDecision is a durable pre-launch decision. Consumed includes
// every started repair, regardless of whether a user or automatic filter chose
// it. AuthorityLimit is the current configured or response-bounded ceiling.
type RepairBudgetDecision struct {
	Consumed       int
	Limit          int
	AuthorityLimit int
	Granted        bool
	Duplicate      bool
}

// ReserveStepRepair is the single authorization boundary for all fix launches.
// The round whose findings prompted repair is the idempotency key. Reservations
// count before agents/custody mutation, even if execution never returns a round.
// Legacy completed rounds and pending selections are conservatively retained.
func (d *DB) ReserveStepRepair(stepID, roundID string, limit int, explicit bool) (RepairBudgetDecision, error) {
	result := RepairBudgetDecision{Limit: limit, AuthorityLimit: limit}
	tx, err := d.sql.Begin()
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	var stepLimit sql.NullInt64
	if err := tx.QueryRow(`SELECT auto_fix_limit FROM step_results WHERE id = ?`, stepID).Scan(&stepLimit); err != nil {
		return result, err
	}
	if stepLimit.Valid {
		limit = min(limit, int(stepLimit.Int64))
	}
	var pinnedLimit sql.NullInt64
	if err := tx.QueryRow(`SELECT MIN(repair_limit) FROM repair_budget_decisions WHERE step_result_id = ?`, stepID).Scan(&pinnedLimit); err != nil {
		return result, err
	}
	if pinnedLimit.Valid {
		limit = min(limit, int(pinnedLimit.Int64))
	}
	result.Limit = limit
	result.AuthorityLimit = limit
	var authorityLimit sql.NullInt64
	err = tx.QueryRow(`SELECT d.authority_limit FROM repair_budget_decisions d JOIN step_rounds r ON r.id = d.round_id WHERE d.step_result_id = ? AND d.authority_limit IS NOT NULL ORDER BY r.round DESC LIMIT 1`, stepID).Scan(&authorityLimit)
	if err != nil && err != sql.ErrNoRows {
		return result, err
	}
	if authorityLimit.Valid {
		result.AuthorityLimit = min(limit, int(authorityLimit.Int64))
	}
	var latestID string
	if err := tx.QueryRow(`SELECT id FROM step_rounds WHERE step_result_id = ? ORDER BY round DESC LIMIT 1`, stepID).Scan(&latestID); err != nil {
		return result, fmt.Errorf("read repair observation: %w", err)
	}
	if roundID == "" || latestID != roundID {
		return result, fmt.Errorf("repair observation is not the latest round")
	}
	var source string
	var recordedLimit int
	var recordedAuthority sql.NullInt64
	err = tx.QueryRow(`SELECT consumed, repair_limit, authority_limit, source FROM repair_budget_decisions WHERE round_id = ? AND step_result_id = ?`, roundID, stepID).Scan(&result.Consumed, &recordedLimit, &recordedAuthority, &source)
	if err != nil && err != sql.ErrNoRows {
		return result, err
	}
	if err == nil {
		limit = min(limit, recordedLimit)
		result.Limit = limit
		if recordedAuthority.Valid {
			result.AuthorityLimit = min(limit, int(recordedAuthority.Int64))
		}
		if source != "exhausted" {
			result.Duplicate = true
			return result, nil
		}
		if !explicit {
			return result, nil
		}
	}
	// Count actual legacy fixes, not selection_source=auto_fix alone. A selected
	// observation without a following fix result conservatively represents a
	// started attempt, including one interrupted before a reset/revalidation.
	var completed, pending, reserved int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM step_rounds WHERE step_result_id = ? AND trigger_type IN ('auto_fix', 'user_fix')`, stepID).Scan(&completed); err != nil {
		return result, err
	}
	if err := tx.QueryRow(`SELECT COUNT(*) FROM step_rounds r WHERE r.step_result_id = ? AND r.selection_source IN ('auto_fix', 'user') AND r.selected_finding_ids IS NOT NULL AND r.selected_finding_ids != '[]' AND COALESCE((SELECT n.trigger_type FROM step_rounds n WHERE n.step_result_id = r.step_result_id AND n.round > r.round ORDER BY n.round LIMIT 1), '') NOT IN ('auto_fix', 'user_fix')`, stepID).Scan(&pending); err != nil {
		return result, err
	}
	if err := tx.QueryRow(`SELECT COALESCE(MAX(consumed), 0) FROM repair_budget_decisions WHERE step_result_id = ? AND source != 'exhausted'`, stepID).Scan(&reserved); err != nil {
		return result, err
	}
	result.Consumed = max(completed+pending, reserved)
	if source == "exhausted" && recordedAuthority.Valid {
		result.AuthorityLimit = min(limit, int(recordedAuthority.Int64))
	}
	if explicit {
		result.AuthorityLimit = min(limit, result.Consumed+1)
	}
	source = "exhausted"
	if result.Consumed < result.AuthorityLimit {
		result.Granted = true
		result.Consumed++
		source = RoundSelectionSourceAutoFix
		if explicit {
			source = RoundSelectionSourceUser
		}
	}
	if _, err := tx.Exec(`INSERT INTO repair_budget_decisions(round_id, step_result_id, consumed, repair_limit, authority_limit, source) VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(round_id) DO UPDATE SET consumed = excluded.consumed, repair_limit = excluded.repair_limit, authority_limit = excluded.authority_limit, source = excluded.source`, roundID, stepID, result.Consumed, limit, result.AuthorityLimit, source); err != nil {
		return result, err
	}
	if result.Granted {
		ts := now()
		if _, err := tx.Exec(`UPDATE step_results SET status = ?, round_started_at = ?, last_activity_at = ?, last_activity = ?, auto_fix_limit = CASE WHEN auto_fix_limit IS NULL OR ? < auto_fix_limit THEN ? ELSE auto_fix_limit END, override_reason = NULL WHERE id = ?`, types.StepStatusFixing, ts, ts, "repair authorized", limit, limit, stepID); err != nil {
			return result, err
		}
	}
	if err := tx.Commit(); err != nil {
		return RepairBudgetDecision{}, err
	}
	return result, nil
}

// SetRepairBudgetFindings keeps the parked round and gate payload identical for
// recovery. The decision itself is already durable before this publication.
func (d *DB) SetRepairBudgetFindings(roundID, findings string) error {
	_, err := d.sql.Exec(`UPDATE step_rounds SET findings_json = ? WHERE id = ?`, findings, roundID)
	return err
}
