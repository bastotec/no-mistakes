package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestSelectedHistoricalCustodyDoesNotBorrowSuccessorOrAlignment(t *testing.T) {
	dir, _, d, repo := setupAxiQueryRepo(t)
	run(t, dir, "git", "checkout", "-b", "feature/custody")
	chdir(t, dir)
	head, err := git.HeadSHA(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	old, err := d.InsertRun(repo.ID, "feature/custody", head, head)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(old.ID, types.RunFailed); err != nil {
		t.Fatal(err)
	}
	out := axiStatusOutput(t, old.ID)
	for _, want := range []string{"selected_run_custody:", "unknown_legacy_or_not_returned", "equal_not_custody_proof", "unknown_not_freshly_observed"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q: %s", want, out)
		}
	}
	successor, err := d.InsertRun(repo.ID, old.Branch, head, head)
	if err != nil {
		t.Fatal(err)
	}
	out = axiStatusOutput(t, old.ID)
	if !strings.Contains(out, "active_owner_run_id: \""+successor.ID+"\"") && !strings.Contains(out, "active_owner_run_id: "+successor.ID) {
		t.Fatalf("actual owner not reported: %s", out)
	}
	if !strings.Contains(out, "ownership: active_run") || !strings.Contains(out, "unknown_legacy_or_not_returned") {
		t.Fatalf("successor borrowed as selected custody: %s", out)
	}
	after, _ := d.GetRun(old.ID)
	if after.CustodyReturnedAt != nil || after.Status != types.RunFailed {
		t.Fatal("inspection mutated historical custody")
	}
}
