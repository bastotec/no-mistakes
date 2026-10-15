package db

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRunSurvivedWorkReplacesAndClears(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/tmp/repo", "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature/x", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if work, err := d.GetRunSurvivedWork(run.ID); err != nil || work != nil {
		t.Fatalf("empty run = %+v %v", work, err)
	}
	first := &types.SurvivedWork{State: types.SurvivedWorkCaptured, Worktree: "/wt", Files: []string{"?? a.txt"}, Ref: "refs/no-mistakes/survived/r/1", BaseSHA: "abc", Next: "n"}
	if err := d.SetRunSurvivedWork(run.ID, first); err != nil {
		t.Fatal(err)
	}
	second := &types.SurvivedWork{State: types.SurvivedWorkInPlace, Worktree: "/wt", InspectionError: "e", Next: "m"}
	if err := d.SetRunSurvivedWork(run.ID, second); err != nil {
		t.Fatal(err)
	}
	got, err := d.GetRunSurvivedWork(run.ID)
	if err != nil || got == nil || got.State != types.SurvivedWorkInPlace || got.Ref != "" || got.InspectionError != "e" || got.Next != "m" {
		t.Fatalf("replaced handoff = %+v %v", got, err)
	}
	if err := d.ClearRunSurvivedWork(run.ID); err != nil {
		t.Fatal(err)
	}
	if work, err := d.GetRunSurvivedWork(run.ID); err != nil || work != nil {
		t.Fatalf("cleared handoff = %+v %v", work, err)
	}
}

func TestBoundSurvivedWorkFiles(t *testing.T) {
	files := make([]string, types.SurvivedWorkMaxFiles+7)
	bounded := types.BoundSurvivedWorkFiles(files)
	if len(bounded) != types.SurvivedWorkMaxFiles+1 || bounded[types.SurvivedWorkMaxFiles] != "... and 7 more" {
		t.Fatalf("bounded = %d entries, last %q", len(bounded), bounded[len(bounded)-1])
	}
}

// Consuming a ref retires only captured copies: a run that restored it and
// never settled keeps its row, and the ref with it.
func TestClearSurvivedWorkRefKeepsRestoredHandoffs(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/tmp/repo", "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	var runs []string
	for i := 0; i < 2; i++ {
		run, err := d.InsertRun(repo.ID, "feature/x", "head", "base")
		if err != nil {
			t.Fatal(err)
		}
		runs = append(runs, run.ID)
	}
	ref := "refs/no-mistakes/survived/a/1"
	if err := d.SetRunSurvivedWork(runs[0], &types.SurvivedWork{State: types.SurvivedWorkCaptured, Ref: ref, Next: "n"}); err != nil {
		t.Fatal(err)
	}
	if err := d.SetRunSurvivedWork(runs[1], &types.SurvivedWork{State: types.SurvivedWorkRestored, Ref: ref, Next: "n"}); err != nil {
		t.Fatal(err)
	}
	inUse, err := d.ClearSurvivedWorkRef(ref)
	if err != nil || !inUse {
		t.Fatalf("ClearSurvivedWorkRef = %v, %v; want the restored row to keep the ref in use", inUse, err)
	}
	if w, _ := d.GetRunSurvivedWork(runs[0]); w != nil {
		t.Fatalf("captured copy kept: %+v", w)
	}
	if w, _ := d.GetRunSurvivedWork(runs[1]); w == nil {
		t.Fatal("restored handoff retired")
	}
}
