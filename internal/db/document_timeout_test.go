package db

import (
	"path/filepath"
	"testing"
	"time"
)

func TestDocumentTimeoutImmutableRunPinSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := d.InsertRepo("/repo", "https://example.invalid/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err = d.PinDocumentTimeout(run.ID, 0); err == nil {
		t.Fatal("zero accepted")
	}
	if err = d.PinDocumentTimeout(run.ID, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err = d.PinDocumentTimeout(run.ID, 2*time.Hour); err == nil {
		t.Fatal("pin mutable")
	}
	d.Close()
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	got, err := d.DocumentTimeout(run.ID)
	if err != nil || got != time.Hour {
		t.Fatalf("%s %v", got, err)
	}
	next, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	got, err = d.DocumentTimeout(next.ID)
	if err != nil || got != 0 {
		t.Fatalf("new run inherited %s %v", got, err)
	}
}
