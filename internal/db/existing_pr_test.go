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
	run, err := d.InsertRunWithIntentAndLaunchNonce(repo.ID, "feature", "head", "base", nil, "", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.AssociateRunWithExistingPR(run.ID, repo.ID, "feature", target, "release/2.0"); err != nil {
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
	run, err := d.InsertRunWithIntentAndLaunchNonce(repo.ID, "feature", "head", "base", nil, "", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	// Creating the run proves nothing about the target, so it pins nothing,
	// associates nothing, and records no base to integrate with.
	if run.ExistingPRURL != nil || run.PRURL != nil || run.PRBaseBranch != nil {
		t.Fatalf("run insert pinned an unproven target: %+v", run)
	}
	if stored, err := d.GetBranchPRTarget(repo.ID, "feature"); err != nil || stored != "" {
		t.Fatalf("branch association after insert = %q (%v), want none", stored, err)
	}
	if err := d.AssociateRunWithExistingPR(run.ID, repo.ID, "feature", target, "release/2.0"); err != nil {
		t.Fatal(err)
	}
	pinned, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pinned.ExistingPRURL == nil || *pinned.ExistingPRURL != target || pinned.PRURL == nil || *pinned.PRURL != target {
		t.Fatalf("run not pinned to %s: %+v", target, pinned)
	}
	// A base with no pin beside it reads as an operator --base-branch override
	// to the next rerun, so the two are durable together or not at all.
	if pinned.PRBaseBranch == nil || *pinned.PRBaseBranch != "release/2.0" {
		t.Fatalf("pull request base did not ride the pin: %+v", pinned.PRBaseBranch)
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
	first, err := d.InsertRunWithIntentAndLaunchNonce(repo.ID, "feature", "head", "base", nil, "", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.AssociateRunWithExistingPR(first.ID, repo.ID, "feature", original, "main"); err != nil {
		t.Fatal(err)
	}
	// No such run: the pin cannot land, so neither may the association it
	// travels with.
	replacement := "https://github.com/upstream/widgets/pull/999"
	if err := d.AssociateRunWithExistingPR("run-that-does-not-exist", repo.ID, "feature", replacement, "main"); err == nil {
		t.Fatal("expected association against an unknown run to fail")
	}
	stored, err := d.GetBranchPRTarget(repo.ID, "feature")
	if err != nil {
		t.Fatal(err)
	}
	if stored != original {
		t.Fatalf("branch association = %q, want the original %q", stored, original)
	}
	kept, err := d.GetRun(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if kept.PRBaseBranch == nil || *kept.PRBaseBranch != "main" {
		t.Fatalf("failed association disturbed the prior base: %+v", kept.PRBaseBranch)
	}
}
