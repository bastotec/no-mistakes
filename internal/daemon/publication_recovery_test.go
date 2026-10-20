package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

func TestPublicationRecoveryRPCRefusesBeforeWorktreeOrRunMutation(t *testing.T) {
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return nil })
	repo, head := setupTestGitRepo(t, p, d, "publication-refusal")
	run, err := d.InsertRun(repo.ID, "feature", head, head)
	if err != nil {
		t.Fatal(err)
	}
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var result ipc.RecoverPublicationResult
	err = client.Call(ipc.MethodRecoverPublication, &ipc.RecoverPublicationParams{RepoID: repo.ID, RunID: run.ID}, &result)
	if err == nil || !strings.Contains(err.Error(), "terminal and failed") {
		t.Fatalf("RPC error=%v", err)
	}
	after, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != run.Status || after.HeadSHA != head || after.PRURL != nil {
		t.Fatalf("refusal changed run: %+v", after)
	}
	mgr := NewRunManager(d, p, nil)
	if _, err := mgr.HandleRecoverPublication(context.Background(), &ipc.RecoverPublicationParams{RepoID: "other", RunID: run.ID}); err == nil || !strings.Contains(err.Error(), "not found in this repository") {
		t.Fatalf("cross-repository recovery error=%v", err)
	}
}
