package db

import (
	"path/filepath"
	"testing"
)

func TestTestPreviewBindingSurvivesReopenAndRefusesReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := d.InsertRepo(t.TempDir(), "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	const url = "https://github.com/test/repo/pull/99"
	for i := 0; i < 2; i++ {
		if err := d.BindTestPreview(run.ID, url); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.BindTestPreview(run.ID, "https://github.com/test/repo/pull/100"); err == nil {
		t.Fatal("replaced binding")
	}
	if needed, err := d.TestPreviewNeedsValidation(run.ID); err != nil || !needed {
		t.Fatalf("new preview validation=%v, %v", needed, err)
	}
	if err := d.RecordTestPreviewValidation(run.ID, "head"); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if got, err := d.TestPreviewPR(run.ID); err != nil || got != url {
		t.Fatalf("binding=%q,%v", got, err)
	}
	if runs, err := d.BranchTestPreviewRuns(repo.ID, "feature"); err != nil || len(runs) != 1 || runs[0].RunID != run.ID || runs[0].PRURL != url {
		t.Fatalf("branch preview after reopen=%+v,%v", runs, err)
	}
	if runs, err := d.BranchTestPreviewRuns(repo.ID, "other"); err != nil || len(runs) != 0 {
		t.Fatalf("unrelated branch previews=%+v,%v", runs, err)
	}
	if needed, err := d.TestPreviewNeedsValidation(run.ID); err != nil || needed {
		t.Fatalf("persisted validation=%v, %v", needed, err)
	}
	if err := d.UpdateRunHeadSHA(run.ID, "changed"); err != nil {
		t.Fatal(err)
	}
	if needed, err := d.TestPreviewNeedsValidation(run.ID); err != nil || needed {
		t.Fatalf("post-validation head advance=%v, %v", needed, err)
	}
	if err := d.RequireTestPreviewValidation(run.ID); err != nil {
		t.Fatal(err)
	}
	if needed, err := d.TestPreviewNeedsValidation(run.ID); err != nil || !needed {
		t.Fatalf("republished validation=%v, %v", needed, err)
	}
	for _, state := range []string{"closed", " MERGED "} {
		if err := d.UpdateRunPRState(run.ID, state); err != nil {
			t.Fatal(err)
		}
		if runs, err := d.BranchTestPreviewRuns(repo.ID, "feature"); err != nil || len(runs) != 1 || runs[0].PRURL != url {
			t.Fatalf("cached %s lifecycle hid preview: %+v,%v", state, runs, err)
		}
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.BranchTestPreviewRuns(repo.ID, "feature"); err == nil {
		t.Fatal("unreadable preview ownership did not fail closed")
	}
}
