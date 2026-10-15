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

// CopyRunSurvivedWork gives a rerun the captured handoff of the run it
// reruns, naming the rerun's worktree. The original row stays, so a rerun
// whose setup fails strands nothing; ClearSurvivedWorkRef retires every copy
// once the work is consumed. Only captured work is copied: work in place or
// restored is in the old run's worktree.
func (d *DB) CopyRunSurvivedWork(fromRunID, toRunID, worktree string) error {
	_, err := d.sql.Exec(
		`INSERT INTO run_survived_work (run_id, state, worktree, files_json, ref, base_sha, inspection_error, next, updated_at)
		 SELECT ?, state, ?, files_json, ref, base_sha, inspection_error, next, ? FROM run_survived_work WHERE run_id = ? AND state = ?
		 ON CONFLICT(run_id) DO UPDATE SET state = excluded.state, worktree = excluded.worktree,
		   files_json = excluded.files_json, ref = excluded.ref, base_sha = excluded.base_sha,
		   inspection_error = excluded.inspection_error, next = excluded.next, updated_at = excluded.updated_at`,
		toRunID, worktree, now(), fromRunID, string(types.SurvivedWorkCaptured),
	)
	if err != nil {
		return fmt.Errorf("copy run survived work: %w", err)
	}
	return nil
}

// ClearSurvivedWorkRef drops every run's handoff that points at ref: copies
// a rerun took (see CopyRunSurvivedWork) describe the same work.
func (d *DB) ClearSurvivedWorkRef(ref string) error {
	if ref == "" {
		return nil
	}
	if _, err := d.sql.Exec(`DELETE FROM run_survived_work WHERE ref = ?`, ref); err != nil {
		return fmt.Errorf("clear survived work ref: %w", err)
	}
	return nil
}

// ClearRunSurvivedWork drops the run's survived-work handoff.
func (d *DB) ClearRunSurvivedWork(runID string) error {
	if _, err := d.sql.Exec(`DELETE FROM run_survived_work WHERE run_id = ?`, runID); err != nil {
		return fmt.Errorf("clear run survived work: %w", err)
	}
	return nil
}
