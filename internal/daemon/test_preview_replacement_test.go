package daemon

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Exercise the actual launch RPCs with local Git and an isolated database and
// socket. No provider, hosted preview or ambient daemon participates.
func TestManagedPreviewReplacement_AllCreationEntrypoints(t *testing.T) {
	for _, path := range []string{"push-new-head", "proof-new-head", "fresh-new-head", "rerun-newer-unbound-run", "existing-pr", "history", "active-push"} {
		t.Run(path, func(t *testing.T) {
			p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return []pipeline.Step{&mockPassStep{name: types.StepReview}} })
			repo, base := setupTestGitRepo(t, p, d, "preview-replacement")
			gate := p.RepoDir(repo.ID)
			gitCmd(t, repo.WorkingPath, "checkout", "-b", "feature")
			gitCmd(t, repo.WorkingPath, "commit", "--allow-empty", "-m", "submitted change")
			old := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
			gitCmd(t, repo.WorkingPath, "push", "gate", "feature")
			owner, err := d.InsertRun(repo.ID, "feature", old, base)
			if err != nil {
				t.Fatal(err)
			}
			if path != "active-push" {
				if err := d.UpdateRunStatus(owner.ID, types.RunFailed); err != nil {
					t.Fatal(err)
				}
			}
			url := "https://github.com/test/repo/pull/99"
			// Deliberately leave PRURL absent: a crash between the binding and the
			// convenience URL write must not erase ownership.
			if err := d.BindTestPreview(owner.ID, url); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, repo.WorkingPath, "commit", "--allow-empty", "-m", "later change")
			head := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
			gitCmd(t, repo.WorkingPath, "push", "gate", "feature")
			if path == "existing-pr" {
				// Explicit submission may advance this ref before shared run
				// setup; ownership refusal must precede that advance too.
				gitCmd(t, gate, "update-ref", "refs/heads/feature", old)
			}
			selected := owner.ID
			if path == "rerun-newer-unbound-run" {
				newer, err := d.InsertRun(repo.ID, "feature", head, base)
				if err != nil {
					t.Fatal(err)
				}
				if err := d.UpdateRunStatus(newer.ID, types.RunFailed); err != nil {
					t.Fatal(err)
				}
				selected = newer.ID
			}
			if path == "history" {
				upstream := t.TempDir()
				gitCmd(t, upstream, "init", "--bare")
				gitCmd(t, repo.WorkingPath, "remote", "add", "origin", upstream)
				gitCmd(t, repo.WorkingPath, "push", "origin", base+":refs/heads/main")
				gitCmd(t, gate, "remote", "set-url", "origin", upstream)
				// A finished run with no unpublished work has returned custody. Keep
				// the binding, which is independent of custody and run status.
				if err := d.UpdateRunStatus(owner.ID, types.RunCompleted); err != nil {
					t.Fatal(err)
				}
				if err := d.SetRunCustodyReturned(owner.ID); err != nil {
					t.Fatal(err)
				}
			}
			before, err := d.GetRunsByRepo(repo.ID)
			if err != nil {
				t.Fatal(err)
			}
			refs := gitOutput(t, gate, "show-ref")
			fresh := ipc.StartFreshRunParams{RepoID: repo.ID, Branch: "feature", HeadSHA: head, Intent: "validate feature", LaunchNonce: "replacement", ValidationGeneration: "generation"}
			method := ipc.MethodPushReceived
			var params any = &ipc.PushReceivedParams{Gate: gate, Ref: "refs/heads/feature", Old: old, New: head}
			switch path {
			case "proof-new-head":
				params = &ipc.PushReceivedParams{Gate: gate, Ref: "refs/heads/feature", Old: old, New: head, Intent: fresh.Intent, LaunchNonce: fresh.LaunchNonce, ValidationGeneration: fresh.ValidationGeneration}
			case "fresh-new-head":
				method, params = ipc.MethodStartFreshRun, &fresh
			case "rerun-newer-unbound-run":
				method, params = ipc.MethodRerun, &ipc.RerunParams{RepoID: repo.ID, Branch: "feature", PreviousRunID: selected}
			case "existing-pr":
				method, params = ipc.MethodStartExistingPRRun, &ipc.StartExistingPRRunParams{RepoID: repo.ID, Branch: "feature", HeadSHA: head, Intent: fresh.Intent, URL: url}
			case "history":
				fresh.PRBaseBranch = "main"
				method, params = ipc.MethodStartHistoryRun, &ipc.StartHistoryRunParams{StartFreshRunParams: fresh, WorkDir: repo.WorkingPath, PreserveHistoryBaseSHA: base}
			}
			client, err := ipc.Dial(p.Socket())
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			if err := client.Call(method, params, nil); err == nil || !strings.Contains(err.Error(), "managed Test preview belongs only to that run") {
				t.Fatalf("launch error=%v", err)
			}
			after, err := d.GetRunsByRepo(repo.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("refusal changed runs: before=%+v after=%+v", before, after)
			}
			if bound, err := d.TestPreviewPR(owner.ID); err != nil || bound != url {
				t.Fatalf("binding=%q, %v", bound, err)
			}
			if got := gitOutput(t, gate, "show-ref"); got != refs {
				t.Fatalf("refusal changed refs: %s -> %s", refs, got)
			}
			if target, err := d.GetBranchPRTarget(repo.ID, "feature"); err != nil || target != "" {
				t.Fatalf("refusal changed association: %q, %v", target, err)
			}
		})
	}
}
