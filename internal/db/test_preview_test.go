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
}
