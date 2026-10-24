package pipeline

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRefreshPublication_DispatchesOnlyAtMonitorBoundary(t *testing.T) {
	database, paths, run, repo := setupTest(t)
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	url := "https://github.com/test/repo/pull/42"
	if err := database.UpdateRunPRURL(run.ID, url); err != nil {
		t.Fatal(err)
	}
	ci, err := database.InsertStepResult(run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatus(ci.ID, types.StepStatusRunning); err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(database, paths, nil, nil, nil, nil)
	p := ipc.RefreshPublicationParams{RepoID: repo.ID, RunID: run.ID, HeadSHA: run.HeadSHA, PRURL: url}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := e.RefreshPublication(ctx, p); done <- err }()
	var request *PublicationRefreshRequest
	select {
	case request = <-e.publicationRefreshCh:
	case <-ctx.Done():
		t.Fatal("request never reached monitor")
	}
	select {
	case <-done:
		t.Fatal("request completed before monitor served it")
	default:
	}
	before, _ := database.GetStepResult(ci.ID)
	calls := 0
	request.Serve(&StepContext{Ctx: ctx, Run: run, Repo: repo, DB: database}, func(_ *StepContext, got ipc.RefreshPublicationParams) (*ipc.RefreshPublicationResult, error) {
		calls++
		if got != p {
			t.Fatal("binding changed")
		}
		return &ipc.RefreshPublicationResult{RunID: p.RunID}, nil
	})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	after, _ := database.GetStepResult(ci.ID)
	if calls != 1 || !reflect.DeepEqual(before, after) {
		t.Fatal("dispatch changed CI state")
	}
	select {
	case <-e.approvalCh:
		t.Fatal("dispatch granted approval")
	default:
	}
}

func TestRefreshPublication_RefusesNonMonitorStates(t *testing.T) {
	for _, status := range []types.StepStatus{types.StepStatusPending, types.StepStatusFixing, types.StepStatusAwaitingApproval, types.StepStatusCompleted, types.StepStatusFailed} {
		t.Run(string(status), func(t *testing.T) {
			database, paths, run, repo := setupTest(t)
			database.UpdateRunStatus(run.ID, types.RunRunning)
			url := "https://github.com/test/repo/pull/42"
			database.UpdateRunPRURL(run.ID, url)
			ci, _ := database.InsertStepResult(run.ID, types.StepCI)
			database.UpdateStepStatus(ci.ID, status)
			e := NewExecutor(database, paths, nil, nil, nil, nil)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if _, err := e.RefreshPublication(ctx, ipc.RefreshPublicationParams{RepoID: repo.ID, RunID: run.ID, HeadSHA: run.HeadSHA, PRURL: url}); err == nil {
				t.Fatal("accepted non-monitor state")
			}
			select {
			case <-e.publicationRefreshCh:
				t.Fatal("queued invalid request")
			default:
			}
		})
	}
}

func TestRefreshPublication_CancelledDispatchNeverPublishes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := &PublicationRefreshRequest{ctx: ctx, done: make(chan publicationRefreshResponse, 1)}
	request.Serve(&StepContext{Ctx: context.Background()}, func(*StepContext, ipc.RefreshPublicationParams) (*ipc.RefreshPublicationResult, error) {
		t.Fatal("cancelled request published")
		return nil, nil
	})
	if response := <-request.done; response.err == nil {
		t.Fatal("cancelled request succeeded")
	}
}
