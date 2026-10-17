package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type previewReviewStep struct{}

func (*previewReviewStep) Name() types.StepName { return types.StepReview }
func (*previewReviewStep) Execute(s *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	return &pipeline.StepOutcome{ReviewApprovedHeadSHA: s.Run.HeadSHA}, nil
}

type previewParkStep struct{ env []string }

func (*previewParkStep) Name() types.StepName { return types.StepTest }
func (s *previewParkStep) Execute(ctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	ctx.Env = s.env
	return &pipeline.StepOutcome{NeedsApproval: true, Findings: `{"findings":[],"summary":"Waiting for PR validation preview"}`}, nil
}

// An actual isolated daemon/socket and real bare Git remote exercise dispatch,
// executor custody and publication together. Only the forge is a tiny fake CLI;
// no ambient database, service, daemon socket or network is used.
func TestPublishTestPreviewRPC_IsolatedDaemonKeepsTestParked(t *testing.T) {
	bin := t.TempDir()
	gh := filepath.Join(bin, "gh")
	if os.PathSeparator == '\\' {
		gh += ".exe"
	}
	build := exec.Command("go", "build", "-o", gh, "../pipeline/fakecli")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fake forge: %v %s", err, out)
	}
	body := filepath.Join(t.TempDir(), "body")
	parked := &previewParkStep{}
	p, database := startTestDaemonWithSteps(t, func() []pipeline.Step { return []pipeline.Step{&previewReviewStep{}, parked} })
	repo, head := setupTestGitRepo(t, p, database, "test-preview-rpc")
	gate := p.RepoDir(repo.ID)
	remote := filepath.Join(t.TempDir(), "upstream.git")
	gitCmd(t, "", "clone", "--bare", gate, remote)
	gitCmd(t, gate, "remote", "set-url", "origin", remote)
	gitCmd(t, gate, "update-ref", "refs/heads/feature", head)
	parked.env = []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), "FAKE_CLI_MODE=test-preview-gh", "FAKE_CLI_PR_BODY_FILE=" + body, "FAKE_CLI_PR_HEAD_SHA=" + head}
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var started ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{Gate: gate, Ref: "refs/heads/feature", New: head}, &started); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		run, err := database.GetRun(started.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if run.AwaitingAgentSince != nil {
			break
		}
		if run.Status.Terminal() || time.Now().After(deadline) {
			t.Fatalf("Test never parked: %+v", run)
		}
		time.Sleep(10 * time.Millisecond)
	}
	before, err := database.GetStepsByRun(started.RunID)
	if err != nil {
		t.Fatal(err)
	}
	params := ipc.PublishTestPreviewParams{RepoID: repo.ID, RunID: started.RunID, HeadSHA: head}
	for i := 0; i < 2; i++ {
		var result ipc.PublishTestPreviewResult
		if err := client.CallWithContext(context.Background(), ipc.MethodPublishTestPreview, params, &result, time.Minute); err != nil {
			t.Fatal(err)
		}
		if result.PRURL != "https://github.com/test/repo/pull/99" || result.TestStatus != "awaiting_approval" {
			t.Fatalf("result=%+v", result)
		}
	}
	after, _ := database.GetStepsByRun(started.RunID)
	// Publication writes an audit log, whose activity timestamps are only
	// observability. Every verdict, gate, findings and repair field stays put.
	for _, row := range append(before, after...) {
		row.LastActivityAt = nil
		row.LastActivity = nil
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("publication changed gate rows: before=%+v after=%+v", before, after)
	}
	if got := gitOutput(t, remote, "rev-parse", "refs/heads/feature"); got != head {
		t.Fatalf("remote=%s", got)
	}
	params.HeadSHA = "stale"
	if err := client.Call(ipc.MethodPublishTestPreview, params, nil); err == nil {
		t.Fatal("accepted stale head")
	}
	if err := client.Call(ipc.MethodCancelRun, &ipc.CancelRunParams{RunID: started.RunID}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestReplacementLaunchRefusesManagedTestPreview(t *testing.T) {
	p, database := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepReview}}
	})
	repo, head := setupTestGitRepo(t, p, database, "preview-rerun")
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	request := ipc.StartFreshRunParams{RepoID: repo.ID, Branch: "main", HeadSHA: head, Intent: "validate feature", LaunchNonce: "first-preview-launch", ValidationGeneration: "generation"}
	var first ipc.StartFreshRunResult
	if err := client.Call(ipc.MethodStartFreshRun, &request, &first); err != nil {
		t.Fatal(err)
	}
	original := waitForRunTerminalState(t, database, first.Receipt.RunID)
	url := "https://github.com/test/repo/pull/99"
	if err := database.BindTestPreview(original.ID, url); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunPRURL(original.ID, url); err != nil {
		t.Fatal(err)
	}
	var replay ipc.StartFreshRunResult
	if err := client.Call(ipc.MethodStartFreshRun, &request, &replay); err != nil || replay.Receipt.RunID != original.ID || replay.Receipt.Disposition != "reused" {
		t.Fatalf("same-run replay=%+v error=%v", replay, err)
	}
	before, err := database.GetRunsByRepo(repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	request.LaunchNonce = "replacement-preview-launch"
	for _, launch := range []struct {
		name, method string
		params       any
	}{
		{"rerun", ipc.MethodRerun, &ipc.RerunParams{RepoID: repo.ID, Branch: "main", PreviousRunID: original.ID}},
		{"proof-mode", ipc.MethodStartFreshRun, &request},
	} {
		t.Run(launch.name, func(t *testing.T) {
			err := client.Call(launch.method, launch.params, nil)
			if err == nil || !strings.Contains(err.Error(), "managed Test preview belongs only to that run") {
				t.Fatalf("replacement error=%v", err)
			}
			after, err := database.GetRunsByRepo(repo.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("refused handoff changed runs: before=%+v after=%+v", before, after)
			}
			if bound, err := database.TestPreviewPR(original.ID); err != nil || bound != url {
				t.Fatalf("binding=%s, %v", bound, err)
			}
			if got := gitOutput(t, p.RepoDir(repo.ID), "rev-parse", "refs/heads/main"); got != head {
				t.Fatalf("refused handoff moved gate head to %s", got)
			}
		})
	}
}
