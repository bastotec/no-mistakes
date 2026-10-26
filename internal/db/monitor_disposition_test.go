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

func TestMonitorDispositionPostRepairPolling(t *testing.T) {
	for _, stage := range []string{"prepared", "registered", "started", "terminal", "post-monitor", "unresolved", "newer-terminal"} {
		t.Run(stage, func(t *testing.T) {
			d, p := monitorFixture(t, "polling")
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			steps, err := d.GetStepsByRun(p.RunID)
			must(err)
			var stepID string
			for _, step := range steps {
				if step.StepName == types.StepCI {
					stepID = step.ID
				}
			}
			must(d.UpdateStepStatus(stepID, types.StepStatusPending))
			must(d.StartStepWithAutoFixLimit(stepID, 1))
			round, err := d.InsertStepRound(stepID, 1, "initial", nil, nil, 1)
			must(err)
			decision, err := d.ReserveStepRepairWithSelection(stepID, round.ID, 1, nil)
			must(err)
			if !decision.Granted {
				t.Fatal("repair reservation refused")
			}
			must(d.ClaimStepRepair(stepID, round.ID))
			if stage != "prepared" {
				inv, err := d.RegisterRepairInvocation(stepID, round.ID, 0, t.TempDir(), "agent", "ci-fix", 0)
				must(err)
				if stage != "registered" {
					must(d.BindRepairInvocationProcess(inv.ID, "repair active", 4242))
					if stage != "started" {
						must(d.FinishRepairInvocation(inv.ID, []byte(`{"text":"published repair"}`), true, nil, nil))
						if stage != "terminal" {
							must(d.MarkRepairPostMonitor(stepID, round.ID))
							if stage == "unresolved" {
								_, err := d.MarkRepairInvocationUnresolved(stepID, round.ID)
								must(err)
							}
							if stage == "newer-terminal" {
								next, err := d.RegisterRepairInvocation(stepID, round.ID, 1, t.TempDir(), "agent", "ci-fix", 0)
								must(err)
								must(d.FinishRepairInvocation(next.ID, nil, false, nil, nil))
							}
						}
					}
				}
			}
			must(d.UpdateStepStatus(stepID, types.StepStatusRunning))
			before, err := d.GetRun(p.RunID)
			must(err)
			stepsBefore, err := d.GetStepsByRun(p.RunID)
			must(err)
			var dispatchBefore string
			must(d.sql.QueryRow(`SELECT dispatch_state FROM repair_budget_decisions WHERE round_id=?`, round.ID).Scan(&dispatchBefore))
			invBefore, err := d.RepairInvocationForRound(stepID, round.ID)
			must(err)
			repairBefore, err := d.GetRepairInvocation(invBefore)
			must(err)
			for _, apply := range []func(MonitorDisposition) error{d.ValidateMonitorDisposition, d.FreezeMonitor} {
				err := apply(p)
				if stage == "post-monitor" {
					must(err)
				} else if err == nil || !strings.Contains(err.Error(), "active or uncertain repair") {
					t.Fatalf("%s freeze = %v, want repair refusal", stage, err)
				}
			}
			frozen, err := d.MonitorFrozen(p.RunID)
			must(err)
			if frozen != (stage == "post-monitor") {
				t.Fatalf("frozen=%v for %s", frozen, stage)
			}
			after, err := d.GetRun(p.RunID)
			must(err)
			stepsAfter, err := d.GetStepsByRun(p.RunID)
			must(err)
			repairAfter, err := d.GetRepairInvocation(invBefore)
			must(err)
			var dispatchAfter string
			must(d.sql.QueryRow(`SELECT dispatch_state FROM repair_budget_decisions WHERE round_id=?`, round.ID).Scan(&dispatchAfter))
			if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(stepsBefore, stepsAfter) || !reflect.DeepEqual(repairBefore, repairAfter) || dispatchBefore != dispatchAfter {
				t.Fatal("monitor disposition changed validation, custody, or repair evidence")
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
