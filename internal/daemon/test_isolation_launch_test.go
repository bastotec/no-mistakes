package daemon

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestIsolatedLaunchRefusesMissingPrerequisitesWithoutCreatingRun(t *testing.T) {
	t.Setenv("NM_TEST_POSTGRES_ROOT", "")
	p, database := startTestDaemonWithSteps(t, func() []pipeline.Step { return []pipeline.Step{&mockPassStep{name: types.StepTest}} })
	repo, head := setupTestGitRepo(t, p, database, "isolation-refusal")
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var result ipc.StartFreshRunResult
	err = client.Call(ipc.MethodStartIsolatedTestRun, &ipc.StartHistoryRunParams{StartFreshRunParams: ipc.StartFreshRunParams{RepoID: repo.ID, Branch: "feature", HeadSHA: head, Intent: "fixture", LaunchNonce: "isolated-nonce", ValidationGeneration: "fixture"}, WorkDir: repo.WorkingPath}, &result)
	if err == nil || !strings.Contains(err.Error(), "test-isolation-") {
		t.Fatalf("missing prerequisite did not fail closed: %v", err)
	}
	runs, err := database.GetRunsByRepo(repo.ID)
	if err != nil || len(runs) != 0 {
		t.Fatalf("refusal created a run: %+v %v", runs, err)
	}
}
