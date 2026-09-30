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
	Consumed                int
	Limit                   int
	AuthorityLimit          int
	ExplicitRepairAvailable bool
	Granted                 bool
	Duplicate               bool
}

// ReserveStepRepair is the single authorization boundary for all fix launches.
// The round whose findings prompted repair is the idempotency key. Reservations
// count before agents/custody mutation, even if execution never returns a round.
// Legacy completed rounds and pending selections are conservatively retained.
func (d *DB) ReserveStepRepair(stepID, roundID string, limit int, explicit bool) (RepairBudgetDecision, error) {
	return d.reserveStepRepair(stepID, roundID, limit, explicit, false, nil, nil)
}

func (d *DB) ReserveStepRepairWithSelection(stepID, roundID string, limit int, selectedFindingIDs *string) (RepairBudgetDecision, error) {
	return d.reserveStepRepair(stepID, roundID, limit, false, false, selectedFindingIDs, nil)
}

func (d *DB) AuthorizeStepRepair(stepID, roundID string, limit int, selectedFindingIDs, userFindingsJSON *string) (RepairBudgetDecision, error) {
	return d.reserveStepRepair(stepID, roundID, limit, true, true, selectedFindingIDs, userFindingsJSON)
}

func (d *DB) reserveStepRepair(stepID, roundID string, limit int, explicit, replayAuthorization bool, selectedFindingIDs, userFindingsJSON *string) (RepairBudgetDecision, error) {
	result := RepairBudgetDecision{Limit: limit, AuthorityLimit: limit}
	tx, err := d.sql.Begin()
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	var stepLimit sql.NullInt64
	var stepStatus types.StepStatus
	var limitProvenance string
	if err := tx.QueryRow(`SELECT auto_fix_limit, status, auto_fix_limit_provenance FROM step_results WHERE id = ?`, stepID).Scan(&stepLimit, &stepStatus, &limitProvenance); err != nil {
		return result, err
	}
	switch limitProvenance {
	case "initialized":
		if stepLimit.Valid {
			limit = min(limit, int(stepLimit.Int64))
		}
	case "legacy_normalized":
		limit = 0
	default:
		switch stepStatus {
		case types.StepStatusPending:
			if _, err := tx.Exec(`UPDATE step_results SET auto_fix_limit = ?, auto_fix_limit_provenance = 'initialized' WHERE id = ? AND auto_fix_limit_provenance = 'legacy_unknown'`, limit, stepID); err != nil {
				return result, err
			}
		case types.StepStatusRunning, types.StepStatusAwaitingApproval, types.StepStatusFixing, types.StepStatusFixReview:
			limit = 0
			if _, err := tx.Exec(`UPDATE step_results SET auto_fix_limit = 0, auto_fix_limit_provenance = 'legacy_normalized' WHERE id = ? AND auto_fix_limit_provenance = 'legacy_unknown'`, stepID); err != nil {
				return result, err
			}
		default:
			limit = 0
		}
	}
	authorityLimit := limit
	var pinnedLimit sql.NullInt64
	if err := tx.QueryRow(`SELECT MIN(repair_limit) FROM repair_budget_decisions WHERE step_result_id = ?`, stepID).Scan(&pinnedLimit); err != nil {
		return result, err
	}
	if pinnedLimit.Valid {
		limit = min(limit, int(pinnedLimit.Int64))
	}
	authorityLimit = min(authorityLimit, limit)
	result.Limit = limit
	result.AuthorityLimit = authorityLimit
	var recordedLatestAuthority sql.NullInt64
	err = tx.QueryRow(`SELECT d.authority_limit FROM repair_budget_decisions d JOIN step_rounds r ON r.id = d.round_id WHERE d.step_result_id = ? AND d.authority_limit IS NOT NULL ORDER BY r.round DESC LIMIT 1`, stepID).Scan(&recordedLatestAuthority)
	if err != nil && err != sql.ErrNoRows {
		return result, err
	}
	if recordedLatestAuthority.Valid {
		result.AuthorityLimit = int(recordedLatestAuthority.Int64)
	}
	var latestID string
	if err := tx.QueryRow(`SELECT id FROM step_rounds WHERE step_result_id = ? ORDER BY round DESC LIMIT 1`, stepID).Scan(&latestID); err != nil {
		return result, fmt.Errorf("read repair observation: %w", err)
	}
	if roundID == "" || latestID != roundID {
		return result, fmt.Errorf("repair observation is not the latest round")
	}
	var source string
	var dispatchState sql.NullString
	var recordedLimit int
	var recordedAuthority sql.NullInt64
	err = tx.QueryRow(`SELECT consumed, repair_limit, authority_limit, source, dispatch_state FROM repair_budget_decisions WHERE round_id = ? AND step_result_id = ?`, roundID, stepID).Scan(&result.Consumed, &recordedLimit, &recordedAuthority, &source, &dispatchState)
	if err != nil && err != sql.ErrNoRows {
		return result, err
	}
	if err == nil {
		limit = min(limit, recordedLimit)
		result.Limit = limit
		if recordedAuthority.Valid {
			result.AuthorityLimit = int(recordedAuthority.Int64)
		}
		if source != "exhausted" {
			result.Duplicate = true
			result.Granted = replayAuthorization && explicit && source == RoundSelectionSourceUser && dispatchState.Valid && dispatchState.String == "fix_authorized"
			return result, nil
		}
		if !explicit {
			result.ExplicitRepairAvailable = true
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
		result.AuthorityLimit = int(recordedAuthority.Int64)
	}
	if explicit {
		result.AuthorityLimit = min(limit, result.Consumed+1)
		if replayAuthorization {
			result.AuthorityLimit = result.Consumed + 1
		}
	}
	result.ExplicitRepairAvailable = true
	source = "exhausted"
	authorizedLimit := min(limit, result.AuthorityLimit)
	if replayAuthorization && explicit {
		authorizedLimit = result.AuthorityLimit
	}
	if result.Consumed < authorizedLimit {
		result.Granted = true
		result.ExplicitRepairAvailable = false
		result.Consumed++
		source = RoundSelectionSourceAutoFix
		if explicit {
			source = RoundSelectionSourceUser
		}
	}
	dispatch := any(nil)
	if result.Granted {
		dispatch = "claimed"
		if explicit {
			dispatch = "fix_authorized"
		}
	}
	if _, err := tx.Exec(`INSERT INTO repair_budget_decisions(round_id, step_result_id, consumed, repair_limit, authority_limit, source, dispatch_state) VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT(round_id) DO UPDATE SET consumed = excluded.consumed, repair_limit = excluded.repair_limit, authority_limit = excluded.authority_limit, source = excluded.source, dispatch_state = excluded.dispatch_state`, roundID, stepID, result.Consumed, limit, result.AuthorityLimit, source, dispatch); err != nil {
		return result, err
	}
	if result.Granted {
		ts := now()
		if _, err := tx.Exec(`INSERT OR IGNORE INTO repair_invocations(id, round_id, step_result_id, ordinal, state, created_at, updated_at) VALUES (?, ?, ?, 0, 'prepared', ?, ?)`, newID(), roundID, stepID, ts, ts); err != nil {
			return result, err
		}
	}
	if result.Granted && explicit {
		if _, err := tx.Exec(`UPDATE step_rounds SET selected_finding_ids = ?, selection_source = ?, user_findings_json = ? WHERE id = ?`, selectedFindingIDs, RoundSelectionSourceUser, userFindingsJSON, roundID); err != nil {
			return result, err
		}
		if _, err := tx.Exec(`UPDATE step_results SET auto_fix_limit_provenance = CASE WHEN auto_fix_limit_provenance = 'legacy_normalized' THEN 'initialized' ELSE auto_fix_limit_provenance END WHERE id = ?`, stepID); err != nil {
			return result, err
		}
	}
	if result.Granted && !explicit {
		if selectedFindingIDs != nil {
			if _, err := tx.Exec(`UPDATE step_rounds SET selected_finding_ids = ?, selection_source = ? WHERE id = ?`, selectedFindingIDs, RoundSelectionSourceAutoFix, roundID); err != nil {
				return result, err
			}
		}
		ts := now()
		if _, err := tx.Exec(`UPDATE step_results SET status = ?, round_started_at = ?, last_activity_at = ?, last_activity = ?, override_reason = NULL, auto_fix_limit_provenance = CASE WHEN auto_fix_limit_provenance = 'legacy_normalized' THEN 'initialized' ELSE auto_fix_limit_provenance END WHERE id = ?`, types.StepStatusFixing, ts, ts, "repair authorized", stepID); err != nil {
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

func (d *DB) HasRecoverableRepairDispatch(runID string) (bool, error) {
	var found bool
	err := d.sql.QueryRow(`SELECT EXISTS(
		SELECT 1 FROM repair_invocations i
		JOIN step_results s ON s.id = i.step_result_id
		WHERE s.run_id = ? AND i.state IN ('prepared', 'registered', 'fixer_started', 'terminal')
		UNION ALL
		SELECT 1 FROM repair_budget_decisions d
		JOIN step_results s ON s.id = d.step_result_id
		WHERE s.run_id = ? AND d.dispatch_state IN ('claimed', 'started', 'repair_started_unresolved')
		AND NOT EXISTS (SELECT 1 FROM repair_invocations i WHERE i.round_id = d.round_id)
	)`, runID, runID).Scan(&found)
	return found, err
}

func (d *DB) HasStartedRepairDispatch(runID string) (bool, error) {
	var found bool
	err := d.sql.QueryRow(`SELECT EXISTS(
		SELECT 1 FROM repair_invocations i
		JOIN step_results s ON s.id = i.step_result_id
		WHERE s.run_id = ? AND i.state IN ('registered', 'fixer_started')
		UNION ALL
		SELECT 1 FROM repair_budget_decisions d
		JOIN step_results s ON s.id = d.step_result_id
		WHERE s.run_id = ? AND d.dispatch_state = 'started'
		AND NOT EXISTS (SELECT 1 FROM repair_invocations i WHERE i.round_id = d.round_id)
	)`, runID, runID).Scan(&found)
	return found, err
}

func (d *DB) ClaimStepRepair(stepID, roundID string) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state sql.NullString
	if err := tx.QueryRow(`SELECT dispatch_state FROM repair_budget_decisions WHERE round_id = ? AND step_result_id = ?`, roundID, stepID).Scan(&state); err != nil {
		return fmt.Errorf("read repair authorization: %w", err)
	}
	if !state.Valid || (state.String != "fix_authorized" && state.String != "claimed") {
		return fmt.Errorf("repair authorization is not dispatchable")
	}
	if state.String == "fix_authorized" {
		if _, err := tx.Exec(`UPDATE repair_budget_decisions SET dispatch_state = 'claimed' WHERE round_id = ? AND dispatch_state = 'fix_authorized'`, roundID); err != nil {
			return err
		}
	}
	ts := now()
	if _, err := tx.Exec(`UPDATE step_results SET status = ?, round_started_at = ?, last_activity_at = ?, last_activity = ?, override_reason = NULL WHERE id = ?`, types.StepStatusFixing, ts, ts, "repair claimed", stepID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE runs SET
		parked_ms = COALESCE(parked_ms, 0) + CASE WHEN awaiting_agent_since IS NOT NULL AND ? > awaiting_agent_since THEN (? - awaiting_agent_since) * 1000 ELSE 0 END,
		awaiting_agent_since = NULL, updated_at = ?
		WHERE id = (SELECT run_id FROM step_results WHERE id = ?)`, ts, ts, ts, stepID); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) BindStepRepairProcess(stepID, roundID, activity string, pid int) error {
	invocation, err := d.RegisterRepairInvocation(stepID, roundID, 0, "", "", "", pid)
	if err != nil {
		return err
	}
	return d.BindRepairInvocationProcess(invocation.ID, activity, pid)
}

func (d *DB) CompleteStepRepair(stepID, roundID string) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE repair_invocations SET state = 'consumed', updated_at = ? WHERE round_id = ? AND step_result_id = ? AND state = 'terminal'`, now(), roundID, stepID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE repair_budget_decisions SET dispatch_state = 'completed' WHERE round_id = ? AND step_result_id = ? AND dispatch_state IN ('claimed', 'started')`, roundID, stepID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE step_results SET agent_pid = NULL WHERE id = ?`, stepID); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) RetryUnstartedStepRepair(stepID, roundID string) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state sql.NullString
	var pid sql.NullInt64
	if err := tx.QueryRow(`SELECT d.dispatch_state, s.agent_pid FROM repair_budget_decisions d JOIN step_results s ON s.id = d.step_result_id WHERE d.round_id = ? AND d.step_result_id = ?`, roundID, stepID).Scan(&state, &pid); err != nil {
		return fmt.Errorf("read unresolved repair: %w", err)
	}
	if !state.Valid || state.String != "repair_started_unresolved" {
		return fmt.Errorf("repair is not awaiting reconciliation")
	}
	var invocationEvidence bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM repair_invocations WHERE round_id = ? AND state != 'prepared')`, roundID).Scan(&invocationEvidence); err != nil {
		return err
	}
	if invocationEvidence {
		return fmt.Errorf("repair invocation was registered; abort rather than replaying it")
	}
	if pid.Valid {
		return fmt.Errorf("repair process identity %d was registered; abort rather than replaying it", pid.Int64)
	}
	if _, err := tx.Exec(`UPDATE repair_budget_decisions SET dispatch_state = 'fix_authorized' WHERE round_id = ? AND dispatch_state = 'repair_started_unresolved'`, roundID); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) StepRepairBudgetStatus(stepID string, configuredLimit int) (RepairBudgetDecision, error) {
	result := RepairBudgetDecision{Limit: configuredLimit, AuthorityLimit: configuredLimit}
	var stepLimit sql.NullInt64
	if err := d.sql.QueryRow(`SELECT auto_fix_limit FROM step_results WHERE id = ?`, stepID).Scan(&stepLimit); err != nil {
		return result, err
	}
	if stepLimit.Valid {
		result.Limit = min(result.Limit, int(stepLimit.Int64))
		result.AuthorityLimit = result.Limit
	}
	var reserved, completed int
	if err := d.sql.QueryRow(`SELECT COALESCE(MAX(consumed), 0) FROM repair_budget_decisions WHERE step_result_id = ? AND source != 'exhausted'`, stepID).Scan(&reserved); err != nil {
		return result, err
	}
	if err := d.sql.QueryRow(`SELECT COUNT(*) FROM step_rounds WHERE step_result_id = ? AND trigger_type IN ('auto_fix', 'user_fix')`, stepID).Scan(&completed); err != nil {
		return result, err
	}
	result.Consumed = max(reserved, completed)
	result.ExplicitRepairAvailable = true
	return result, nil
}

func (d *DB) RestoreLegacyRepairAuthorization(stepID, roundID string) (string, error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var state sql.NullString
	var source string
	if err := tx.QueryRow(`SELECT source, dispatch_state FROM repair_budget_decisions WHERE round_id = ? AND step_result_id = ?`, roundID, stepID).Scan(&source, &state); err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", err
	}
	var invocationState sql.NullString
	if err := tx.QueryRow(`SELECT state FROM repair_invocations WHERE round_id = ? AND step_result_id = ? ORDER BY ordinal DESC LIMIT 1`, roundID, stepID).Scan(&invocationState); err != nil && err != sql.ErrNoRows {
		return "", err
	}
	if invocationState.Valid {
		switch invocationState.String {
		case "prepared", "terminal":
			state = sql.NullString{String: "fix_authorized", Valid: true}
			if _, err := tx.Exec(`UPDATE repair_budget_decisions SET dispatch_state = 'fix_authorized' WHERE round_id = ?`, roundID); err != nil {
				return "", err
			}
		case "registered", "fixer_started":
			state = sql.NullString{String: "repair_started_unresolved", Valid: true}
			if _, err := tx.Exec(`UPDATE repair_budget_decisions SET dispatch_state = 'repair_started_unresolved' WHERE round_id = ?`, roundID); err != nil {
				return "", err
			}
		}
	}
	if state.Valid && (state.String == "claimed" || state.String == "started") {
		state.String = "repair_started_unresolved"
		if _, err := tx.Exec(`UPDATE repair_budget_decisions SET dispatch_state = ? WHERE round_id = ?`, state.String, roundID); err != nil {
			return "", err
		}
		if _, err := tx.Exec(`UPDATE step_results SET status = ?, last_activity_at = ?, last_activity = ? WHERE id = ?`, types.StepStatusFixing, now(), "repair outcome unresolved after daemon restart", stepID); err != nil {
			return "", err
		}
	}
	if source == RoundSelectionSourceUser && !state.Valid {
		state = sql.NullString{String: "fix_authorized", Valid: true}
		if _, err := tx.Exec(`UPDATE repair_budget_decisions SET dispatch_state = ? WHERE round_id = ?`, state.String, roundID); err != nil {
			return "", err
		}
		if _, err := tx.Exec(`UPDATE step_results SET status = ? WHERE id = ? AND status = ?`, types.StepStatusAwaitingApproval, stepID, types.StepStatusFixing); err != nil {
			return "", err
		}
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return state.String, nil
}
