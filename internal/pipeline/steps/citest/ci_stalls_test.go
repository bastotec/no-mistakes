package citest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCIStep_ProviderNoStartRecoveryRestoresReadiness(t *testing.T) {
	t.Parallel()
	dir, upstream, base, head := setupCIRerunRepo(t)
	env, logFile := stepstest.FakeCIGHLoggedSequence(t, "OPEN", []string{
		`[{"name":"build","state":"SUCCESS","bucket":"pass","link":"https://github.com/test/repo/actions/runs/900/job/900"}]`,
		`[{"name":"build","state":"FAILURE","bucket":"fail","link":"https://github.com/test/repo/actions/runs/900/job/901"}]`,
		`[{"name":"build","state":"SUCCESS","bucket":"pass","link":"https://github.com/test/repo/actions/runs/900/job/902"}]`,
	}, "", "")
	fixture := map[string]any{
		"repos/test/repo/actions/runs/900":                              map[string]any{"id": 900, "head_sha": head, "status": "completed", "path": ".github/workflows/ci.yml"},
		"repos/test/repo/actions/runs/900/jobs?per_page=100&page=1":     map[string]any{"jobs": []any{map[string]any{"id": 901, "name": "build", "status": "completed", "conclusion": "failure", "runner_id": 0, "steps": []any{}, "check_run_url": "https://api.github.com/repos/test/repo/check-runs/903", "html_url": "https://github.com/test/repo/actions/runs/900/job/901"}}},
		"repos/test/repo/check-runs/903/annotations?per_page=100":       []any{map[string]string{"message": "The job was not started because recent account payments have failed or your spending limit needs to be increased."}},
		"repos/test/repo/contents/.github/workflows/ci.yml?ref=" + head: map[string]string{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte("jobs:\n  build:\n    runs-on: ubuntu-latest\n"))},
	}
	data, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	fixturePath := filepath.Join(t.TempDir(), "api.json")
	if err := os.WriteFile(fixturePath, data, 0600); err != nil {
		t.Fatal(err)
	}
	env = append(env, "FAKE_CLI_STALL_API="+fixturePath)
	sctx := stepstest.NewTestContextWithDBRecords(t, &stepstest.MockAgent{AgentName: "test"}, dir, base, head, config.Commands{})
	sctx.Env = env
	sctx.Repo.UpstreamURL = upstream
	prURL := "https://github.com/test/repo/pull/42"
	sctx.Run.PRURL = &prURL
	row, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID = row.ID
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx
	polls := 0
	step := (&steps.CIStep{}).SetWaitForNextPoll(func(ctx context.Context, _ time.Duration) error {
		polls++
		run, err := sctx.DB.GetRun(sctx.Run.ID)
		if err != nil {
			t.Fatal(err)
		}
		wantReady := polls != 2
		if (run.CIReadyAt != nil) != wantReady {
			t.Fatalf("poll %d readiness=%v, want %v", polls, run.CIReadyAt, wantReady)
		}
		if polls == 3 {
			cancel()
			return ctx.Err()
		}
		return nil
	})
	if _, err := step.Execute(sctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("monitor=%v", err)
	}
	if got := strings.Count(ghLog(t, logFile), "run rerun --job 901"); got != 1 {
		t.Fatalf("targeted reruns=%d log=%s", got, ghLog(t, logFile))
	}
	persisted, err := sctx.DB.GetRunCIRerunState(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(persisted, `"phase":"recovered"`) {
		t.Fatalf("budget=%s", persisted)
	}
	result, err := sctx.DB.GetStepResult(row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.FindingsJSON == nil || !strings.Contains(*result.FindingsJSON, "provider-no-start") || !strings.Contains(*result.FindingsJSON, "recovered") {
		t.Fatalf("findings=%#v", result.FindingsJSON)
	}
}
