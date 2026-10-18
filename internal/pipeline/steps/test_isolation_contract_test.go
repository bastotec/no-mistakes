package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
)

func TestTestStepIsolationRefusesUnsupportedAgentBeforeAnyCommand(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	marker := filepath.Join(dir, "must-not-run")
	ag := &mockAgent{name: "unsupported", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		t.Fatal("agent executed despite isolation refusal")
		return nil, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Test: "echo forbidden > must-not-run"})
	if err := sctx.DB.EnableTestIsolation(sctx.Run.ID); err != nil {
		t.Fatal(err)
	}
	_, err := (&TestStep{}).Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "test-isolation-adapter") {
		t.Fatalf("unsupported adapter did not refuse: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("command ran before refusal")
	}
}
func TestTestStepOrdinaryRunKeepsAmbientEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX environment assertion")
	}
	t.Setenv("UNRECOGNIZED_CREDENTIAL", "ordinary-sentinel")
	t.Setenv("NM_TEST_MODEL_URL", "")
	t.Setenv("NM_TEST_POSTGRES_ROOT", "")
	dir, base, head := setupGitRepo(t)
	ag := &mockAgent{name: "ordinary", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"ordinary","tested":["baseline"],"testing_summary":"baseline","artifacts":[],"scenarios":[{"name":"ordinary","result":"pass","live":true,"evidence":"fixture","reason":""}],"verdict":"go"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{Test: `test "$UNRECOGNIZED_CREDENTIAL" = ordinary-sentinel`})
	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil || outcome == nil || outcome.NeedsApproval || outcome.ExitCode != 0 {
		t.Fatalf("ordinary behavior changed: %+v %v", outcome, err)
	}
}
