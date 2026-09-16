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
	run, err := d.InsertRunWithIntentAndLaunchNonce(repo.ID, "feature", "head", "base", nil, "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.AssociateRunWithExistingPR(run.ID, repo.ID, "feature", target); err != nil {
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
	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExistingPRURL == nil || *got.ExistingPRURL != target || got.PRURL == nil || *got.PRURL != target {
		t.Fatalf("reopen lost pin: %+v", got)
	}
}

// The run's pin and the branch's association are one fact; a caller that
// records the pin must not be able to observe a branch that still maps
// somewhere else.
func TestExplicitTargetPinsTheRunAndAssociatesTheBranchInOneWrite(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/test/repo", "https://github.com/contributor/widgets.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	target := "https://github.com/upstream/widgets/pull/168"
	run, err := d.InsertRunWithIntentAndLaunchNonce(repo.ID, "feature", "head", "base", nil, "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	// Creating the run proves nothing about the target, so it pins nothing and
	// associates nothing.
	if run.ExistingPRURL != nil || run.PRURL != nil {
		t.Fatalf("run insert pinned an unproven target: %+v", run)
	}
	if stored, err := d.GetBranchPRTarget(repo.ID, "feature"); err != nil || stored != "" {
		t.Fatalf("branch association after insert = %q (%v), want none", stored, err)
	}
	if err := d.AssociateRunWithExistingPR(run.ID, repo.ID, "feature", target); err != nil {
		t.Fatal(err)
	}
	pinned, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pinned.ExistingPRURL == nil || *pinned.ExistingPRURL != target || pinned.PRURL == nil || *pinned.PRURL != target {
		t.Fatalf("run not pinned to %s: %+v", target, pinned)
	}
	stored, err := d.GetBranchPRTarget(repo.ID, "feature")
	if err != nil {
		t.Fatal(err)
	}
	if stored != target {
		t.Fatalf("branch association = %q, want %q", stored, target)
	}
}

// A replacement that does not commit must leave the previous association
// intact: the alternative is a branch mapped to a pull request no run is
// pinned to, which the next unflagged launch would publish to.
func TestFailedExplicitAssociationLeavesThePriorAssociationIntact(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/test/repo", "https://github.com/contributor/widgets.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	original := "https://github.com/upstream/widgets/pull/168"
	first, err := d.InsertRunWithIntentAndLaunchNonce(repo.ID, "feature", "head", "base", nil, "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.AssociateRunWithExistingPR(first.ID, repo.ID, "feature", original); err != nil {
		t.Fatal(err)
	}
	// No such run: the pin cannot land, so neither may the association it
	// travels with.
	replacement := "https://github.com/upstream/widgets/pull/999"
	if err := d.AssociateRunWithExistingPR("run-that-does-not-exist", repo.ID, "feature", replacement); err == nil {
		t.Fatal("expected association against an unknown run to fail")
	}
	stored, err := d.GetBranchPRTarget(repo.ID, "feature")
	if err != nil {
		t.Fatal(err)
	}
	if stored != original {
		t.Fatalf("branch association = %q, want the original %q", stored, original)
	}
}
