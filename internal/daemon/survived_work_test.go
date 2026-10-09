package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
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
	sr, err := database.InsertStepResult(run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := types.MarshalFindingsJSON(types.Findings{SurvivedWork: pipeline.InspectSurvivedWork(dir)})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetStepFindings(sr.ID, raw); err != nil {
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
	cleanupOrphanWorktrees(database, p, nil)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("clean worktree not reclaimed: %v", err)
	}
}
