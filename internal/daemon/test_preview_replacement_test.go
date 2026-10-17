package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/github"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func installTestPreviewForge(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	name := "gh"
	if os.PathSeparator == '\\' {
		name += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", filepath.Join(bin, name), "../pipeline/fakecli")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fake forge: %v %s", err, out)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_CLI_MODE", "test-preview-gh")
	body := filepath.Join(t.TempDir(), "body")
	t.Setenv("FAKE_CLI_PR_BODY_FILE", body)
	return body
}

func TestManagedPreviewReplacement_AllCreationEntrypoints(t *testing.T) {
	body := installTestPreviewForge(t)
	for _, creation := range []string{"bound", "interrupted", "interrupted-multiple-prs"} {
		interrupted := creation != "bound"
		for _, path := range []string{"push-new-head", "proof-new-head", "fresh-new-head", "rerun-newer-unbound-run", "existing-pr", "history", "active-push"} {
			name := path + "-" + creation
			t.Run(name, func(t *testing.T) {
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
				if interrupted {
					if _, err := d.InsertStepResult(owner.ID, types.StepTest); err != nil {
						t.Fatal(err)
					}
					t.Setenv("FAKE_CLI_PREVIEW_CREATE_RESPONSE_LOST", "1")
					host := github.New(func(ctx context.Context, name string, args ...string) *exec.Cmd {
						return exec.CommandContext(ctx, name, args...)
					}, nil, "github.com", "test/repo")
					_, err := host.CreateTestPreview(context.Background(), "feature", "main", scm.PRContent{
						Title: "PR validation preview", Body: "<!-- no-mistakes:pr-validation:" + owner.ID + " -->",
					})
					if err == nil {
						t.Fatal("expected lost creation response")
					}
					if _, err := os.Stat(body); err != nil {
						t.Fatalf("remote draft was not created: %v", err)
					}
				} else if err := d.BindTestPreview(owner.ID, url); err != nil {
					t.Fatal(err)
				}
				host := github.New(func(ctx context.Context, name string, args ...string) *exec.Cmd {
					return exec.CommandContext(ctx, name, args...)
				}, nil, "github.com", "test/repo")
				if creation == "interrupted-multiple-prs" {
					if err := os.WriteFile(body+".other-open", nil, 0600); err != nil {
						t.Fatal(err)
					}
					pr, err := host.FindPR(context.Background(), "feature", "")
					if err != nil || pr == nil || pr.Number != "100" || pr.BaseBranch != "release" {
						t.Fatalf("expected newer unmarked PR to be discovered first: %+v, %v", pr, err)
					}
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
				if creation == "interrupted-multiple-prs" {
					if err := os.Remove(body + ".other-open"); err != nil {
						t.Fatal(err)
					}
					pr, err := host.FindPR(context.Background(), "feature", "")
					if err != nil || pr == nil || pr.URL != url || pr.BaseBranch != "main" {
						t.Fatalf("expected original preview after other PR closed: %+v, %v", pr, err)
					}
					content, err := host.GetPRContent(context.Background(), pr)
					if err != nil || content.Body != "<!-- no-mistakes:pr-validation:"+owner.ID+" -->" {
						t.Fatalf("preview ownership marker changed: %+v, %v", content, err)
					}
				}
				after, err := d.GetRunsByRepo(repo.ID)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(before, after) {
					t.Fatalf("refusal changed runs: before=%+v after=%+v", before, after)
				}
				wantBinding := url
				if interrupted {
					wantBinding = ""
				}
				if bound, err := d.TestPreviewPR(owner.ID); err != nil || bound != wantBinding {
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
}

func TestManagedPreviewReplacement_LiveLifecycle(t *testing.T) {
	installTestPreviewForge(t)
	for _, path := range []string{"push", "fresh", "rerun"} {
		for _, state := range []string{"CLOSED", "MERGED", "OPEN", "ERROR", "UNKNOWN"} {
			t.Run(path+"-"+state, func(t *testing.T) {
				t.Setenv("FAKE_CLI_PR_STATE", state)
				p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
					return []pipeline.Step{&mockPassStep{name: types.StepReview}}
				})
				repo, head := setupTestGitRepo(t, p, d, "preview-lifecycle")
				owner, err := d.InsertRun(repo.ID, "main", head, head)
				if err != nil {
					t.Fatal(err)
				}
				if err := d.UpdateRunStatus(owner.ID, types.RunCancelled); err != nil {
					t.Fatal(err)
				}
				url := "https://github.com/test/repo/pull/99"
				if err := d.BindTestPreview(owner.ID, url); err != nil {
					t.Fatal(err)
				}
				if err := d.UpdateRunPRURL(owner.ID, url); err != nil {
					t.Fatal(err)
				}
				if state == "OPEN" {
					if err := d.UpdateRunPRState(owner.ID, "closed"); err != nil {
						t.Fatal(err)
					}
				}
				before, err := d.GetRunsByRepo(repo.ID)
				if err != nil {
					t.Fatal(err)
				}
				client, err := ipc.Dial(p.Socket())
				if err != nil {
					t.Fatal(err)
				}
				defer client.Close()
				var id string
				switch path {
				case "push":
					var result ipc.PushReceivedResult
					err = client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{Gate: p.RepoDir(repo.ID), Ref: "refs/heads/main", Old: head, New: head}, &result)
					id = result.RunID
				case "fresh":
					var result ipc.StartFreshRunResult
					err = client.Call(ipc.MethodStartFreshRun, &ipc.StartFreshRunParams{RepoID: repo.ID, Branch: "main", HeadSHA: head, Intent: "validate", LaunchNonce: "replacement", ValidationGeneration: "generation"}, &result)
					id = result.Receipt.RunID
				case "rerun":
					var result ipc.RerunResult
					err = client.Call(ipc.MethodRerun, &ipc.RerunParams{RepoID: repo.ID, Branch: "main", PreviousRunID: owner.ID}, &result)
					id = result.RunID
				}
				if state == "CLOSED" || state == "MERGED" {
					if err != nil {
						t.Fatalf("confirmed terminal preview blocked launch: %v", err)
					}
					run := waitForRunTerminalState(t, d, id)
					if run.Status != types.RunCompleted || run.PRURL != nil {
						t.Fatalf("replacement inherited closed preview: %+v", run)
					}
					if bound, err := d.TestPreviewPR(id); err != nil || bound != "" {
						t.Fatalf("replacement took ownership: %q, %v", bound, err)
					}
				} else {
					if err == nil {
						t.Fatalf("accepted %s lifecycle", state)
					}
					after, readErr := d.GetRunsByRepo(repo.ID)
					if readErr != nil || !reflect.DeepEqual(before, after) {
						t.Fatalf("refusal mutated runs: %+v -> %+v, %v", before, after, readErr)
					}
				}
				if bound, err := d.TestPreviewPR(owner.ID); err != nil || bound != url {
					t.Fatalf("owner binding changed: %q, %v", bound, err)
				}
			})
		}
	}
}
