package steps

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPreserveHistory_ResumeKeepsPinsBeforeFixing(t *testing.T) {
	f, base, originalParents := newHistoryFixture(t, false)
	sequence := []pipeline.Step{&RebaseStep{}, &ReviewStep{}, &PushStep{}}
	for _, step := range sequence {
		sr, err := f.sctx.DB.InsertStepResult(f.sctx.Run.ID, step.Name())
		if err != nil {
			t.Fatal(err)
		}
		if step.Name() != types.StepPush {
			// Recovery only accepts a gate the ordinary lifecycle produced.
			if err := f.sctx.DB.StartStep(sr.ID); err != nil {
				t.Fatal(err)
			}
		}
		switch step.Name() {
		case types.StepRebase:
			if err := f.sctx.DB.CompleteStepWithStatus(sr.ID, types.StepStatusCompleted, 0, 1, ""); err != nil {
				t.Fatal(err)
			}
		case types.StepReview:
			findings := `{"findings":[{"id":"review-1","severity":"warning","description":"fixture finding","action":"ask-user"}],"summary":"fixture"}`
			if _, err := f.sctx.DB.InsertReviewStepRound(sr.ID, 1, "initial", &findings, nil, f.headSHA, 1); err != nil {
				t.Fatal(err)
			}
			if err := f.sctx.DB.ParkStepForApproval(f.sctx.Run.ID, sr.ID, types.StepStatusAwaitingApproval, 0, 1, &findings); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := f.sctx.DB.UpdateRunStatus(f.sctx.Run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	run, err := f.sctx.DB.GetRun(f.sctx.Run.ID) // no in-memory policy survives recovery
	if err != nil {
		t.Fatal(err)
	}
	tree := gitCmd(t, f.dir, "rev-parse", base+"^{tree}")
	moved := gitCmd(t, f.dir, "commit-tree", tree, "-p", base, "-m", "base advanced during restart")
	gitCmd(t, f.dir, "push", "origin", moved+":refs/heads/main")
	var executor *pipeline.Executor
	executor = pipeline.NewExecutor(f.sctx.DB, paths.WithRoot(t.TempDir()), f.sctx.Config, f.sctx.Agent, sequence, func(event ipc.Event) {
		if event.StepName != nil && *event.StepName == types.StepReview && event.Status != nil && *event.Status == string(types.StepStatusAwaitingApproval) {
			if err := executor.Respond(types.StepReview, types.ActionFix, nil); err != nil {
				t.Error(err)
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = executor.Resume(ctx, run, f.sctx.Repo, f.dir)
	if err == nil || !strings.Contains(err.Error(), "integration base main moved") {
		t.Fatalf("resumed fixer discarded its pins: %v", err)
	}
	assertRestoredToReviewedHead(t, f.dir, f.headSHA)
	assertHistoryUntouched(t, f, originalParents)
}

func TestPreserveHistory_PublicationRaceUsesServerEnforcedFastForward(t *testing.T) {
	f, _, originalParents := newHistoryFixture(t, false)
	if err := publishRunHead(f.sctx, f.headSHA, "", nil); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	gitCmd(t, other, "clone", "--branch", "feature", f.upstream, ".")
	gitCmd(t, other, "config", "user.name", "test")
	gitCmd(t, other, "config", "user.email", "test@test.com")
	gitCmd(t, other, "commit", "--allow-empty", "-m", "concurrent remote work")
	concurrent := gitCmd(t, other, "rev-parse", "HEAD")
	spyHistoryGit(t, f)
	f.sctx.Env = append(f.sctx.Env,
		"FAKE_CLI_INTERLOPER_DIR="+other,
		"FAKE_CLI_INTERLOPER_REMOTE="+f.upstream,
		"FAKE_CLI_INTERLOPER_REF=HEAD:refs/heads/feature")
	writeCIFix(f.dir)
	_, err := (&CIStep{}).commitRepair(f.sctx, "additive local correction")
	if err == nil || !strings.Contains(err.Error(), "push") {
		t.Fatalf("concurrent remote update was overwritten: %v", err)
	}
	if f.remoteHead(t) != concurrent {
		t.Fatal("remote concurrent history was lost")
	}
	assertHistoryUntouched(t, f, originalParents)
}
