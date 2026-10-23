package db

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// PinDocumentTimeout records immutable launch authority. New runs and reruns
// do not inherit it; recovery reads the same pin as initial execution.
func (d *DB) PinDocumentTimeout(runID string, timeout time.Duration) error {
	if timeout <= 0 {
		return fmt.Errorf("Document timeout must be positive")
	}
	_, err := d.sql.Exec(`INSERT INTO run_document_timeout(run_id, timeout_ns) VALUES (?, ?)`, runID, int64(timeout))
	return err
}

func (d *DB) DocumentTimeout(runID string) (time.Duration, error) {
	var ns int64
	err := d.sql.QueryRow(`SELECT timeout_ns FROM run_document_timeout WHERE run_id=?`, runID).Scan(&ns)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if ns <= 0 {
		return 0, fmt.Errorf("invalid persisted Document timeout")
	}
	return time.Duration(ns), nil
}
