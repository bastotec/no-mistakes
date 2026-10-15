package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestSurvivedWorkRetainedAfterAbortAndStartupCleanup(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repo, head := setupTestGitRepo(t, p, database, "survived-repair")
	run, err := database.InsertRun(repo.ID, "main", head, head)
	if err != nil {
		t.Fatal(err)
	}
	dir := p.WorktreeDir(repo.ID, run.ID)
	if err := git.WorktreeAdd(context.Background(), p.RepoDir(repo.ID), dir, head); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "partial.txt")
	if err := os.WriteFile(file, []byte("survived"), 0600); err != nil {
		t.Fatal(err)
	}
	// Work that could not be captured stays in the worktree.
	if err := database.SetRunSurvivedWork(run.ID, &types.SurvivedWork{State: types.SurvivedWorkInPlace, Worktree: dir, Files: []string{"?? partial.txt"}}); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunCancelled); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunError(run.ID, types.RunCancelReasonAbortedByUser); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reason := survivedWorkCleanupReason(database, run, dir); reason == "" {
		t.Fatal("abort released dirty work")
	}
	cleanupOrphanWorktrees(database, p, nil)
	if data, err := os.ReadFile(file); err != nil || string(data) != "survived" {
		t.Fatalf("cleanup erased work: %s %v", data, err)
	}
	if reason := survivedWorkCleanupReason(database, run, filepath.Join(dir, "missing")); reason == "" {
		t.Fatal("unreadable status released work")
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if reason := survivedWorkCleanupReason(database, run, dir); reason != "" {
		t.Fatalf("clean worktree retained: %s", reason)
	}
	if left, err := database.GetRunSurvivedWork(run.ID); err != nil || left != nil {
		t.Fatalf("stale survival metadata remains: %+v %v", left, err)
	}
	cleanupOrphanWorktrees(database, p, nil)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("clean worktree not reclaimed: %v", err)
	}
}

// Captured survived work lives in a run-owned ref, so it never holds the
// worktree, even when the worktree is dirty for another reason. Restored work
// does.
func TestCapturedSurvivedWorkDoesNotHoldTheWorktreeButRestoredWorkDoes(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repo, head := setupTestGitRepo(t, p, database, "survived-captured")
	run, err := database.InsertRun(repo.ID, "main", head, head)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "other.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	work := &types.SurvivedWork{State: types.SurvivedWorkCaptured, Worktree: dir, Ref: "refs/no-mistakes/survived/" + run.ID + "/abc", BaseSHA: head}
	if err := database.SetRunSurvivedWork(run.ID, work); err != nil {
		t.Fatal(err)
	}
	if reason := survivedWorkCleanupReason(database, run, dir); reason != "" {
		t.Fatalf("captured work held the worktree: %s", reason)
	}
	// Restored for a round a shutdown cut off: the worktree may hold more
	// than the ref, so it is held while dirty.
	work.State = types.SurvivedWorkRestored
	if err := database.SetRunSurvivedWork(run.ID, work); err != nil {
		t.Fatal(err)
	}
	if reason := survivedWorkCleanupReason(database, run, dir); reason == "" {
		t.Fatal("restored work released a dirty worktree")
	}
}

// A rerun copies a terminal run's captured repair work, naming its
// recorded worktree, so its next authorized repair can restore it; work
// still in the old worktree stays.
func TestCarrySurvivedWorkMovesOnlyCapturedWork(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repo, head := setupTestGitRepo(t, p, database, "survived-carry")
	old, err := database.InsertRun(repo.ID, "main", head, head)
	if err != nil {
		t.Fatal(err)
	}
	next, err := database.InsertRun(repo.ID, "main", head, head)
	if err != nil {
		t.Fatal(err)
	}
	m := NewRunManager(database, p, nil)
	captured := &types.SurvivedWork{State: types.SurvivedWorkCaptured, Worktree: "/old", Ref: "refs/no-mistakes/survived/" + old.ID + "/abc", BaseSHA: head, Next: "n"}
	if err := database.SetRunSurvivedWork(old.ID, captured); err != nil {
		t.Fatal(err)
	}
	ctx := withSurvivedWorkFrom(context.Background(), old.ID)
	if err := m.carrySurvivedWork(ctx, next.ID, "/custom/root/wt"); err != nil {
		t.Fatal(err)
	}
	moved, err := database.GetRunSurvivedWork(next.ID)
	if err != nil || moved == nil || moved.Ref != captured.Ref || moved.Worktree != "/custom/root/wt" {
		t.Fatalf("carried handoff = %+v %v", moved, err)
	}
	// The original stays until the work is consumed; consuming it retires
	// every copy.
	if left, _ := database.GetRunSurvivedWork(old.ID); left == nil {
		t.Fatal("the copy consumed the original handoff")
	}
	if inUse, err := database.ClearSurvivedWorkRef(captured.Ref); err != nil || inUse {
		t.Fatalf("ClearSurvivedWorkRef = %v, %v", inUse, err)
	}
	for _, id := range []string{old.ID, next.ID} {
		if left, _ := database.GetRunSurvivedWork(id); left != nil {
			t.Fatalf("run %s kept a consumed handoff: %+v", id, left)
		}
	}

	inPlace := &types.SurvivedWork{State: types.SurvivedWorkInPlace, Worktree: "/old", Next: "n"}
	if err := database.SetRunSurvivedWork(old.ID, inPlace); err != nil {
		t.Fatal(err)
	}
	third, err := database.InsertRun(repo.ID, "main", head, head)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.carrySurvivedWork(ctx, third.ID, "/wt3"); err != nil {
		t.Fatal(err)
	}
	if moved, _ := database.GetRunSurvivedWork(third.ID); moved != nil {
		t.Fatalf("in-place work moved off its worktree: %+v", moved)
	}
}

// A ref-backed in-place handoff whose worktree is clean is all in its ref:
// cleanup records it as captured, so a rerun inherits it.
func TestCleanRefBackedInPlaceWorkBecomesCaptured(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repo, head := setupTestGitRepo(t, p, database, "survived-promote")
	run, err := database.InsertRun(repo.ID, "main", head, head)
	if err != nil {
		t.Fatal(err)
	}
	dir := p.WorktreeDir(repo.ID, run.ID)
	if err := git.WorktreeAdd(context.Background(), p.RepoDir(repo.ID), dir, head); err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunSurvivedWork(run.ID, &types.SurvivedWork{State: types.SurvivedWorkInPlace, Worktree: dir, Ref: "refs/no-mistakes/survived/" + run.ID + "/abc", BaseSHA: head, InspectionError: "final write failed", Next: "n"}); err != nil {
		t.Fatal(err)
	}
	if reason := survivedWorkCleanupReason(database, run, dir); reason != "" {
		t.Fatalf("clean worktree retained: %s", reason)
	}
	work, err := database.GetRunSurvivedWork(run.ID)
	if err != nil || work == nil || work.State != types.SurvivedWorkCaptured {
		t.Fatalf("handoff after cleanup = %+v %v", work, err)
	}
}
