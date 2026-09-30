package db

import (
	"database/sql"
	"fmt"
)

type RepairInvocation struct {
	ID             string
	State          string
	ResultJSON     []byte
	ResultPresent  bool
	ErrorClass     *string
	ErrorText      *string
	LifecyclePhase string
}

func (d *DB) RegisterRepairInvocation(stepID, roundID string, ordinal int, worktree, agentName, purpose string, wrapperPID int) (*RepairInvocation, error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var inv RepairInvocation
	var resultJSON []byte
	var resultPresent int
	var errorClass, errorText sql.NullString
	if err := tx.QueryRow(`SELECT id, state, result_json, result_present, error_class, error_text, lifecycle_phase FROM repair_invocations WHERE round_id = ? AND step_result_id = ? AND ordinal = ?`, roundID, stepID, ordinal).Scan(&inv.ID, &inv.State, &resultJSON, &resultPresent, &errorClass, &errorText, &inv.LifecyclePhase); err != nil {
		if err != sql.ErrNoRows || ordinal == 0 {
			return nil, fmt.Errorf("read repair invocation: %w", err)
		}
		ts := now()
		inv = RepairInvocation{ID: newID(), State: "prepared", LifecyclePhase: "repair"}
		if _, err := tx.Exec(`INSERT INTO repair_invocations(id, round_id, step_result_id, ordinal, state, created_at, updated_at) VALUES (?, ?, ?, ?, 'prepared', ?, ?)`, inv.ID, roundID, stepID, ordinal, ts, ts); err != nil {
			return nil, err
		}
	}
	inv.ResultJSON = resultJSON
	inv.ResultPresent = resultPresent != 0
	if errorClass.Valid {
		inv.ErrorClass = &errorClass.String
	}
	if errorText.Valid {
		inv.ErrorText = &errorText.String
	}
	if inv.State == "terminal" {
		return &inv, tx.Commit()
	}
	if inv.State != "prepared" {
		return nil, fmt.Errorf("repair invocation is %s; refusing to replay it", inv.State)
	}
	if _, err := tx.Exec(`UPDATE repair_invocations SET state = 'registered', worktree = ?, agent = ?, purpose = ?, wrapper_pid = NULLIF(?, 0), updated_at = ? WHERE id = ? AND state = 'prepared'`, worktree, agentName, purpose, wrapperPID, now(), inv.ID); err != nil {
		return nil, err
	}
	inv.State = "registered"
	return &inv, tx.Commit()
}

func (d *DB) SetRepairInvocationFiles(invocationID, descriptorPath, resultPath string) error {
	result, err := d.sql.Exec(`UPDATE repair_invocations SET descriptor_path = ?, result_path = ?, updated_at = ? WHERE id = ? AND state = 'registered'`, descriptorPath, resultPath, now(), invocationID)
	if err != nil {
		return err
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		return fmt.Errorf("repair invocation files were not accepted")
	}
	return nil
}

func (d *DB) BindRepairInvocationWrapper(invocationID, token string, pid int) error {
	result, err := d.sql.Exec(`UPDATE repair_invocations SET wrapper_pid = ?, wrapper_token = ?, updated_at = ? WHERE id = ? AND state = 'registered' AND (wrapper_token IS NULL OR wrapper_token = ?)`, pid, token, now(), invocationID, token)
	if err != nil {
		return err
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		return fmt.Errorf("repair wrapper identity was not accepted")
	}
	return nil
}

func (d *DB) GetRepairInvocation(invocationID string) (*RepairInvocation, error) {
	var inv RepairInvocation
	var resultPresent int
	var errorClass, errorText sql.NullString
	err := d.sql.QueryRow(`SELECT id, state, result_json, result_present, error_class, error_text, lifecycle_phase FROM repair_invocations WHERE id = ?`, invocationID).Scan(&inv.ID, &inv.State, &inv.ResultJSON, &resultPresent, &errorClass, &errorText, &inv.LifecyclePhase)
	if err != nil {
		return nil, err
	}
	inv.ResultPresent = resultPresent != 0
	if errorClass.Valid {
		inv.ErrorClass = &errorClass.String
	}
	if errorText.Valid {
		inv.ErrorText = &errorText.String
	}
	return &inv, nil
}

func (d *DB) RepairInvocationFiles(invocationID string) (string, string, error) {
	var descriptorPath, resultPath string
	err := d.sql.QueryRow(`SELECT descriptor_path, result_path FROM repair_invocations WHERE id = ?`, invocationID).Scan(&descriptorPath, &resultPath)
	return descriptorPath, resultPath, err
}

func (d *DB) RepairInvocationWrapperIdentity(stepID, roundID string) (string, int, string, error) {
	var token, resultPath string
	var pid int
	err := d.sql.QueryRow(`SELECT wrapper_token, wrapper_pid, result_path FROM repair_invocations WHERE step_result_id = ? AND round_id = ? ORDER BY ordinal DESC LIMIT 1`, stepID, roundID).Scan(&token, &pid, &resultPath)
	return token, pid, resultPath, err
}

func (d *DB) MarkRepairInvocationUnresolved(stepID, roundID string) error {
	_, err := d.sql.Exec(`UPDATE repair_budget_decisions SET dispatch_state = 'repair_started_unresolved' WHERE step_result_id = ? AND round_id = ?`, stepID, roundID)
	return err
}

func (d *DB) BindRepairInvocationProcess(invocationID, activity string, pid int) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE repair_invocations SET state = 'fixer_started', fixer_pid = ?, updated_at = ? WHERE id = ? AND state IN ('registered', 'fixer_started')`, pid, now(), invocationID)
	if err != nil {
		return err
	}
	if changed, err := result.RowsAffected(); err != nil {
		return err
	} else if changed != 1 {
		return fmt.Errorf("repair invocation is not registered")
	}
	if _, err := tx.Exec(`UPDATE repair_budget_decisions SET dispatch_state = 'started' WHERE round_id = (SELECT round_id FROM repair_invocations WHERE id = ?) AND dispatch_state IN ('claimed', 'started')`, invocationID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE step_results SET last_activity_at = ?, last_activity = ?, agent_pid = ? WHERE id = (SELECT step_result_id FROM repair_invocations WHERE id = ?)`, now(), activity, pid, invocationID); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) FinishRepairInvocation(invocationID string, resultJSON []byte, resultPresent bool, errorClass, errorText *string) error {
	present := 0
	if resultPresent {
		present = 1
	}
	result, err := d.sql.Exec(`UPDATE repair_invocations SET state = 'terminal', result_json = ?, result_present = ?, error_class = ?, error_text = ?, updated_at = ? WHERE id = ? AND state IN ('registered', 'fixer_started')`, resultJSON, present, errorClass, errorText, now(), invocationID)
	if err != nil {
		return err
	}
	if changed, err := result.RowsAffected(); err != nil {
		return err
	} else if changed != 1 {
		return fmt.Errorf("repair invocation terminal result was not accepted")
	}
	return nil
}

func (d *DB) MarkRepairPostMonitor(stepID, roundID string) error {
	result, err := d.sql.Exec(`UPDATE repair_invocations SET lifecycle_phase = 'post_repair_monitor', updated_at = ? WHERE step_result_id = ? AND round_id = ? AND state = 'terminal'`, now(), stepID, roundID)
	if err != nil {
		return err
	}
	if changed, err := result.RowsAffected(); err != nil {
		return err
	} else if changed < 1 {
		return nil
	}
	return nil
}

func (d *DB) RepairInvocationLifecyclePhase(stepID, roundID string) (string, error) {
	var phase string
	err := d.sql.QueryRow(`SELECT lifecycle_phase FROM repair_invocations WHERE round_id = ? AND step_result_id = ? ORDER BY ordinal DESC LIMIT 1`, roundID, stepID).Scan(&phase)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return phase, err
}

func (d *DB) RepairInvocationRecoveryState(stepID, roundID string) (string, *int, error) {
	var state string
	var pid sql.NullInt64
	err := d.sql.QueryRow(`SELECT state, COALESCE(fixer_pid, wrapper_pid) FROM repair_invocations WHERE round_id = ? AND step_result_id = ? ORDER BY ordinal DESC LIMIT 1`, roundID, stepID).Scan(&state, &pid)
	if err == sql.ErrNoRows {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	var processID *int
	if pid.Valid {
		value := int(pid.Int64)
		processID = &value
	}
	return state, processID, nil
}
