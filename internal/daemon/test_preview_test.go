package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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
