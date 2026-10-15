package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// SetRunSurvivedWork records the run's survived-work handoff, replacing any
// earlier one: the latest capture always describes everything outstanding.
func (d *DB) SetRunSurvivedWork(runID string, work *types.SurvivedWork) error {
	if work == nil {
		return d.ClearRunSurvivedWork(runID)
	}
	files, err := json.Marshal(work.Files)
	if err != nil {
		return fmt.Errorf("encode survived work files: %w", err)
	}
	_, err = d.sql.Exec(
		`INSERT INTO run_survived_work (run_id, state, worktree, files_json, ref, base_sha, inspection_error, next, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(run_id) DO UPDATE SET state = excluded.state, worktree = excluded.worktree,
		   files_json = excluded.files_json, ref = excluded.ref, base_sha = excluded.base_sha,
		   inspection_error = excluded.inspection_error, next = excluded.next, updated_at = excluded.updated_at`,
		runID, string(work.State), work.Worktree, string(files), work.Ref, work.BaseSHA, work.InspectionError, work.Next, now(),
	)
	if err != nil {
		return fmt.Errorf("set run survived work: %w", err)
	}
	return nil
}

// GetRunSurvivedWork returns the run's survived-work handoff, or nil.
func (d *DB) GetRunSurvivedWork(runID string) (*types.SurvivedWork, error) {
	var work types.SurvivedWork
	var state, files string
	err := d.sql.QueryRow(
		`SELECT state, worktree, files_json, ref, base_sha, inspection_error, next FROM run_survived_work WHERE run_id = ?`,
		runID,
	).Scan(&state, &work.Worktree, &files, &work.Ref, &work.BaseSHA, &work.InspectionError, &work.Next)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get run survived work: %w", err)
	}
	work.State = types.SurvivedWorkState(state)
	if err := json.Unmarshal([]byte(files), &work.Files); err != nil {
		return nil, fmt.Errorf("decode survived work files: %w", err)
	}
	return &work, nil
}

// MoveRunSurvivedWork hands a run's captured handoff to another run, whose
// worktree it now names, in one statement. Only captured work moves: work in
// place or restored is in the old run's worktree. It reports whether a
// handoff moved.
func (d *DB) MoveRunSurvivedWork(fromRunID, toRunID, worktree string) (bool, error) {
	res, err := d.sql.Exec(
		`UPDATE run_survived_work SET run_id = ?, worktree = ?, updated_at = ? WHERE run_id = ? AND state = ?`,
		toRunID, worktree, now(), fromRunID, string(types.SurvivedWorkCaptured),
	)
	if err != nil {
		return false, fmt.Errorf("move run survived work: %w", err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ClearRunSurvivedWork drops the run's survived-work handoff.
func (d *DB) ClearRunSurvivedWork(runID string) error {
	if _, err := d.sql.Exec(`DELETE FROM run_survived_work WHERE run_id = ?`, runID); err != nil {
		return fmt.Errorf("clear run survived work: %w", err)
	}
	return nil
}
