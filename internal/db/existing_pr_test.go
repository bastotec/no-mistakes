package db

import (
	"path/filepath"
	"testing"
)

func TestExistingPRPinSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := d.InsertRepo("/test/repo", "https://github.com/contributor/widgets.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	target := "https://github.com/upstream/widgets/pull/168"
	run, err := d.InsertRunWithIntentAndLaunchNonce(repo.ID, "feature", "head", "base", nil, "", "", "", "", target)
	if err != nil {
		t.Fatal(err)
	}
	if run.ExistingPRURL == nil || *run.ExistingPRURL != target || run.PRURL == nil || *run.PRURL != target {
		t.Fatalf("creation did not atomically pin target: %+v", run)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExistingPRURL == nil || *got.ExistingPRURL != target || got.PRURL == nil || *got.PRURL != target {
		t.Fatalf("reopen lost pin: %+v", got)
	}
}
