package db

import (
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestTestIsolationPinSurvivesRecoveryAndDoesNotInfectNewRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := d.InsertRepo("/repo", "https://example.invalid/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	isolated, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	ordinary, err := d.InsertRun(repo.ID, "ordinary", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.EnableTestIsolation(isolated.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.EnableTestIsolation(isolated.ID); err == nil {
		t.Fatal("isolation pin is mutable")
	}
	d.UpdateRunStatus(isolated.ID, types.RunRunning)
	d.Close()
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.RecoverStaleRuns("test restart"); err != nil {
		t.Fatal(err)
	}
	enabled, err := d.TestIsolationEnabled(isolated.ID)
	if err != nil || !enabled {
		t.Fatalf("recovery lost isolation: %v %v", enabled, err)
	}
	enabled, err = d.TestIsolationEnabled(ordinary.ID)
	if err != nil || enabled {
		t.Fatalf("ordinary run opted in: %v %v", enabled, err)
	}
	rerun, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	enabled, err = d.TestIsolationEnabled(rerun.ID)
	if err != nil || enabled {
		t.Fatalf("new run inherited opt-in: %v %v", enabled, err)
	}
}

func TestTestSnapshotContainsOnlyThisRunAndNoConfigProvenance(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "daemon.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	repo, err := d.InsertRepo("/repo", "https://user:secret@example.invalid/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := d.InsertRun(repo.ID, "foreign", "other", "base")
	if err != nil {
		t.Fatal(err)
	}
	step, err := d.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[]}`
	if _, err := d.InsertReviewStepRoundWithProvenance(step.ID, 1, "initial", &findings, nil, "head", "base", "trusted", []byte("operator-secret"), []byte("repo-secret"), 1); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "snapshot.sqlite")
	if err := d.ExportTestSnapshot(path, run.ID); err != nil {
		t.Fatal(err)
	}
	snapshot, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	if got, err := snapshot.GetRun(foreign.ID); err != nil || got != nil {
		t.Fatalf("foreign authority leaked: %+v %v", got, err)
	}
	copied, err := snapshot.GetRepo(repo.ID)
	if err != nil || copied == nil || copied.UpstreamURL != "" || copied.ForkURL != "" {
		t.Fatalf("remote credential leaked: %+v %v", copied, err)
	}
	rounds, err := snapshot.GetRoundsByStep(step.ID)
	if err != nil || len(rounds) != 1 {
		t.Fatalf("advisory history missing: %v %v", rounds, err)
	}
	if len(rounds[0].GlobalConfigYAML) != 0 || len(rounds[0].RepoConfigYAML) != 0 {
		t.Fatal("raw config provenance leaked")
	}
	if err := snapshot.UpdateRunHeadSHA(run.ID, "worker-only"); err != nil {
		t.Fatal(err)
	}
	live, err := d.GetRun(run.ID)
	if err != nil || live.HeadSHA != "head" {
		t.Fatalf("snapshot write reached daemon: %+v %v", live, err)
	}
}
