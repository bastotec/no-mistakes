package db

import "database/sql"

// EnableRunConfinement is launch-only authority, exactly like Test isolation:
// never inferred from repository or global configuration, never copied to a
// rerun. Recovery re-reads this immutable per-run pin before any dispatch.
//
// digest is runconfine.PolicyDigest(policy, runID, headSHA) at launch time; it
// re-validates on every dispatch so a policy mismatch is a hard refusal, never
// a silent reinterpretation of the pinned boundary. headSHA records the head
// the pin was made against; the run's head legitimately advances afterwards as
// repairs commit, so digest equality - not head equality - is the contract.
func (d *DB) EnableRunConfinement(runID, headSHA, digest string) error {
	_, err := d.sql.Exec(`INSERT INTO run_confinement(run_id, head_sha, policy_digest) VALUES (?,?,?)`, runID, headSHA, digest)
	return err
}

// RunConfinementDigest returns the pinned policy digest for a run. An absent
// row is an ordinary unconfined run, reported as ok=false with no error.
func (d *DB) RunConfinementDigest(runID string) (digest string, ok bool, err error) {
	err = d.sql.QueryRow(`SELECT policy_digest FROM run_confinement WHERE run_id=?`, runID).Scan(&digest)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return digest, true, nil
}

// RunConfinementEnabled answers whether a run carries the per-run confinement
// pin. It is the cheap boolean the executor's dispatch boundary and daemon
// cleanup read; callers that need the digest itself use RunConfinementDigest.
func (d *DB) RunConfinementEnabled(runID string) (bool, error) {
	var n int
	err := d.sql.QueryRow(`SELECT COUNT(*) FROM run_confinement WHERE run_id=?`, runID).Scan(&n)
	return n == 1, err
}

func (d *DB) GitCallbacksDisabled(runID string) (bool, error) {
	var disabled bool
	err := d.sql.QueryRow(`SELECT EXISTS(SELECT 1 FROM run_test_isolation WHERE run_id=?) OR EXISTS(SELECT 1 FROM run_confinement WHERE run_id=?)`, runID, runID).Scan(&disabled)
	return disabled, err
}
