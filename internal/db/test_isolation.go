package db

import (
	"context"
	"fmt"
)

// EnableTestIsolation is launch-only authority, never inferred from repo config
// or copied to a rerun. Recovery reads this immutable per-run pin.
func (d *DB) EnableTestIsolation(runID string) error {
	_, err := d.sql.Exec(`INSERT INTO run_test_isolation(run_id) VALUES (?)`, runID)
	return err
}
func (d *DB) TestIsolationEnabled(runID string) (bool, error) {
	var n int
	err := d.sql.QueryRow(`SELECT COUNT(*) FROM run_test_isolation WHERE run_id=?`, runID).Scan(&n)
	return n == 1, err
}

// ExportTestSnapshot copies only this run's advisory records. Never mount the
// live daemon database inside Test: it contains authority for unrelated runs.
// Worker writes are disposable; the parent separately acknowledges head updates.
func (d *DB) ExportTestSnapshot(path, runID string) error {
	target, err := Open(path)
	if err != nil {
		return err
	}
	if err = target.Close(); err != nil {
		return err
	}
	ctx := context.Background()
	conn, err := d.sql.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, `ATTACH DATABASE ? AS test_snapshot`, path); err != nil {
		return err
	}
	defer conn.ExecContext(ctx, `DETACH DATABASE test_snapshot`)
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	copies := []string{
		`INSERT INTO test_snapshot.repos (id,working_path,upstream_url,fork_url,default_branch,created_at) SELECT id,working_path,'',NULL,default_branch,created_at FROM main.repos WHERE id=(SELECT repo_id FROM main.runs WHERE id=?)`,
		`INSERT INTO test_snapshot.runs SELECT * FROM main.runs WHERE id=?`,
		`INSERT INTO test_snapshot.step_results SELECT * FROM main.step_results WHERE run_id=?`,
		`INSERT INTO test_snapshot.step_rounds
		 (id,step_result_id,round,trigger_type,findings_json,reviewed_head_sha,starting_head_sha,trusted_config_sha,user_findings_json,selected_finding_ids,selection_source,fix_summary,repair_published,duration_ms,created_at)
		 SELECT id,step_result_id,round,trigger_type,findings_json,reviewed_head_sha,starting_head_sha,trusted_config_sha,user_findings_json,selected_finding_ids,selection_source,fix_summary,repair_published,duration_ms,created_at
		 FROM main.step_rounds WHERE step_result_id IN (SELECT id FROM main.step_results WHERE run_id=?)`,
		`INSERT INTO test_snapshot.run_test_previews SELECT * FROM main.run_test_previews WHERE run_id=?`,
	}
	for _, query := range copies {
		if _, err = tx.ExecContext(ctx, query, runID); err != nil {
			return fmt.Errorf("export Test records: %w", err)
		}
	}
	return tx.Commit()
}
