package db

import (
	"testing"
)

func TestPublicationIntentIsProspectiveAndImmutable(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo(t.TempDir(), "https://github.com/example/project", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	eligible, err := d.PublicationRecoveryEligible(run.ID)
	if err != nil || !eligible {
		t.Fatalf("new eligibility=%v %v", eligible, err)
	}
	if err := d.PinPublicationIntent(run.ID, "head", `{"base":"main"}`); err != nil {
		t.Fatal(err)
	}
	if err := d.PinPublicationIntent(run.ID, "head", `{"base":"main"}`); err != nil {
		t.Fatal(err)
	}
	if err := d.PinPublicationIntent(run.ID, "head", `{"base":"different"}`); err == nil {
		t.Fatal("immutable pin replaced")
	}
	if err := d.RecordPublicationRefusal(run.ID, "head", "original refusal"); err != nil {
		t.Fatal(err)
	}
	if err := d.RecordPublicationRefusal(run.ID, "head", "later refusal"); err != nil {
		t.Fatal(err)
	}
	pin, err := d.GetPublicationIntent(run.ID, "head")
	if err != nil || pin.Refusal == nil || *pin.Refusal != "original refusal" {
		t.Fatalf("pin=%+v err=%v", pin, err)
	}
}

func TestPublicationIntentHistoricalRunCannotBePinned(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo(t.TempDir(), "https://github.com/example/project", "main")
	if err != nil {
		t.Fatal(err)
	}
	// Migration/default values, not a new writer's eligibility marker.
	_, err = d.sql.Exec(`INSERT INTO runs (id, repo_id, branch, head_sha, base_sha, status, created_at, updated_at) VALUES ('historical', ?, 'feature', 'head', 'base', 'failed', 1, 1)`, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	eligible, err := d.PublicationRecoveryEligible("historical")
	if err != nil || eligible {
		t.Fatalf("historical eligibility=%v %v", eligible, err)
	}
	if err := d.PinPublicationIntent("historical", "head", `{"base":"main"}`); err != nil {
		t.Fatal(err)
	}
	pin, err := d.GetPublicationIntent("historical", "head")
	if err != nil || pin != nil {
		t.Fatalf("historical run gained pin: %+v %v", pin, err)
	}
}
