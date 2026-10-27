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

func updateStallMonitorInputs(t *testing.T, sctx *pipeline.StepContext, fixture map[string]any, checks string) {
	t.Helper()
	data, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range sctx.Env {
		name, path, _ := strings.Cut(entry, "=")
		var content []byte
		switch name {
		case "FAKE_CLI_STALL_API":
			content = data
		case "FAKE_CLI_CHECKS_PATH":
			content = []byte(checks)
		default:
			continue
		}
		if err := os.WriteFile(path, content, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCIStep_StallRetriesOnceAfterHealthySiblingCompletes(t *testing.T) {
	t.Parallel()
	checks := `[{"name":"build","state":"TIMED_OUT","bucket":"fail","link":"https://github.com/test/repo/actions/runs/900/job/901"},{"name":"test","state":"IN_PROGRESS","bucket":"pending","link":"https://github.com/test/repo/actions/runs/900/job/902"}]`
	now := time.Now()
	jobs := []map[string]any{
		{"id": 901, "name": "build", "status": "completed", "conclusion": "timed_out", "steps": []any{map[string]any{"number": 2, "status": "completed", "conclusion": "failure"}}},
		{"id": 902, "name": "test", "status": "in_progress", "runner_id": 42, "started_at": now.Add(-time.Minute)},
	}
	fixture := map[string]any{
		"repos/test/repo/actions/runs/900/jobs?per_page=100&page=1": map[string]any{"jobs": jobs},
	}
	sctx, logFile := stallMonitorContext(t, checks, fixture)
	fixture["repos/test/repo/actions/runs/900"] = map[string]any{"head_sha": sctx.Run.HeadSHA, "path": ".github/workflows/ci.yml", "status": "in_progress"}
	fixture["repos/test/repo/contents/.github/workflows/ci.yml?ref="+sctx.Run.HeadSHA] = map[string]string{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte("jobs:\n  build: {}\n  test:\n    timeout-minutes: 30\n"))}
	updateStallMonitorInputs(t, sctx, fixture, checks)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx
	polls := 0
	step := (&steps.CIStep{}).SetNow(func() time.Time { return now }).SetWaitForNextPoll(func(context.Context, time.Duration) error {
		polls++
		if polls == 1 {
			if strings.Contains(ghLog(t, logFile), "run rerun") || strings.Contains(ghLog(t, logFile), "run cancel") {
				t.Fatal("healthy sibling allowed a mutation")
			}
			persisted, err := sctx.DB.GetRunCIRerunState(sctx.Run.ID)
			var budget struct {
				Stalls map[string]struct{ Phase string }
			}
			if err != nil || json.Unmarshal([]byte(persisted), &budget) != nil || budget.Stalls[".github/workflows/ci.yml/build"].Phase != "deferred" {
				t.Fatalf("retry was not durably deferred: %s err=%v", persisted, err)
			}
			jobs[1]["status"], jobs[1]["conclusion"] = "completed", "success"
			fixture["repos/test/repo/actions/runs/900"].(map[string]any)["status"] = "completed"
			checks = strings.ReplaceAll(checks, `"state":"IN_PROGRESS","bucket":"pending"`, `"state":"SUCCESS","bucket":"pass"`)
			updateStallMonitorInputs(t, sctx, fixture, checks)
			now = now.Add(11 * time.Minute)
		} else {
			now = now.Add(time.Minute)
		}
		if polls == 3 {
			cancel()
			return ctx.Err()
		}
		return nil
	})
	if out, err := step.Execute(sctx); !errors.Is(err, context.Canceled) || out != nil || polls != 3 {
		t.Fatalf("monitor out=%#v err=%v polls=%d", out, err, polls)
	}
	log := ghLog(t, logFile)
	if strings.Count(log, "run rerun --job 901") != 1 || strings.Contains(log, "run cancel") || strings.Contains(log, "run rerun --job 902") {
		t.Fatalf("unsafe or duplicated retry: %s", log)
	}
}

func TestCIStep_AssignedRunnerSetupFailureRetiresNoStartDiagnosis(t *testing.T) {
	t.Parallel()
	checks := `[{"name":"build","state":"FAILURE","bucket":"fail","link":"https://github.com/test/repo/actions/runs/900/job/901"}]`
	fixture := map[string]any{
		"repos/test/repo/actions/runs/900/jobs?per_page=100&page=1": map[string]any{"jobs": []any{map[string]any{"id": 901, "name": "build", "status": "completed", "conclusion": "failure", "check_run_url": "https://api.github.com/repos/test/repo/check-runs/903"}}},
		"repos/test/repo/check-runs/903/annotations?per_page=100":   []any{map[string]string{"message": "recent account payments have failed"}},
	}
	sctx, logFile := stallMonitorContext(t, checks, fixture)
	fixture["repos/test/repo/contents/.github/workflows/ci.yml?ref="+sctx.Run.HeadSHA] = map[string]string{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte("jobs:\n  build:\n    runs-on: ubuntu-latest\n"))}
	updateStallMonitorInputs(t, sctx, fixture, checks)
	head := sctx.Run.HeadSHA
	now := time.Now()
	polls := 0
	step := (&steps.CIStep{}).SetNow(func() time.Time { return now }).SetWaitForNextPoll(func(context.Context, time.Duration) error {
		polls++
		if polls > 1 {
			return errors.New("setup failure did not reach ordinary classification")
		}
		fixture["repos/test/repo/actions/runs/900/jobs?per_page=100&page=1"] = map[string]any{"jobs": []any{map[string]any{"id": 904, "name": "build", "status": "completed", "conclusion": "failure", "runner_id": 42, "steps": []any{map[string]any{"number": 1, "status": "completed", "conclusion": "failure"}}, "html_url": "https://github.com/test/repo/actions/runs/900/job/904"}}}
		checks = strings.ReplaceAll(checks, "job/901", "job/904")
		updateStallMonitorInputs(t, sctx, fixture, checks)
		now = now.Add(11 * time.Minute)
		return nil
	})
	out, err := step.Execute(sctx)
	if err != nil || out == nil || !out.AutoFixable || polls != 1 {
		t.Fatalf("replacement failure out=%#v err=%v polls=%d", out, err, polls)
	}
	findings, err := types.ParseFindingsJSON(out.Findings)
	if err != nil || len(findings.Items) != 2 || findings.Items[0].Action != types.ActionAutoFix || !strings.Contains(findings.Items[0].Description, "job/904") || findings.Items[1].Action != types.ActionNoOp {
		t.Fatalf("ordinary failure findings=%#v err=%v", findings, err)
	}
	persisted, err := sctx.DB.GetRunCIRerunState(sctx.Run.ID)
	var budget struct {
		Stalls map[string]struct{ Phase string }
	}
	if err != nil || json.Unmarshal([]byte(persisted), &budget) != nil || budget.Stalls[".github/workflows/ci.yml/build"].Phase != "recovered" || sctx.Run.HeadSHA != head {
		t.Fatalf("stale no-start diagnosis: %s head=%s err=%v", persisted, sctx.Run.HeadSHA, err)
	}
	if strings.Count(ghLog(t, logFile), "run rerun --job 901") != 1 || strings.Contains(ghLog(t, logFile), "run rerun --job 904") {
		t.Fatalf("spent retry replenished: %s", ghLog(t, logFile))
	}
	if _, err := os.Stat(filepath.Join(sctx.WorkDir, ".github/workflows/ci.yml")); !os.IsNotExist(err) {
		t.Fatalf("setup failure attempted a workload rewrite: %v", err)
	}
}

func TestCIStep_StallInspectionFailureUsesBoundedReadBudget(t *testing.T) {
	t.Parallel()
	for _, endpoint := range []string{"jobs", "annotations", "planning"} {
		t.Run(endpoint, func(t *testing.T) {
			fixture := map[string]any{
				"repos/test/repo/actions/runs/900/jobs?per_page=100&page=1": map[string]any{"jobs": []any{map[string]any{"id": 901, "name": "build", "status": "completed", "conclusion": "failure", "check_run_url": "https://api.github.com/repos/test/repo/check-runs/903"}}},
				"repos/test/repo/check-runs/903/annotations?per_page=100":   "provider unavailable",
			}
			if endpoint == "jobs" {
				fixture["repos/test/repo/actions/runs/900/jobs?per_page=100&page=1"] = "provider unavailable"
			}
			sctx, _ := stallMonitorContext(t, `[{"name":"build","state":"FAILURE","bucket":"fail","link":"https://github.com/test/repo/actions/runs/900/job/901"}]`, fixture)
			if endpoint == "planning" {
				fixture["repos/test/repo/actions/runs/900"].(map[string]any)["status"] = "in_progress"
				fixture["repos/test/repo/actions/runs/900/jobs?per_page=100&page=1"] = map[string]any{"jobs": []any{map[string]any{"id": 901, "name": "build", "status": "completed", "conclusion": "timed_out", "steps": []any{map[string]any{"number": 1, "status": "completed", "conclusion": "failure"}}}}}
				fixture["repos/test/repo/contents/.github/workflows/ci.yml?ref="+sctx.Run.HeadSHA] = "provider unavailable"
				updateStallMonitorInputs(t, sctx, fixture, `[{"name":"build","state":"TIMED_OUT","bucket":"fail","link":"https://github.com/test/repo/actions/runs/900/job/901"}]`)
			}
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
