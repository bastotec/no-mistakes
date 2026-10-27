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
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
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

func stallMonitorContext(t *testing.T, checks string, fixture map[string]any) (*pipeline.StepContext, string) {
	t.Helper()
	dir, upstream, base, head := setupCIRerunRepo(t)
	env, logFile := stepstest.FakeCIGHLoggedSequence(t, "OPEN", []string{checks}, "", "")
	fixture["repos/test/repo/actions/runs/900"] = map[string]any{"id": 900, "head_sha": head, "status": "completed", "path": ".github/workflows/ci.yml"}
	data, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	fixturePath := filepath.Join(t.TempDir(), "api.json")
	if err := os.WriteFile(fixturePath, data, 0600); err != nil {
		t.Fatal(err)
	}
	sctx := stepstest.NewTestContextWithDBRecords(t, &stepstest.MockAgent{AgentName: "test"}, dir, base, head, config.Commands{})
	sctx.Env = append(env, "FAKE_CLI_STALL_API="+fixturePath)
	sctx.Repo.UpstreamURL = upstream
	prURL := "https://github.com/test/repo/pull/42"
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = -1
	return sctx, logFile
}

func TestCIStep_StallInspectionFailureUsesBoundedReadBudget(t *testing.T) {
	t.Parallel()
	for _, endpoint := range []string{"jobs", "annotations"} {
		t.Run(endpoint, func(t *testing.T) {
			fixture := map[string]any{
				"repos/test/repo/actions/runs/900/jobs?per_page=100&page=1": map[string]any{"jobs": []any{map[string]any{"id": 901, "name": "build", "status": "completed", "conclusion": "failure", "check_run_url": "https://api.github.com/repos/test/repo/check-runs/903"}}},
				"repos/test/repo/check-runs/903/annotations?per_page=100":   "provider unavailable",
			}
			if endpoint == "jobs" {
				fixture["repos/test/repo/actions/runs/900/jobs?per_page=100&page=1"] = "provider unavailable"
			}
			sctx, _ := stallMonitorContext(t, `[{"name":"build","state":"FAILURE","bucket":"fail","link":"https://github.com/test/repo/actions/runs/900/job/901"}]`, fixture)
			sctx.DeferredFindings = `{"findings":[{"id":"deferred","severity":"warning","description":"retained decision","action":"ask-user"}]}`
			polls := 0
			step := (&steps.CIStep{}).SetWaitForNextPoll(func(context.Context, time.Duration) error {
				polls++
				if polls > steps.ConsecutiveCheckErrorLimit() {
					return errors.New("inspection errors were not bounded")
				}
				return nil
			})
			out, err := step.Execute(sctx)
			if err != nil || out == nil || !out.NeedsApproval || out.AutoFixable || polls != steps.ConsecutiveCheckErrorLimit()-1 {
				t.Fatalf("out=%#v err=%v polls=%d", out, err, polls)
			}
			findings, err := types.ParseFindingsJSON(out.Findings)
			if err != nil || len(findings.Items) != 2 || findings.Items[0].Action != types.ActionAskUser || findings.Items[1].ID != "deferred" || !strings.Contains(findings.Items[0].Description, "decode CI recovery metadata") {
				t.Fatalf("read failure findings=%#v err=%v", findings, err)
			}
		})
	}
}

func TestCIStep_StallHandoffPreservesSettledFailuresAndApproval(t *testing.T) {
	t.Parallel()
	checks := `[{"name":"build","state":"TIMED_OUT","bucket":"fail","link":"https://github.com/test/repo/actions/runs/900/job/901"},{"name":"tests","state":"FAILURE","bucket":"fail","link":"https://ci.example/tests/902"},{"name":"approval","state":"ACTION_REQUIRED","bucket":"awaiting_approval"}]`
	fixture := map[string]any{
		"repos/test/repo/actions/runs/900/jobs?per_page=100&page=1": map[string]any{"jobs": []any{map[string]any{"id": 901, "name": "build", "status": "completed", "conclusion": "timed_out", "steps": []any{map[string]any{"number": 2, "status": "completed", "conclusion": "failure"}}}}},
	}
	sctx, logFile := stallMonitorContext(t, checks, fixture)
	now := time.Now()
	polls := 0
	step := (&steps.CIStep{}).SetNow(func() time.Time { return now }).SetWaitForNextPoll(func(context.Context, time.Duration) error {
		polls++
		now = now.Add(11 * time.Minute)
		if polls > 2 {
			return errors.New("settled failures were not reported")
		}
		return nil
	})
	out, err := step.Execute(sctx)
	if err != nil || out == nil || !out.NeedsApproval || !out.AutoFixable {
		t.Fatalf("out=%#v err=%v polls=%d", out, err, polls)
	}
	findings, err := types.ParseFindingsJSON(out.Findings)
	if err != nil || len(findings.Items) != 3 {
		t.Fatalf("findings=%#v err=%v", findings, err)
	}
	var failure, approval, stall bool
	for _, f := range findings.Items {
		switch {
		case f.Check == "tests":
			failure = f.Action == types.ActionAutoFix && strings.Contains(f.Description, "https://ci.example/tests/902")
		case f.Category == types.FindingCategoryCIApproval:
			approval = f.Action == types.ActionAskUser
		case f.Check == "build":
			stall = f.Action == types.ActionAskUser && strings.Contains(f.Description, "handed-off")
		}
	}
	if !failure || !approval || !stall || strings.Count(ghLog(t, logFile), "run rerun --job 901") != 1 {
		t.Fatalf("mixed findings=%#v log=%s", findings.Items, ghLog(t, logFile))
	}
}
