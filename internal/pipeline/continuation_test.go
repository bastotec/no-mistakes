package pipeline

import (
	"context"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"strings"
	"testing"
)

func TestContinuationExecutorRefusesPerRunSkip(t *testing.T) {
	d, p, run, repo := setupTest(t)
	policy := `{"version":1}` // Presence is authority; even invalid policy cannot opt out.
	run.ContinuationJSON = &policy
	step := newApprovalStep(types.StepReview, `{"summary":"review"}`)
	e := NewExecutor(d, p, nil, nil, []Step{step}, nil)
	e.SetSkippedSteps([]types.StepName{types.StepReview})
	err := e.Execute(context.Background(), run, repo, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "per-run skips are forbidden") {
		t.Fatalf("err=%v", err)
	}
	records, _ := d.GetStepsByRun(run.ID)
	if len(records) != 1 || records[0].Status == types.StepStatusSkipped {
		t.Fatalf("required review skipped: %+v", records)
	}
}

func TestContinuationExecutorCIApprovalCannotWaiveUnresolvedEvidence(t *testing.T) {
	for _, continuation := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "continuation"}[continuation], func(t *testing.T) {
			d, p, run, repo := setupTest(t)
			if continuation {
				policy := `{"version":1}`
				run.ContinuationJSON = &policy
			}
			sr, err := d.InsertStepResult(run.ID, types.StepCI)
			if err != nil {
				t.Fatal(err)
			}
			step := newOverrideVerifyingApprovalStep(types.StepCI, `{"summary":"red check"}`, func(*StepContext) (string, error) { return "required check is still red", nil })
			e := NewExecutor(d, p, nil, nil, []Step{step}, nil)
			err = e.applyApprovalOverride(step, &StepContext{Ctx: context.Background(), Run: run, Repo: repo, DB: d}, sr.ID)
			if continuation && (err == nil || !strings.Contains(err.Error(), "cannot waive")) {
				t.Fatalf("err=%v", err)
			}
			if !continuation && err != nil {
				t.Fatalf("ordinary approval contract changed: %v", err)
			}
		})
	}
}
