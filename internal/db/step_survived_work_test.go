package db

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestReplaceRunSurvivedWork_MovesTheHandoffInOneStep(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/work", "https://example.com/r.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	review, _ := d.InsertStepResult(run.ID, types.StepReview)
	ci, _ := d.InsertStepResult(run.ID, types.StepCI)
	old, _ := types.MarshalFindingsJSON(types.Findings{Summary: "kept", SurvivedWork: &types.SurvivedWork{Ref: "refs/no-mistakes/survived/r/old"}})
	if err := d.SetStepFindings(review.ID, old); err != nil {
		t.Fatal(err)
	}
	if err := d.ReplaceRunSurvivedWork(run.ID, ci.ID, &types.SurvivedWork{Ref: "refs/no-mistakes/survived/r/new"}); err != nil {
		t.Fatal(err)
	}
	got := func(id string) types.Findings {
		sr, err := d.GetStepResult(id)
		if err != nil || sr.FindingsJSON == nil {
			t.Fatalf("read %s: %+v %v", id, sr, err)
		}
		f, err := types.ParseFindingsJSON(*sr.FindingsJSON)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	if f := got(review.ID); f.SurvivedWork != nil || f.Summary != "kept" {
		t.Fatalf("old handoff not removed, or other findings lost: %+v", f)
	}
	if f := got(ci.ID); f.SurvivedWork == nil || f.SurvivedWork.Ref != "refs/no-mistakes/survived/r/new" {
		t.Fatalf("new handoff not recorded: %+v", f)
	}
	if err := d.ReplaceRunSurvivedWork(run.ID, "", nil); err != nil {
		t.Fatal(err)
	}
	if f := got(ci.ID); f.SurvivedWork != nil {
		t.Fatalf("clear left a handoff: %+v", f)
	}
}
