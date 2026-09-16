package db

import (
	"database/sql"
	"errors"
	"fmt"
)

// AssociateRunWithExistingPR makes a validated target durable. The run's pin
// and the branch's canonical association are one fact, so they are one write:
// a daemon exit can never leave a run publishing to a pull request its own
// branch no longer maps to, which is what would let a later unflagged launch
// discover or create another one. Nothing here is written before the forge has
// proven the target, so a refused launch leaves the branch mapped exactly
// where it already was.
//
// The pull request's own base branch rides the same write. `pr_base_branch`
// otherwise means an operator `--base-branch` override, and that is exactly how
// a later rerun reads it (`explicitRunTarget`), so a base persisted without the
// pin beside it would be inherited as an override the operator never made.
func (d *DB) AssociateRunWithExistingPR(runID, repoID, branch, prURL, prBaseBranch string) error {
	ts := now()
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("associate run with existing pr: begin: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.Exec(
		`UPDATE runs SET existing_pr_url = ?, pr_url = ?, pr_base_branch = ?, updated_at = ? WHERE id = ?`,
		prURL, prURL, prBaseBranch, ts, runID,
	)
	if err != nil {
		return fmt.Errorf("pin run to existing pr: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("pin run to existing pr: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("pin run to existing pr: run %s not found", runID)
	}
	if err := setBranchPRTarget(tx, repoID, branch, prURL, ts); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("associate run with existing pr: commit: %w", err)
	}
	return nil
}

// setBranchPRTarget records the canonical pull request a branch publishes to.
// It outlives the run that established it: later launches, push-hook runs and
// CI repairs reuse it until it is replaced by another explicit association or
// retired. It is transaction-scoped on purpose - the only writer is
// AssociateRunWithExistingPR, which pins the same pull request on the run in
// the same transaction.
func setBranchPRTarget(tx *sql.Tx, repoID, branch, prURL string, ts int64) error {
	_, err := tx.Exec(
		`INSERT INTO branch_pr_targets (repo_id, branch, pr_url, created_at, updated_at) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(repo_id, branch) DO UPDATE SET pr_url = excluded.pr_url, updated_at = excluded.updated_at`,
		repoID, branch, prURL, ts, ts,
	)
	if err != nil {
		return fmt.Errorf("set branch pr target: %w", err)
	}
	return nil
}

// GetBranchPRTarget returns the branch's canonical pull request, or "" when the
// branch has none and ordinary repository-scoped discovery applies.
func (d *DB) GetBranchPRTarget(repoID, branch string) (string, error) {
	var prURL string
	err := d.sql.QueryRow(`SELECT pr_url FROM branch_pr_targets WHERE repo_id = ? AND branch = ?`, repoID, branch).Scan(&prURL)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get branch pr target: %w", err)
	}
	return prURL, nil
}

// DeleteBranchPRTarget retires a branch's association and reports whether one
// was recorded, so a caller can tell the operator nothing was retired.
func (d *DB) DeleteBranchPRTarget(repoID, branch string) (string, error) {
	existing, err := d.GetBranchPRTarget(repoID, branch)
	if err != nil {
		return "", err
	}
	if existing == "" {
		return "", nil
	}
	if _, err := d.sql.Exec(`DELETE FROM branch_pr_targets WHERE repo_id = ? AND branch = ?`, repoID, branch); err != nil {
		return "", fmt.Errorf("retire branch pr target: %w", err)
	}
	return existing, nil
}
