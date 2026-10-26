package db

import (
	"database/sql"
	"fmt"
	"regexp"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// MonitorDisposition binds a one-way monitor freeze to exact observed evidence.
// State is either green CI polling or an unfinished parked Review. It does not
// assert how the run reached Review: old runs have no durable restart-origin proof.
type MonitorDisposition struct {
	RunID         string `json:"run_id"`
	RepoID        string `json:"repo_id"`
	PRURL         string `json:"pr_url"`
	PublishedHead string `json:"published_head"`
	ObservedHead  string `json:"observed_head"`
	State         string `json:"state"`
}

var dispositionSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

func (d *DB) MonitorFrozen(runID string) (bool, error) {
	var n int
	err := d.sql.QueryRow(`SELECT count(*) FROM run_monitor_dispositions WHERE run_id = ?`, runID).Scan(&n)
	return n == 1, err
}

// FreezeMonitor must be called by the executor at a quiescent monitor boundary,
// or with its parked Review response lock held. The transaction rechecks every
// identity and state pin; the only write is the disposition record.
func (d *DB) FreezeMonitor(p MonitorDisposition) error { return d.monitorDisposition(p, true) }

func (d *DB) ValidateMonitorDisposition(p MonitorDisposition) error {
	return d.monitorDisposition(p, false)
}

func (d *DB) monitorDisposition(p MonitorDisposition, persist bool) error {
	if p.RunID == "" || p.RepoID == "" || p.PRURL == "" || !dispositionSHA.MatchString(p.PublishedHead) || !dispositionSHA.MatchString(p.ObservedHead) || (p.State != "polling" && p.State != "parked-review") {
		return fmt.Errorf("complete exact monitor identity and state required")
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var repo, head, status string
	var url, pushed, prState sql.NullString
	var ready, awaiting sql.NullInt64
	var pushActive bool
	err = tx.QueryRow(`SELECT repo_id, head_sha, status, pr_url, last_pushed_sha, ci_ready_at, push_active, pr_state, awaiting_agent_since FROM runs WHERE id = ?`, p.RunID).Scan(&repo, &head, &status, &url, &pushed, &ready, &pushActive, &prState, &awaiting)
	if err != nil {
		return err
	}
	if repo != p.RepoID || head != p.ObservedHead || status != "running" || url.String != p.PRURL || pushed.String != p.PublishedHead || pushActive || prState.String != "open" {
		return fmt.Errorf("monitor identity or run state changed")
	}
	var old MonitorDisposition
	err = tx.QueryRow(`SELECT repo_id, pr_url, published_head, observed_head, observed_state FROM run_monitor_dispositions WHERE run_id = ?`, p.RunID).Scan(&old.RepoID, &old.PRURL, &old.PublishedHead, &old.ObservedHead, &old.State)
	alreadyFrozen := err == nil
	if alreadyFrozen {
		old.RunID = p.RunID
		if old != p {
			return fmt.Errorf("monitor disposition identity changed")
		}
	}
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	var active int
	if err = tx.QueryRow(`SELECT count(*) FROM repair_invocations ri JOIN step_results sr ON sr.id=ri.step_result_id WHERE sr.run_id=? AND ri.state NOT IN ('terminal','consumed')`, p.RunID).Scan(&active); err != nil {
		return err
	}
	var dispatch int
	if err := tx.QueryRow(`SELECT count(*) FROM repair_budget_decisions b JOIN step_results s ON s.id=b.step_result_id
		WHERE s.run_id=? AND b.dispatch_state IN ('claimed','started','repair_started_unresolved')
		AND NOT (b.dispatch_state IN ('claimed','started') AND s.step_name='ci' AND s.status='running'
			AND EXISTS (SELECT 1 FROM repair_invocations ri WHERE ri.step_result_id=b.step_result_id AND ri.round_id=b.round_id
				AND ri.state='terminal' AND ri.lifecycle_phase='post_repair_monitor'
				AND ri.ordinal=(SELECT MAX(latest.ordinal) FROM repair_invocations latest WHERE latest.step_result_id=b.step_result_id AND latest.round_id=b.round_id)))`, p.RunID).Scan(&dispatch); err != nil {
		return err
	}
	if active != 0 || dispatch != 0 {
		return fmt.Errorf("active or uncertain repair prevents monitor freeze")
	}
	rows, err := tx.Query(`SELECT step_name,status FROM step_results WHERE run_id=?`, p.RunID)
	if err != nil {
		return err
	}
	states := map[string]string{}
	for rows.Next() {
		var name, state string
		if err = rows.Scan(&name, &state); err != nil {
			rows.Close()
			return err
		}
		if _, exists := states[name]; exists {
			rows.Close()
			return fmt.Errorf("ambiguous duplicate step evidence")
		}
		states[name] = state
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, name := range types.AllSteps() {
		if _, ok := states[string(name)]; !ok {
			return fmt.Errorf("incomplete step evidence")
		}
	}
	if p.State == "polling" {
		if states["ci"] != "running" || !ready.Valid || awaiting.Valid || head != p.PublishedHead {
			return fmt.Errorf("run is not exact-head post-green CI polling")
		}
		for name, state := range states {
			if name != "ci" && state != "completed" && state != "skipped" {
				return fmt.Errorf("unfinished validation prevents polling freeze")
			}
		}
	} else {
		if states["review"] != "awaiting_approval" || states["ci"] != "pending" || !awaiting.Valid {
			return fmt.Errorf("run is not parked at unfinished Review")
		}
		for name, state := range states {
			if name != "review" && (state == "running" || state == "fixing" || state == "fix_review" || state == "awaiting_approval") {
				return fmt.Errorf("another active step prevents freeze")
			}
		}
	}
	if !persist || alreadyFrozen {
		return nil
	}
	_, err = tx.Exec(`INSERT INTO run_monitor_dispositions(run_id,repo_id,pr_url,published_head,observed_head,observed_state,created_at) VALUES(?,?,?,?,?,?,?)`, p.RunID, p.RepoID, p.PRURL, p.PublishedHead, p.ObservedHead, p.State, now())
	if err != nil {
		return err
	}
	return tx.Commit()
}
