package db

import (
	"database/sql"
	"fmt"
)

// PublicationIntent is an immutable pre-draft binding, not replacement validation
// evidence. Refusal records the ordinary final-content guard's refusal separately
// from step results. Recovery never changes the run's recorded validation verdicts.
type PublicationIntent struct {
	RunID   string
	HeadSHA string
	JSON    string
	Refusal *string
}

func (d *DB) PublicationRecoveryEligible(runID string) (bool, error) {
	var eligible bool
	err := d.sql.QueryRow(`SELECT publication_recovery_eligible FROM runs WHERE id = ?`, runID).Scan(&eligible)
	return eligible, err
}

func (d *DB) PinPublicationIntent(runID, head, payload string) error {
	eligible, err := d.PublicationRecoveryEligible(runID)
	if err != nil || !eligible {
		return err
	}
	_, err = d.execRetryBusy(`INSERT INTO publication_intents (run_id, head_sha, intent_json) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`, runID, head, payload)
	if err != nil {
		return err
	}
	pin, err := d.GetPublicationIntent(runID, head)
	if err != nil {
		return err
	}
	if pin == nil || pin.JSON != payload {
		return fmt.Errorf("publication intent changed for recorded head; refusing to replace immutable pins")
	}
	return nil
}

func (d *DB) GetPublicationIntent(runID, head string) (*PublicationIntent, error) {
	p := &PublicationIntent{RunID: runID, HeadSHA: head}
	err := d.sql.QueryRow(`SELECT intent_json, refusal FROM publication_intents WHERE run_id = ? AND head_sha = ?`, runID, head).Scan(&p.JSON, &p.Refusal)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return p, err
}

func (d *DB) RecordPublicationRefusal(runID, head, refusal string) error {
	_, err := d.execRetryBusy(`UPDATE publication_intents SET refusal = COALESCE(refusal, ?) WHERE run_id = ? AND head_sha = ?`, refusal, runID, head)
	return err
}
