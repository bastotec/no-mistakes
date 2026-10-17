package db

import (
	"database/sql"
	"errors"
	"fmt"
)

// BindTestPreview records only a run-owned PR. A different URL is never adopted
// on retry; this binding survives restart and the later normal PR step.
func (d *DB) BindTestPreview(runID, prURL string) error {
	if runID == "" || prURL == "" {
		return fmt.Errorf("preview requires run and PR identity")
	}
	_, err := d.sql.Exec(`INSERT INTO run_test_previews(run_id, pr_url) VALUES (?, ?) ON CONFLICT(run_id) DO NOTHING`, runID, prURL)
	if err != nil {
		return err
	}
	bound, err := d.TestPreviewPR(runID)
	if err != nil {
		return err
	}
	if bound != prURL {
		return fmt.Errorf("run already owns a different preview PR")
	}
	return nil
}

func (d *DB) TestPreviewPR(runID string) (string, error) {
	var url string
	err := d.sql.QueryRow(`SELECT pr_url FROM run_test_previews WHERE run_id = ?`, runID).Scan(&url)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return url, err
}

// BranchTestPreviewOwner finds the run whose still-open preview a replacement
// could rediscover by branch. The binding is authoritative even if PRURL was
// never persisted, or a newer run on the branch has no binding.
func (d *DB) BranchTestPreviewOwner(repoID, branch string) (string, error) {
	var owner string
	err := d.sql.QueryRow(`SELECT r.id FROM runs r JOIN run_test_previews p ON p.run_id = r.id
		WHERE r.repo_id = ? AND r.branch = ? AND lower(trim(coalesce(r.pr_state, ''))) NOT IN ('closed', 'merged')
		ORDER BY r.created_at DESC, r.id DESC LIMIT 1`, repoID, branch).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return owner, err
}

func (d *DB) RequireTestPreviewValidation(runID string) error {
	_, err := d.sql.Exec(`UPDATE run_test_previews SET validated_head = NULL WHERE run_id = ?`, runID)
	return err
}

func (d *DB) RecordTestPreviewValidation(runID, head string) error {
	_, err := d.sql.Exec(`UPDATE run_test_previews SET validated_head = ? WHERE run_id = ?`, head, runID)
	return err
}

func (d *DB) TestPreviewNeedsValidation(runID string) (bool, error) {
	var needed bool
	err := d.sql.QueryRow(`SELECT EXISTS(SELECT 1 FROM run_test_previews WHERE run_id = ? AND validated_head IS NULL)`, runID).Scan(&needed)
	return needed, err
}
