package pipeline

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func executorMonitorFixture(t *testing.T, state string) (*Executor, db.MonitorDisposition) {
	t.Helper()
	d, p, run, repo := setupTest(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	head := strings.Repeat("a", 40)
	must(d.UpdateRunStatus(run.ID, types.RunRunning))
	must(d.UpdateRunPRURL(run.ID, "https://github.com/example/repo/pull/1"))
	must(d.UpdateRunPRState(run.ID, "open"))
	must(d.UpdateRunPublication(run.ID, db.PushBinding{HeadSHA: head}))
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
	}
	if state == "polling" {
		must(d.SetRunCIReady(run.ID, true))
	} else {
		must(d.SetRunAwaitingAgent(run.ID))
	}
	e := NewExecutor(d, p, nil, nil, nil, nil)
	if state == "parked-review" {
		e.waiting = true
		e.waitingStep = types.StepReview
	}
	return e, db.MonitorDisposition{RunID: run.ID, RepoID: repo.ID, PRURL: "https://github.com/example/repo/pull/1", PublishedHead: head, ObservedHead: head, State: state}
}

func TestMonitorStopParkedPreservesGateAndRefusesResponses(t *testing.T) {
	e, p := executorMonitorFixture(t, "parked-review")
	before, _ := e.db.GetRun(p.RunID)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := e.StopMonitor(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := e.StopMonitor(ctx, p); err != nil {
		t.Fatal(err)
	}
	for _, action := range []types.ApprovalAction{types.ActionFix, types.ActionApprove, types.ActionSkip, types.ActionAbort} {
		if err := e.Respond(types.StepReview, action, nil); err == nil {
			t.Fatalf("accepted %s", action)
		}
	}
	after, _ := e.db.GetRun(p.RunID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("changed gate/custody/evidence")
	}
	if !e.waiting {
		t.Fatal("released unfinished gate")
	}
}

func TestMonitorStopPollingAcknowledgesOnlyAtBoundary(t *testing.T) {
	e, p := executorMonitorFixture(t, "polling")
	before, _ := e.db.GetRun(p.RunID)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- e.StopMonitor(ctx, p) }()
	select {
	case err := <-result:
		t.Fatalf("acknowledged before boundary: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	finished := make(chan error, 1)
	go func() { finished <- e.monitorCheckpoint(ctx) }()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("no acknowledgement")
	}
	select {
	case err := <-finished:
		t.Fatalf("frozen monitor continued: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	after, _ := e.db.GetRun(p.RunID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("freeze changed evidence")
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("checkpoint outlived owner")
	}
}

func TestMonitorStopCancelledRequestCannotFreezeLater(t *testing.T) {
	e, p := executorMonitorFixture(t, "polling")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := e.StopMonitor(ctx, p); err == nil {
		t.Fatal("accepted cancelled request")
	}
	if err := e.monitorCheckpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	frozen, _ := e.db.MonitorFrozen(p.RunID)
	if frozen {
		t.Fatal("late freeze")
	}
}

func TestMonitorStopRefusesResponseRaceAndMovedHead(t *testing.T) {
	t.Run("response-won", func(t *testing.T) {
		e, p := executorMonitorFixture(t, "parked-review")
		if err := e.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
			t.Fatal(err)
		}
		if err := e.StopMonitor(context.Background(), p); err == nil {
			t.Fatal("froze after response accepted")
		}
	})
	t.Run("head-moved", func(t *testing.T) {
		e, p := executorMonitorFixture(t, "polling")
		if err := e.db.UpdateRunHeadSHA(p.RunID, strings.Repeat("b", 40)); err != nil {
			t.Fatal(err)
		}
		if err := e.StopMonitor(context.Background(), p); err == nil {
			t.Fatal("froze moved head")
		}
	})
}
