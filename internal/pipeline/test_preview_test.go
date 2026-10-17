package pipeline

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPublishTestPreview_PinsEvidenceAndLeavesGateUntouched(t *testing.T) {
	database, p, run, repo := setupTest(t)
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	sr, err := database.InsertStepResult(run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"live","severity":"warning","action":"ask-user","description":"Need live preview"}]}`
	if err := database.ParkStepForApproval(run.ID, sr.ID, types.StepStatusAwaitingApproval, 1, 1, &findings); err != nil {
		t.Fatal(err)
	}
	e := NewExecutor(database, p, nil, nil, nil, nil)
	e.waiting = true
	e.waitingStep = types.StepTest
	e.waitingStepResultID = sr.ID
	e.waitingContext = &StepContext{Ctx: context.Background(), Run: run, Repo: repo, DB: database, StepResultID: sr.ID}
	params := ipc.PublishTestPreviewParams{RepoID: repo.ID, RunID: run.ID, HeadSHA: run.HeadSHA}
	before, _ := database.GetStepResult(sr.ID)
	calls := 0
	publish := func(s *StepContext) (*ipc.PublishTestPreviewResult, error) {
		calls++
		return &ipc.PublishTestPreviewResult{RunID: s.Run.ID, HeadSHA: s.Run.HeadSHA}, nil
	}
	for _, bad := range []ipc.PublishTestPreviewParams{{RepoID: "other", RunID: run.ID, HeadSHA: run.HeadSHA}, {RepoID: repo.ID, RunID: "other", HeadSHA: run.HeadSHA}, {RepoID: repo.ID, RunID: run.ID, HeadSHA: "stale"}} {
		if _, err := e.PublishTestPreview(context.Background(), bad, publish); err == nil {
			t.Fatal("accepted mismatched operator evidence")
		}
	}
	if calls != 0 {
		t.Fatal("publisher called on rejection")
	}
	for i := 0; i < 2; i++ {
		if _, err := e.PublishTestPreview(context.Background(), params, publish); err != nil {
			t.Fatal(err)
		}
	}
	after, _ := database.GetStepResult(sr.ID)
	if !reflect.DeepEqual(before, after) || !e.waiting {
		t.Fatalf("Test gate changed: before=%+v after=%+v", before, after)
	}
	select {
	case <-e.approvalCh:
		t.Fatal("publication sent approval")
	default:
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.PublishTestPreview(ctx, params, publish); err == nil {
		t.Fatal("accepted cancelled request")
	}
	e.waiting = false
	if _, err := e.PublishTestPreview(context.Background(), params, publish); err == nil {
		t.Fatal("accepted nonparked run")
	}
}

func TestPublishTestPreview_SerializesGateResponse(t *testing.T) {
	database, p, run, repo := setupTest(t)
	_ = database.UpdateRunStatus(run.ID, types.RunRunning)
	sr, _ := database.InsertStepResult(run.ID, types.StepTest)
	_ = database.ParkStepForApproval(run.ID, sr.ID, types.StepStatusAwaitingApproval, 1, 0, nil)
	e := NewExecutor(database, p, nil, nil, nil, nil)
	e.waiting = true
	e.waitingStep = types.StepTest
	e.waitingStepResultID = sr.ID
	e.waitingContext = &StepContext{Ctx: context.Background(), Run: run, Repo: repo, DB: database}
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := e.PublishTestPreview(context.Background(), ipc.PublishTestPreviewParams{RepoID: repo.ID, RunID: run.ID, HeadSHA: run.HeadSHA}, func(*StepContext) (*ipc.PublishTestPreviewResult, error) {
			close(entered)
			<-release
			return &ipc.PublishTestPreviewResult{}, nil
		})
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("publication did not start")
	}
	responded := make(chan error, 1)
	go func() { responded <- e.Respond(types.StepTest, types.ActionApprove, nil) }()
	select {
	case err := <-responded:
		close(release)
		t.Fatalf("response raced publication: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-responded; err != nil {
		t.Fatal(err)
	}
}
