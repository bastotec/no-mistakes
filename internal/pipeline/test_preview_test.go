package pipeline

import (
	"context"
	"fmt"
	"reflect"
	"sync/atomic"
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

func TestPublishTestPreview_PendingPublishesBeforeFirstAttempt(t *testing.T) {
	database, p, run, repo := setupTest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reviewEntered := make(chan struct{})
	finishReview := make(chan struct{})
	review := &adaptiveCallStep{name: types.StepReview, fn: func(s *StepContext) (*StepOutcome, error) {
		close(reviewEntered)
		select {
		case <-finishReview:
			return &StepOutcome{ReviewApprovedHeadSHA: s.Run.HeadSHA}, nil
		case <-s.Ctx.Done():
			return nil, s.Ctx.Err()
		}
	}}
	var published atomic.Bool
	var attempts atomic.Int32
	test := &adaptiveCallStep{name: types.StepTest, fn: func(s *StepContext) (*StepOutcome, error) {
		attempts.Add(1)
		if !published.Load() {
			return nil, fmt.Errorf("Test ran before explicit preview publication")
		}
		return &StepOutcome{NeedsApproval: true, Findings: `{"findings":[],"summary":"Needs live evidence"}`}, nil
	}}
	e := NewExecutor(database, p, nil, nil, []Step{review, test}, nil)
	finished := make(chan struct{})
	go func() { defer close(finished); _ = e.Execute(ctx, run, repo, t.TempDir()) }()
	t.Cleanup(func() { cancel(); <-finished })
	select {
	case <-reviewEntered:
	case <-ctx.Done():
		t.Fatal("review did not start")
	}
	result := make(chan error, 1)
	go func() {
		_, err := e.PublishTestPreview(ctx, ipc.PublishTestPreviewParams{RepoID: repo.ID, RunID: run.ID, HeadSHA: run.HeadSHA}, func(s *StepContext) (*ipc.PublishTestPreviewResult, error) {
			rows, err := database.GetStepsByRun(run.ID)
			if err != nil {
				return nil, err
			}
			for _, row := range rows {
				if row.StepName == types.StepTest && row.Status != types.StepStatusPending {
					return nil, fmt.Errorf("Test not pending: %s", row.Status)
				}
			}
			if attempts.Load() != 0 {
				return nil, fmt.Errorf("Test already attempted")
			}
			if err := database.BindTestPreview(run.ID, "https://github.com/test/repo/pull/99"); err != nil {
				return nil, err
			}
			published.Store(true)
			return &ipc.PublishTestPreviewResult{TestStatus: "pending"}, nil
		})
		result <- err
	}()
	for {
		e.mu.Lock()
		queued := e.pendingTestPreview != nil
		e.mu.Unlock()
		if queued {
			break
		}
		select {
		case err := <-result:
			t.Fatalf("request did not queue: %v", err)
		case <-ctx.Done():
			t.Fatal("request did not queue")
		case <-time.After(time.Millisecond):
		}
	}
	if published.Load() {
		t.Fatal("publication raced earlier step")
	}
	close(finishReview)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	waitForStepStatus(t, database, run.ID, types.StepTest, types.StepStatusAwaitingApproval)
	if attempts.Load() != 1 {
		t.Fatalf("Test attempts=%d", attempts.Load())
	}
	if needed, err := database.TestPreviewNeedsValidation(run.ID, run.HeadSHA); err != nil || !needed {
		t.Fatalf("gated Test validated preview: %v, %v", needed, err)
	}
	if err := e.Respond(types.StepTest, types.ActionAbort, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	case <-ctx.Done():
		t.Fatal("executor did not stop")
	}
}

func TestPublishTestPreview_ApprovalReexecutesUntilValidated(t *testing.T) {
	for _, recovered := range []bool{false, true} {
		t.Run(fmt.Sprintf("recovered=%v", recovered), func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var attempts atomic.Int32
			var ready atomic.Bool
			test := &adaptiveCallStep{name: types.StepTest, fn: func(s *StepContext) (*StepOutcome, error) {
				if s.Fixing {
					return nil, fmt.Errorf("approval launched a repair instead of validation")
				}
				if attempts.Add(1) < 3 {
					return &StepOutcome{NeedsApproval: true, Findings: `{"findings":[],"summary":"Preview unavailable"}`}, nil
				}
				return &StepOutcome{}, nil
			}}
			pr := &adaptiveCallStep{name: types.StepPR, fn: func(s *StepContext) (*StepOutcome, error) {
				needed, err := database.TestPreviewNeedsValidation(run.ID, run.HeadSHA)
				if err != nil {
					return nil, err
				}
				if needed || attempts.Load() != 3 {
					return nil, fmt.Errorf("premature ready transition")
				}
				ready.Store(true)
				return &StepOutcome{}, nil
			}}
			e := NewExecutor(database, p, nil, nil, []Step{test, pr}, nil)
			done := make(chan error, 1)
			finished := make(chan struct{})
			work := t.TempDir()
			if recovered {
				_ = database.UpdateRunStatus(run.ID, types.RunRunning)
				sr, err := database.InsertStepResult(run.ID, types.StepTest)
				if err != nil {
					t.Fatal(err)
				}
				if err := database.StartStep(sr.ID); err != nil {
					t.Fatal(err)
				}
				findings := `{"findings":[],"summary":"Preview unavailable"}`
				if _, err := database.InsertStepRound(sr.ID, 1, "initial", &findings, nil, 1); err != nil {
					t.Fatal(err)
				}
				if err := database.ParkStepForApproval(run.ID, sr.ID, types.StepStatusAwaitingApproval, 0, 1, &findings); err != nil {
					t.Fatal(err)
				}
				if _, err := database.InsertStepResult(run.ID, types.StepPR); err != nil {
					t.Fatal(err)
				}
				if err := database.BindTestPreview(run.ID, "https://github.com/test/repo/pull/99"); err != nil {
					t.Fatal(err)
				}
				run, _ = database.GetRun(run.ID)
				attempts.Store(1)
				go func() { defer close(finished); done <- e.Resume(ctx, run, repo, work) }()
			} else {
				go func() { defer close(finished); done <- e.Execute(ctx, run, repo, work) }()
			}
			t.Cleanup(func() { cancel(); <-finished })
			for {
				e.mu.Lock()
				waiting := e.waiting
				e.mu.Unlock()
				if waiting {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal("Test never parked")
				case <-time.After(time.Millisecond):
				}
			}
			if !recovered {
				waitForStepStatus(t, database, run.ID, types.StepTest, types.StepStatusAwaitingApproval)
				_, err := e.PublishTestPreview(ctx, ipc.PublishTestPreviewParams{RepoID: repo.ID, RunID: run.ID, HeadSHA: run.HeadSHA}, func(s *StepContext) (*ipc.PublishTestPreviewResult, error) {
					return &ipc.PublishTestPreviewResult{}, database.BindTestPreview(run.ID, "https://github.com/test/repo/pull/99")
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := e.Respond(types.StepTest, types.ActionApprove, nil); err != nil {
				t.Fatal(err)
			}
			for {
				e.mu.Lock()
				waiting := e.waiting
				e.mu.Unlock()
				if attempts.Load() == 2 && waiting {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal("approval did not rerun Test and preserve its new gate")
				case <-time.After(time.Millisecond):
				}
			}
			if ready.Load() {
				t.Fatal("approval waived inconclusive Test")
			}
			if err := e.Respond(types.StepTest, types.ActionApprove, nil); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("validation never completed")
			}
			if !ready.Load() {
				t.Fatal("successful Test did not permit normal PR transition")
			}
		})
	}
}
