package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
	toon "github.com/toon-format/toon-go"
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
	if !strings.Contains(out, "cached_branch_run_id: "+successor.ID) && !strings.Contains(out, "cached_branch_run_id: \""+successor.ID+"\"") {
		t.Fatalf("caller's actual cached owner missing: %s", out)
	}
	if strings.Contains(out, "branch_sync:") {
		t.Fatalf("successor's sync state attached as selected run: %s", out)
	}
	after, _ := d.GetRun(old.ID)
	if after.CustodyReturnedAt != nil || after.Status != types.RunFailed {
		t.Fatal("inspection mutated historical custody")
	}
}

func TestSelectedCustodyRequiresRegisteredCallerRepository(t *testing.T) {
	dir, _, d, repo := setupAxiQueryRepo(t)
	run(t, dir, "git", "checkout", "-b", "feature/custody")
	head := cliGit(t, dir, "rev-parse", "HEAD")
	selected, err := d.InsertRun(repo.ID, "feature/custody", head, head)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(selected.ID, types.RunFailed); err != nil {
		t.Fatal(err)
	}
	owner, err := d.InsertRun(repo.ID, selected.Branch, head, head)
	if err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(t.TempDir(), "foreign")
	run(t, dir, "git", "clone", dir, foreign)
	foreignRoot, err := git.FindGitRoot(foreign)
	if err != nil {
		t.Fatal(err)
	}
	foreignRepo, err := d.InsertRepoWithID("repo-2", foreignRoot, "origin", "main")
	if err != nil {
		t.Fatal(err)
	}
	foreignRun, err := d.InsertRun(foreignRepo.ID, selected.Branch, head, head)
	if err != nil {
		t.Fatal(err)
	}
	unregistered := filepath.Join(t.TempDir(), "unregistered")
	run(t, dir, "git", "clone", dir, unregistered)
	run(t, dir, "git", "checkout", "-b", "feature/other")
	beforeSelected, _ := d.GetRun(selected.ID)
	beforeOwner, _ := d.GetRun(owner.ID)
	beforeForeign, _ := d.GetRun(foreignRun.ID)
	before, err := json.Marshal([]any{beforeSelected, beforeOwner, beforeForeign})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		dir  string
	}{
		{"foreign_same_branch_and_head", foreign},
		{"unregistered_same_branch_and_head", unregistered},
		{"same_repository_other_branch", dir},
		{"outside_git", t.TempDir()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chdir(t, tc.dir)
			out := axiStatusOutput(t, selected.ID)
			var doc struct {
				Custody struct {
					SelectedID    string `toon:"selected_run_id"`
					OwnerID       string `toon:"active_owner_run_id"`
					LocalHead     string `toon:"local_head"`
					HeadAlignment string `toon:"head_alignment"`
					RemoteBinding string `toon:"remote_binding"`
				} `toon:"selected_run_custody"`
			}
			if err := toon.UnmarshalString(out, &doc); err != nil {
				t.Fatal(err)
			}
			if doc.Custody.SelectedID != selected.ID || doc.Custody.OwnerID != owner.ID {
				t.Fatalf("selected repository evidence lost: %s", out)
			}
			if doc.Custody.LocalHead != "" || doc.Custody.HeadAlignment != "unknown" || doc.Custody.RemoteBinding != "unknown_not_freshly_observed" {
				t.Fatalf("unrelated caller supplied local evidence: %s", out)
			}
			for _, unwanted := range []string{"cached_branch_run_id:", "cached_branch_state:", "cached_remote_head:", "cached_remote_freshness:", "branch_sync:", foreignRun.ID} {
				if strings.Contains(out, unwanted) {
					t.Fatalf("caller evidence %q attached to selected repository: %s", unwanted, out)
				}
			}
		})
	}
	afterSelected, _ := d.GetRun(selected.ID)
	afterOwner, _ := d.GetRun(owner.ID)
	afterForeign, _ := d.GetRun(foreignRun.ID)
	after, err := json.Marshal([]any{afterSelected, afterOwner, afterForeign})
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("inspection changed run evidence: before=%s after=%s", before, after)
	}
}

func TestAxiStatusExplicitCustodyInspectsCallerOnce(t *testing.T) {
	f := newCLISyncFixture(t)
	tracePath := filepath.Join(t.TempDir(), "git-trace.json")
	t.Setenv("GIT_TRACE2_EVENT", tracePath)
	out := axiStatusOutput(t, f.runID)
	var doc struct {
		Custody struct {
			LocalHead       string `toon:"local_head"`
			CachedRunID     string `toon:"cached_branch_run_id"`
			CachedRemote    string `toon:"cached_remote_head"`
			CachedFreshness string `toon:"cached_remote_freshness"`
		} `toon:"selected_run_custody"`
		Sync struct {
			Local struct {
				Head string `toon:"head"`
			} `toon:"local"`
			Pipeline struct {
				Run string `toon:"run"`
			} `toon:"pipeline"`
			Remote struct {
				Head      string `toon:"observed_head"`
				Freshness string `toon:"freshness"`
			} `toon:"remote"`
		} `toon:"branch_sync"`
	}
	if err := toon.UnmarshalString(out, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Custody.LocalHead != f.old || doc.Custody.CachedRunID != f.runID || doc.Custody.CachedRemote != f.pushed || doc.Custody.CachedFreshness != "pipeline_push" {
		t.Fatalf("same-repository cached evidence missing: %s", out)
	}
	if doc.Sync.Local.Head != doc.Custody.LocalHead || doc.Sync.Pipeline.Run != doc.Custody.CachedRunID || doc.Sync.Remote.Head != doc.Custody.CachedRemote || doc.Sync.Remote.Freshness != doc.Custody.CachedFreshness {
		t.Fatalf("status renderers disagree about cached observation: %s", out)
	}
	trace, err := os.Open(tracePath)
	if err != nil {
		t.Fatal(err)
	}
	defer trace.Close()
	decoder := json.NewDecoder(trace)
	inspections := 0
	for {
		var event struct {
			Event string   `json:"event"`
			Argv  []string `json:"argv"`
		}
		if err := decoder.Decode(&event); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if event.Event == "start" && len(event.Argv) > 1 && slices.Equal(event.Argv[1:], []string{"status", "--porcelain"}) {
			inspections++
		}
	}
	if inspections != 1 {
		t.Fatalf("caller cleanliness inspected %d times, want one cached inspection", inspections)
	}
}
