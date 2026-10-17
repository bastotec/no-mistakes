package db

import (
	"encoding/json"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestContinuationPinsSurviveRestartAndAdditiveHead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := d.InsertRepo("/test/repo", "https://example.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("a", 40)
	run, err := d.InsertRun(repo.ID, "feature", head, strings.Repeat("b", 40))
	if err != nil {
		t.Fatal(err)
	}
	pins := ContinuationPins{Version: 1, PRHead: strings.Repeat("c", 40), BaseTip: strings.Repeat("d", 40), MergeBase: strings.Repeat("b", 40), BaseBranch: "main", URL: "https://github.com/upstream/widgets/pull/168"}
	raw, _ := json.Marshal(pins)
	if err := d.AssociateRunWithExistingPR(run.ID, repo.ID, "feature", pins.URL, pins.BaseBranch, string(raw)); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunHeadSHA(run.ID, strings.Repeat("e", 40)); err != nil {
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
	actual, err := got.ContinuationPins()
	if err != nil || !reflect.DeepEqual(actual, &pins) || got.SubmittedHeadSHA == nil || *got.SubmittedHeadSHA != head {
		t.Fatalf("pins=%+v run=%+v err=%v", actual, got, err)
	}
	target, err := d.GetBranchPRTarget(repo.ID, "feature")
	if err != nil || target != pins.URL {
		t.Fatalf("target=%s err=%v", target, err)
	}
}

func TestContinuationAssociationCannotEraseOrReplacePins(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	repo, err := d.InsertRepo("/test/repo", "https://example.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", strings.Repeat("a", 40), strings.Repeat("b", 40))
	if err != nil {
		t.Fatal(err)
	}
	pins := ContinuationPins{Version: 1, PRHead: strings.Repeat("c", 40), BaseTip: strings.Repeat("d", 40), MergeBase: strings.Repeat("b", 40), BaseBranch: "main", URL: "https://github.com/upstream/widgets/pull/168"}
	raw, _ := json.Marshal(pins)
	if err := d.AssociateRunWithExistingPR(run.ID, repo.ID, "feature", pins.URL, "main", string(raw)); err != nil {
		t.Fatal(err)
	}
	if err := d.AssociateRunWithExistingPR(run.ID, repo.ID, "feature", pins.URL, "main"); err == nil {
		t.Fatal("ordinary association erased pins")
	}
	pins.BaseTip = strings.Repeat("e", 40)
	other, _ := json.Marshal(pins)
	if err := d.AssociateRunWithExistingPR(run.ID, repo.ID, "feature", pins.URL, "main", string(other)); err == nil {
		t.Fatal("association replaced pins")
	}
	got, _ := d.GetRun(run.ID)
	if *got.ContinuationJSON != string(raw) {
		t.Fatal("refusal changed pins")
	}
}
