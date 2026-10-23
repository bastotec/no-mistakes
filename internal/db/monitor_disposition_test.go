package db

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func monitorFixture(t *testing.T, state string) (*DB, MonitorDisposition) {
	t.Helper()
	d := openTestDB(t)
	repo, err := d.InsertRepo(t.TempDir(), "https://github.com/example/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("a", 40)
	run, err := d.InsertRun(repo.ID, "frozen", head, strings.Repeat("b", 40))
	if err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(d.UpdateRunStatus(run.ID, types.RunRunning))
	must(d.UpdateRunPRURL(run.ID, "https://github.com/example/repo/pull/1"))
	must(d.UpdateRunPRState(run.ID, "open"))
	must(d.UpdateRunPublication(run.ID, PushBinding{HeadSHA: head, TargetKind: "upstream", TargetFingerprint: "fingerprint", Ref: "refs/heads/frozen"}))
	p := MonitorDisposition{RunID: run.ID, RepoID: repo.ID, PRURL: "https://github.com/example/repo/pull/1", PublishedHead: head, ObservedHead: head, State: state}
	for _, name := range types.AllSteps() {
		sr, err := d.InsertStepResult(run.ID, name)
		must(err)
		status := types.StepStatusCompleted
		if state == "polling" && name == types.StepCI {
			status = types.StepStatusRunning
		}
		if state == "parked-review" {
			if name == types.StepReview {
				status = types.StepStatusAwaitingApproval
			}
			if name.Order() > types.StepReview.Order() {
				status = types.StepStatusPending
			}
		}
		must(d.UpdateStepStatus(sr.ID, status))
		if name == types.StepReview {
			must(d.SetStepFindings(sr.ID, `{"summary":"untouched","findings":[]}`))
			_, err = d.InsertStepRound(sr.ID, 1, "initial", nil, nil, 1)
			must(err)
		}
	}
	if state == "polling" {
		must(d.SetRunCIReady(run.ID, true))
	} else {
		p.ObservedHead = strings.Repeat("c", 40)
		must(d.UpdateRunHeadSHA(run.ID, p.ObservedHead))
		must(d.SetRunAwaitingAgent(run.ID))
	}
	return d, p
}

func TestMonitorDispositionPreservesEvidenceAndRecovery(t *testing.T) {
	for _, state := range []string{"polling", "parked-review"} {
		t.Run(state, func(t *testing.T) {
			d, p := monitorFixture(t, state)
			other, err := d.InsertRun(p.RepoID, "unrelated", strings.Repeat("e", 40), strings.Repeat("f", 40))
			if err != nil {
				t.Fatal(err)
			}
			otherBefore, _ := d.GetRun(other.ID)
			before, _ := d.GetRun(p.RunID)
			steps, _ := d.GetStepsByRun(p.RunID)
			var reviewRounds []*StepRound
			var reviewID string
			for _, step := range steps {
				if step.StepName == types.StepReview {
					reviewID = step.ID
					reviewRounds, _ = d.GetRoundsByStep(step.ID)
				}
			}
			if err := d.FreezeMonitor(p); err != nil {
				t.Fatal(err)
			}
			if err := d.FreezeMonitor(p); err != nil {
				t.Fatal("not idempotent", err)
			}
			after, _ := d.GetRun(p.RunID)
			afterSteps, _ := d.GetStepsByRun(p.RunID)
			if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(steps, afterSteps) {
				t.Fatal("freeze altered validation/custody/publication evidence")
			}
			otherAfter, _ := d.GetRun(other.ID)
			roundsAfter, _ := d.GetRoundsByStep(reviewID)
			if !reflect.DeepEqual(otherBefore, otherAfter) || !reflect.DeepEqual(reviewRounds, roundsAfter) {
				t.Fatal("freeze changed unrelated lane or round history")
			}
			if _, err := d.RecoverStaleRuns("restart"); err != nil {
				t.Fatal(err)
			}
			recovered, _ := d.GetRun(p.RunID)
			if !reflect.DeepEqual(before, recovered) {
				t.Fatal("restart changed frozen run")
			}
		})
	}
}

func TestMonitorDispositionRefusesChangedOrUncertainEvidence(t *testing.T) {
	for _, change := range []string{"repo", "run", "url", "published", "observed", "state", "repair", "push", "step", "unknown-repair"} {
		t.Run(change, func(t *testing.T) {
			d, p := monitorFixture(t, "parked-review")
			switch change {
			case "repo":
				p.RepoID = "other"
			case "run":
				p.RunID = "missing"
			case "url":
				p.PRURL += "0"
			case "published":
				p.PublishedHead = strings.Repeat("d", 40)
			case "observed":
				p.ObservedHead = strings.Repeat("d", 40)
			case "state":
				p.State = "polling"
			case "push":
				_ = d.SetRunPushActive(p.RunID, true)
			case "step":
				_, _ = d.sql.Exec(`UPDATE step_results SET status='fixing' WHERE run_id=? AND step_name='review'`, p.RunID)
			case "repair", "unknown-repair":
				steps, _ := d.GetStepsByRun(p.RunID)
				for _, s := range steps {
					if s.StepName == types.StepReview {
						rounds, _ := d.GetRoundsByStep(s.ID)
						inv, err := d.RegisterRepairInvocation(s.ID, rounds[0].ID, 1, t.TempDir(), "deck", "fix", 0)
						if err != nil {
							t.Fatal(err)
						}
						if change == "unknown-repair" {
							_, _ = d.sql.Exec(`UPDATE repair_invocations SET state='unknown' WHERE id=?`, inv.ID)
						}
					}
				}
			}
			if err := d.FreezeMonitor(p); err == nil {
				t.Fatal("accepted changed or uncertain identity/state")
			}
			frozen, _ := d.MonitorFrozen(p.RunID)
			if frozen {
				t.Fatal("refusal wrote a disposition")
			}
		})
	}
}
