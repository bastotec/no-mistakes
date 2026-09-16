package db

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestHistoryPinsSurviveRestartAndHeadAdvancement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := d.InsertRepo("/test/repo", "https://example.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	head, base, repair := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	run, err := d.InsertRunWithIntentAndLaunchNonce(repo.ID, "integration", head, base, nil, "nonce", "generation", "digest", "main", base)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := d.InsertRun(repo.ID, "ordinary", head, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunHeadSHA(run.ID, repair); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
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
	if _, err := d.RecoverStaleRuns("test restart"); err != nil {
		t.Fatal(err)
	}
	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.HeadSHA != repair || got.SubmittedHeadSHA == nil || *got.SubmittedHeadSHA != head ||
		got.PreserveHistoryBaseSHA == nil || *got.PreserveHistoryBaseSHA != base || got.PRBaseBranch == nil || *got.PRBaseBranch != "main" {
		t.Fatalf("restart lost immutable pins: %+v", got)
	}
	got, err = d.GetRun(legacy.ID)
	if err != nil || got.PreserveHistoryBaseSHA != nil {
		t.Fatalf("default caller unexpectedly constrained: %+v, %v", got, err)
	}
}
