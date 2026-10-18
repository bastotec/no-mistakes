package db

import (
	"context"
	"fmt"
)

// FinishIsolatedTestRepairInvocation is the completion boundary for a reaped
// whole-Test worker. Its terminal result and removal of the active step PID
// must become durable together: recovery must never see terminal replay paired
// with an apparently still-running isolated worker. Native repairs are unchanged.
func (d *DB) FinishIsolatedTestRepairInvocation(ctx context.Context, id string, resultJSON []byte, present bool, errorClass, errorText *string) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var step string
	var pid int
	if err := tx.QueryRowContext(ctx, `SELECT step_result_id, fixer_pid FROM repair_invocations WHERE id = ? AND state = 'fixer_started' AND purpose = 'isolated-test-step'`, id).Scan(&step, &pid); err != nil {
		return fmt.Errorf("isolated repair ownership: %w", err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE step_results SET agent_pid = NULL WHERE id = ? AND agent_pid = ?`, step, pid)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return fmt.Errorf("isolated repair no longer owns the active step PID")
	}
	nPresent := 0
	if present {
		nPresent = 1
	}
	result, err = tx.ExecContext(ctx, `UPDATE repair_invocations SET state = 'terminal', result_json = ?, result_present = ?, error_class = ?, error_text = ?, updated_at = ? WHERE id = ? AND state = 'fixer_started'`, resultJSON, nPresent, errorClass, errorText, now(), id)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return fmt.Errorf("isolated repair terminal result was not accepted")
	}
	return tx.Commit()
}
