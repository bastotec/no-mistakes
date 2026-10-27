package steps

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"gopkg.in/yaml.v3"
)

type stallTestHost struct {
	scm.Host
	stalls        []scm.CIStall
	executed      []scm.CIStall
	plan          []scm.CIStall
	planErr       error
	beforeRecover func([]scm.CIStall)
	allowed       bool
	calls         int
	err           error
}

func (h *stallTestHost) CIStalls(context.Context, []scm.Check, time.Time) (scm.CIStallInspection, error) {
	return scm.CIStallInspection{Stalls: h.stalls, Executed: h.executed}, nil
}
func (h *stallTestHost) PlanCIStallRecovery(_ context.Context, stall scm.CIStall, _ time.Time) ([]scm.CIStall, error) {
	if h.planErr != nil {
		return nil, h.planErr
	}
	if h.plan != nil {
		return h.plan, nil
	}
	return []scm.CIStall{stall}, nil
}
func (h *stallTestHost) WorkloadRecoveryAllowed(context.Context) (bool, error) { return h.allowed, nil }
func (h *stallTestHost) RecoverCIStall(_ context.Context, plan []scm.CIStall, _ time.Time) error {
	h.calls++
	if h.beforeRecover != nil {
		h.beforeRecover(plan)
	}
	return h.err
}

func TestCIStallRecoveryOncePersistsAcrossRestart(t *testing.T) {
	for _, class := range []string{"queued-no-runner", "provider-no-start", "running-past-timeout"} {
		t.Run(class, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
			now := time.Now()
			stall := scm.CIStall{Key: "ci/build", Class: class, HeadSHA: head, JobKey: "build", Check: scm.Check{Name: "build", WorkflowID: 1, Bucket: scm.CheckBucketPending}}
			host := &stallTestHost{stalls: []scm.CIStall{stall}}
			step := &CIStep{}
			waiting, out, err := step.recoverStalls(sctx, host, &scm.PR{}, []scm.Check{stall.Check}, now)
			if err != nil || !waiting || out != nil || host.calls != 1 {
				t.Fatalf("first recovery = %v %#v %v calls=%d", waiting, out, err, host.calls)
			}
			restored := &CIStep{}
			restored.loadRerunBudget(sctx)
			_, out, err = restored.recoverStalls(sctx, host, &scm.PR{}, []scm.Check{stall.Check}, now.Add(stallReplacementGrace+time.Second))
			if err != nil || out == nil || !out.NeedsApproval || host.calls != 1 {
				t.Fatalf("recovered budget = %#v %v calls=%d", out, err, host.calls)
			}
			if !strings.Contains(out.Findings, class) || !strings.Contains(out.Findings, "handed-off") || !strings.Contains(out.Findings, "build") {
				t.Fatal(out.Findings)
			}
		})
	}
}

func TestCIStallRecoveryDoesNotStarveIndependentWorkflows(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	now := time.Now()
	a := scm.CIStall{Key: "a/build", Class: "running-past-timeout", HeadSHA: head, Check: scm.Check{Name: "build", WorkflowID: 1}}
	b := scm.CIStall{Key: "b/test", Class: "running-past-timeout", HeadSHA: head, Check: scm.Check{Name: "test", WorkflowID: 2}}
	host := &stallTestHost{stalls: []scm.CIStall{a, b}}
	step := &CIStep{}
	for _, at := range []time.Time{now, now.Add(time.Minute), now.Add(2 * stallReplacementGrace)} {
		_, out, err := step.recoverStalls(sctx, host, &scm.PR{}, nil, at)
		if err != nil || host.calls != 2 {
			t.Fatalf("recovery calls=%d out=%#v err=%v", host.calls, out, err)
		}
		if at.Sub(now) > stallReplacementGrace {
			findings, err := types.ParseFindingsJSON(out.Findings)
			if err != nil || len(findings.Items) != 2 {
				t.Fatalf("handoff=%#v err=%v", out, err)
			}
		}
	}
}

func TestCIStallUnsafeCancellationDefersWithoutSpendingRetry(t *testing.T) {
	for _, boundary := range []string{"planning", "before-cancellation"} {
		t.Run(boundary, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
			now := time.Now()
			stall := scm.CIStall{Key: "ci/build", Class: "queued-no-runner", HeadSHA: head, JobKey: "build", Check: scm.Check{Name: "build"}}
			host := &stallTestHost{stalls: []scm.CIStall{stall}, allowed: true}
			if boundary == "planning" {
				host.planErr = scm.ErrCIRecoveryUnsafe
			} else {
				host.err = scm.ErrCIRecoveryUnsafe
			}
			step := &CIStep{}
			waiting, out, err := step.recoverStalls(sctx, host, &scm.PR{}, nil, now)
			if err != nil || !waiting || out != nil {
				t.Fatalf("unsafe recovery: waiting=%v out=%#v err=%v", waiting, out, err)
			}
			restored := &CIStep{}
			restored.loadRerunBudget(sctx)
			if state := restored.transientReruns.stalls[stall.Key]; state.Phase != "deferred" || state.RemedySpent {
				t.Fatalf("deferred retry spent authority: %#v", state)
			}
			host.err, host.planErr = nil, nil
			calls := host.calls
			waiting, out, err = restored.recoverStalls(sctx, host, &scm.PR{}, nil, now.Add(2*stallReplacementGrace))
			if err != nil || !waiting || out != nil || host.calls != calls+1 {
				t.Fatalf("settled retry: waiting=%v out=%#v err=%v calls=%d", waiting, out, err, host.calls)
			}
			_, _, err = restored.recoverStalls(sctx, host, &scm.PR{}, nil, now.Add(2*stallReplacementGrace+time.Second))
			if err != nil || host.calls != calls+1 {
				t.Fatalf("retry duplicated: err=%v calls=%d", err, host.calls)
			}
		})
	}
}

func TestCIStallPlanningMetadataFailureLeavesRetryUnspent(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	stall := scm.CIStall{Key: "ci/build", Class: "running-past-timeout", HeadSHA: head, Check: scm.Check{Name: "build"}}
	readErr := errors.New("provider metadata unavailable")
	host := &stallTestHost{stalls: []scm.CIStall{stall}, planErr: readErr}
	step := &CIStep{}
	_, out, err := step.recoverStalls(sctx, host, &scm.PR{}, nil, time.Now())
	if !errors.Is(err, readErr) || out != nil || host.calls != 0 || len(step.transientReruns.stalls) != 0 {
		t.Fatalf("metadata failure: out=%#v err=%v calls=%d state=%#v", out, err, host.calls, step.transientReruns.stalls)
	}
	host.planErr = nil
	_, _, err = step.recoverStalls(sctx, host, &scm.PR{}, nil, time.Now())
	if err != nil || host.calls != 1 {
		t.Fatalf("retry after metadata recovery: err=%v calls=%d", err, host.calls)
	}
}

func TestCIStallSettlementFailurePreservesRevalidation(t *testing.T) {
	for _, workload := range []bool{false, true} {
		t.Run(fmt.Sprintf("workload=%v", workload), func(t *testing.T) {
			f := newCIRepairFixture(t, true, writeCIFix)
			p := ".github/workflows/ci.yml"
			if err := os.MkdirAll(filepath.Join(f.dir, ".github/workflows"), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(f.dir, p), []byte("jobs:\n  build:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo test\n"), 0644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, f.dir, "add", p)
			gitCmd(t, f.dir, "commit", "-m", "add workflow")
			gitCmd(t, f.dir, "push", "origin", "feature")
			head := f.localHead(t)
			dbPath := filepath.Join(t.TempDir(), "settlement.db")
			database, err := db.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = database.Close() })
			repo, err := database.InsertRepo(f.dir, "https://github.com/test/repo", "main")
			if err != nil {
				t.Fatal(err)
			}
			run, err := database.InsertRun(repo.ID, "refs/heads/feature", head, f.sctx.Run.BaseSHA)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateRunReviewApprovedHeadSHA(run.ID, head); err != nil {
				t.Fatal(err)
			}
			run.ReviewApprovedHeadSHA = &head
			f.sctx.DB, f.sctx.Repo, f.sctx.Run = database, repo, run
			row, err := database.InsertStepResult(run.ID, types.StepCI)
			if err != nil {
				t.Fatal(err)
			}
			f.sctx.StepResultID = row.ID
			f.sctx.DeferredFindings = `{"findings":[{"id":"deferred","severity":"info","action":"no-op","description":"retain observation"}]}`
			f.sctx.MarkRunning = func() error {
				t.Fatal("unpublished repair resumed monitoring")
				return nil
			}
			step := &CIStep{}
			stall := scm.CIStall{Key: "ci/build", Class: "provider-no-start", HeadSHA: head, WorkflowPath: p, JobKey: "build", Check: scm.Check{Name: "build"}}
			state := stallRecoveryState{Stall: stall, Phase: "handed-off", At: time.Now().Add(-time.Hour)}
			if workload {
				state.Phase = "rerun"
			}
			if err := step.saveStall(f.sctx, stall.Key, state); err != nil {
				t.Fatal(err)
			}
			connection, err := sql.Open("sqlite", dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			_, err = connection.Exec(`CREATE TRIGGER fail_stall_settlement BEFORE UPDATE OF findings_json ON step_results
				WHEN (SELECT head_sha FROM runs WHERE id = NEW.run_id) <> '` + head + `'
				BEGIN SELECT RAISE(FAIL, 'injected stall settlement failure'); END`)
			if err != nil {
				t.Fatal(err)
			}
			var out *pipeline.StepOutcome
			if workload {
				_, out, err = step.recoverStalls(f.sctx, &stallTestHost{allowed: true, stalls: []scm.CIStall{stall}}, &scm.PR{HeadSHA: head}, nil, time.Now())
			} else {
				f.sctx.PreviousFindings, _ = types.MarshalFindingsJSON(Findings{Items: []Finding{stallFinding(stall.Key, state, types.ActionAskUser)}})
				out, err = step.repairFromFindings(f.sctx, &completionSnapshotHost{checks: []scm.Check{stall.Check}}, &scm.PR{Number: "42"})
			}
			if err != nil || out == nil || out.RestartFrom != types.StepReview || out.Findings != f.sctx.DeferredFindings {
				t.Fatalf("partial repair outcome=%#v err=%v", out, err)
			}
			stored, err := database.GetRun(run.ID)
			if err != nil || stored.HeadSHA == head || stored.HeadSHA != f.localHead(t) || stored.ReviewApprovedHeadSHA != nil || f.remoteHead(t) != head {
				t.Fatalf("repair authority: run=%#v err=%v", stored, err)
			}
			if !strings.Contains(f.log(), "injected stall settlement failure") {
				t.Fatalf("settlement did not fail: %s", f.log())
			}
		})
	}
}

func TestRewriteWorkloadRunnerOnlyChangesSelectedLine(t *testing.T) {
	original := "name: CI\non: push\njobs:\n  build:\n    runs-on: ubuntu-latest # keep comment\n    steps:\n      - run: go test ./...\n  other:\n    runs-on: windows-latest\n"
	got, err := rewriteWorkloadRunner([]byte(original), "build")
	want := strings.Replace(original, "runs-on: ubuntu-latest", "runs-on: [self-hosted, linux, workload]", 1)
	if err != nil || string(got) != want {
		t.Fatalf("rewrite %q %v, want %q", got, err, want)
	}
	var workflow struct {
		Jobs map[string]struct {
			RunsOn any `yaml:"runs-on"`
			Steps  []struct {
				Run string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(got, &workflow); err != nil {
		t.Fatal(err)
	}
	labels, ok := workflow.Jobs["build"].RunsOn.([]any)
	if !ok || len(labels) != 3 || labels[0] != "self-hosted" || labels[1] != "linux" || labels[2] != "workload" || len(workflow.Jobs["build"].Steps) != 1 || workflow.Jobs["build"].Steps[0].Run != "go test ./..." || workflow.Jobs["other"].RunsOn != "windows-latest" {
		t.Fatalf("workflow semantics changed: %#v", workflow)
	}
	for _, runner := range []string{"\n      - ubuntu-latest", "${{ matrix.os }}", "&runner ubuntu-latest", "{group: private}", "[self-hosted, linux, workload]", "|-\n      ubuntu-latest"} {
		data := strings.Replace(original, "ubuntu-latest", runner, 1)
		if _, err := rewriteWorkloadRunner([]byte(data), "build"); err == nil {
			t.Fatalf("accepted unsupported runner %q", runner)
		}
	}
	if _, err := rewriteWorkloadRunner([]byte(original), "missing"); err == nil {
		t.Fatal("accepted missing job")
	}
}

func TestRewriteWorkloadRunnerRejectsFlowMappings(t *testing.T) {
	for _, workflow := range []string{
		"jobs:\n  build: {runs-on: ubuntu-latest, steps: [{run: \"echo hello\"}]}\n",
		"jobs: {build: {runs-on: ubuntu-latest, steps: [{run: \"echo hello\"}]}}\n",
		"{jobs: {build: {runs-on: ubuntu-latest, steps: [{run: \"echo hello\"}]}}}\n",
	} {
		if result, err := rewriteWorkloadRunner([]byte(workflow), "build"); err == nil || result != nil {
			t.Fatalf("unsafe rewrite returned %q, %v", result, err)
		}
	}
}

func TestWorkloadRepairRejectsFlowMappingWithoutMutation(t *testing.T) {
	dir, base, _ := setupGitRepo(t)
	p := ".github/workflows/ci.yml"
	workflow := []byte("jobs:\n  build: {runs-on: ubuntu-latest, steps: [{run: \"echo hello\"}]}\n")
	if err := os.MkdirAll(filepath.Join(dir, ".github/workflows"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, p), workflow, 0644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", p)
	gitCmd(t, dir, "commit", "-m", "flow workflow")
	head := gitCmd(t, dir, "rev-parse", "HEAD")
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	step := &CIStep{}
	state := stallRecoveryState{Phase: "remedy-requested", Stall: scm.CIStall{WorkflowPath: p, JobKey: "build", HeadSHA: head}}
	if err := step.saveStall(sctx, "build", state); err != nil {
		t.Fatal(err)
	}
	repair, handled, err := step.workloadRepair(sctx, &stallTestHost{allowed: true}, &scm.PR{}, Findings{Items: []Finding{{ID: workloadFindingPrefix + "build"}}})
	if !handled || err == nil || repair.HeadAdvanced {
		t.Fatalf("repair=%#v handled=%v err=%v", repair, handled, err)
	}
	got, err := os.ReadFile(filepath.Join(dir, p))
	if err != nil || string(got) != string(workflow) || gitCmd(t, dir, "rev-parse", "HEAD") != head || gitStatusPorcelain(t, dir) != "" {
		t.Fatalf("refused workflow was mutated: %q, %v", got, err)
	}
}

func TestWorkloadRepairRefusesIneligibleRepositoryWithoutMutation(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	host := &stallTestHost{allowed: false}
	step := &CIStep{}
	state := stallRecoveryState{Phase: "remedy-requested", Stall: scm.CIStall{WorkflowPath: ".github/workflows/ci.yml", JobKey: "build", HeadSHA: head}}
	if err := step.saveStall(sctx, "build", state); err != nil {
		t.Fatal(err)
	}
	_, handled, err := step.workloadRepair(sctx, host, &scm.PR{}, Findings{Items: []Finding{{ID: workloadFindingPrefix + "build"}}})
	if !handled || err == nil {
		t.Fatalf("ineligible remedy handled=%v err=%v", handled, err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".github/workflows/ci.yml")); !os.IsNotExist(err) {
		t.Fatal("ineligible repair wrote workflow")
	}
	if gitStatusPorcelain(t, dir) != "" {
		t.Fatal("ineligible repair changed tree")
	}
}

func TestCIStallMandatoryWorkloadRepairWithZeroCIBudget(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	workflow := "name: CI\non: push\njobs:\n  build:\n    runs-on: ubuntu-latest\n    steps:\n      - run: go test ./...\n  other:\n    runs-on: windows-latest\n"
	p := ".github/workflows/ci.yml"
	if err := os.MkdirAll(filepath.Join(dir, ".github/workflows"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, p), []byte(workflow), 0644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", p)
	gitCmd(t, dir, "commit", "-m", "add workflow")
	head = gitCmd(t, dir, "rev-parse", "HEAD")
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	sctx.Config.CI.RevalidateRepairs = true
	host := &stallTestHost{allowed: true}
	step := &CIStep{}
	now := time.Now()
	sctx.Config.AutoFix.CI = 0
	stall := scm.CIStall{Key: "build", Class: "provider-no-start", WorkflowPath: p, JobKey: "build", HeadSHA: head, Check: scm.Check{Name: "build"}}
	host.stalls = []scm.CIStall{stall}
	state := stallRecoveryState{Phase: "rerun", At: now.Add(-2 * stallReplacementGrace), Stall: stall}
	if err := step.saveStall(sctx, "build", state); err != nil {
		t.Fatal(err)
	}
	_, out, err := step.recoverStalls(sctx, host, &scm.PR{}, nil, now)
	if err != nil || out == nil || out.RestartFrom != types.StepReview || sctx.Run.HeadSHA == head {
		t.Fatalf("repair=%#v err=%v", out, err)
	}
	got, err := os.ReadFile(filepath.Join(dir, p))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != strings.Replace(workflow, "runs-on: ubuntu-latest", "runs-on: [self-hosted, linux, workload]", 1) {
		t.Fatalf("workflow=%s", got)
	}
	if gitStatusPorcelain(t, dir) != "" {
		t.Fatal("repair not committed cleanly")
	}
	if got := gitCmd(t, dir, "diff", "--name-only", head, "HEAD"); got != p {
		t.Fatalf("repair changed %q", got)
	}
	if step.transientReruns.stalls["build"].Phase != "remedy" {
		t.Fatal("remedy not persisted")
	}
	host.stalls[0].HeadSHA = sctx.Run.HeadSHA
	resumed := now.Add(2 * stallReplacementGrace)
	_, out, err = step.recoverStalls(sctx, host, &scm.PR{}, nil, resumed)
	if err != nil || out != nil || host.calls != 0 {
		t.Fatalf("replacement grace=%#v err=%v calls=%d", out, err, host.calls)
	}
	_, out, err = step.recoverStalls(sctx, host, &scm.PR{}, nil, resumed.Add(stallReplacementGrace+time.Second))
	if err != nil || out == nil || !out.NeedsApproval || host.calls != 0 {
		t.Fatalf("handoff=%#v err=%v calls=%d", out, err, host.calls)
	}
}

func TestCIStallCancellationReservesEveryAffectedJobBeforeMutation(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	now := time.Now()
	a := scm.CIStall{Key: "ci/build", Class: "queued-no-runner", HeadSHA: head, Active: true, Check: scm.Check{Name: "build"}}
	b := a
	b.Key, b.Check.Name = "ci/test", "test"
	host := &stallTestHost{stalls: []scm.CIStall{a, b}, plan: []scm.CIStall{a, b}}
	host.beforeRecover = func(plan []scm.CIStall) {
		persisted, err := sctx.DB.GetRunCIRerunState(sctx.Run.ID)
		var budget persistedRerunBudget
		if err != nil || json.Unmarshal([]byte(persisted), &budget) != nil || len(budget.Stalls) != 2 || len(plan) != 2 {
			t.Fatalf("pre-mutation budget=%s plan=%#v err=%v", persisted, plan, err)
		}
		for _, state := range budget.Stalls {
			if state.Phase != "rerun-reserved" || len(state.Affected) != 2 {
				t.Fatalf("unreserved cancellation: %#v", state)
			}
		}
	}
	step := &CIStep{}
	if _, _, err := step.recoverStalls(sctx, host, &scm.PR{}, nil, now); err != nil || host.calls != 1 {
		t.Fatalf("recovery calls=%d err=%v", host.calls, err)
	}
	restored := &CIStep{}
	restored.loadRerunBudget(sctx)
	_, out, err := restored.recoverStalls(sctx, host, &scm.PR{}, nil, now.Add(2*stallReplacementGrace))
	if err != nil || out == nil || !out.NeedsApproval || host.calls != 1 {
		t.Fatalf("restart out=%#v err=%v calls=%d", out, err, host.calls)
	}
	findings, err := types.ParseFindingsJSON(out.Findings)
	if err != nil || len(findings.Items) != 2 {
		t.Fatalf("handoff findings=%#v err=%v", findings, err)
	}
	for _, f := range findings.Items {
		if !strings.Contains(f.Description, "affected jobs: build, test") {
			t.Fatalf("missing cancellation disclosure: %#v", f)
		}
	}
}

func TestCIStallCancellationRefusesPreviouslySpentSibling(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	a := scm.CIStall{Key: "ci/build", Class: "queued-no-runner", HeadSHA: head, Active: true, Check: scm.Check{Name: "build"}}
	b := a
	b.Key, b.Check.Name = "ci/test", "test"
	host := &stallTestHost{stalls: []scm.CIStall{a, b}, plan: []scm.CIStall{a, b}}
	step := &CIStep{}
	if err := step.saveStall(sctx, b.Key, stallRecoveryState{Stall: b, Phase: "handed-off"}); err != nil {
		t.Fatal(err)
	}
	_, out, err := step.recoverStalls(sctx, host, &scm.PR{}, nil, time.Now())
	if err != nil || out == nil || !out.NeedsApproval || host.calls != 0 {
		t.Fatalf("out=%#v err=%v calls=%d", out, err, host.calls)
	}
}

func TestCIStallReplacementFailureReachesOrdinaryClassification(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	now := time.Now()
	stall := scm.CIStall{Key: "ci/build", Class: "provider-no-start", HeadSHA: head, Check: scm.Check{Name: "build", WorkflowID: 1, Link: "old"}}
	step := &CIStep{}
	if err := step.saveStall(sctx, stall.Key, stallRecoveryState{Stall: stall, Phase: "rerun", At: now}); err != nil {
		t.Fatal(err)
	}
	replacement := stall
	replacement.Check.Link = "new"
	replacement.Check.State, replacement.Check.Bucket = "FAILURE", scm.CheckBucketFail
	host := &stallTestHost{executed: []scm.CIStall{replacement}}
	waiting, out, err := step.recoverStalls(sctx, host, &scm.PR{}, []scm.Check{replacement.Check}, now.Add(2*stallReplacementGrace))
	if err != nil || waiting || out != nil || step.transientReruns.stalls[stall.Key].Phase != "recovered" || host.calls != 0 {
		t.Fatalf("replacement waiting=%v out=%#v err=%v state=%#v", waiting, out, err, step.transientReruns.stalls)
	}
	if classifyCheckFailure(replacement.Check) != classGenuine {
		t.Fatal("code failure no longer classified as genuine")
	}
	restored := &CIStep{}
	restored.loadRerunBudget(sctx)
	host.stalls = []scm.CIStall{stall}
	host.executed = nil
	_, out, err = restored.recoverStalls(sctx, host, &scm.PR{}, nil, now.Add(3*stallReplacementGrace))
	if err != nil || out == nil || host.calls != 0 {
		t.Fatalf("reopened stall replenished allowance: out=%#v err=%v calls=%d", out, err, host.calls)
	}
}

func TestCIStallHandedOffFindingAllowsHumanRepair(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{"summary":"human-selected infrastructure repair","code_change_needed":false}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	stall := scm.CIStall{Key: "ci/build", Class: "running-past-timeout", HeadSHA: head, Check: scm.Check{Name: "build"}}
	step := &CIStep{}
	state := stallRecoveryState{Stall: stall, Phase: "handed-off"}
	if err := step.saveStall(sctx, stall.Key, state); err != nil {
		t.Fatal(err)
	}
	finding := stallFinding(stall.Key, state, types.ActionAskUser)
	finding.UserInstructions = "Repair the infrastructure"
	sctx.PreviousFindings, _ = types.MarshalFindingsJSON(Findings{Items: []Finding{finding}})
	host := &completionSnapshotHost{checks: []scm.Check{stall.Check}}
	out, err := step.repairFromFindings(sctx, host, &scm.PR{Number: "42"})
	if err != nil || out == nil || len(ag.calls) != 1 || step.transientReruns.stalls[stall.Key].Phase != "handed-off" {
		t.Fatalf("human repair out=%#v err=%v calls=%d", out, err, len(ag.calls))
	}
}

func TestCIStallPublishedHumanRepairMonitorsSelectedReplacement(t *testing.T) {
	for _, revalidate := range []bool{false, true} {
		t.Run(fmt.Sprint(revalidate), func(t *testing.T) {
			f := newCIRepairFixture(t, revalidate, writeCIFix)
			sctx := f.sctx
			now := time.Now()
			stall := scm.CIStall{Key: "ci/test", Class: "queued-no-runner", HeadSHA: f.headSHA, Check: scm.Check{Name: "test", WorkflowID: 1}}
			state := stallRecoveryState{Stall: stall, Phase: "handed-off", At: now.Add(-time.Hour), Detail: "automatic allowances exhausted"}
			step := &CIStep{}
			if err := step.saveStall(sctx, stall.Key, state); err != nil {
				t.Fatal(err)
			}
			other := state
			other.Stall.Key, other.Stall.Check.Name = "ci/other", "other"
			if err := step.saveStall(sctx, other.Stall.Key, other); err != nil {
				t.Fatal(err)
			}
			finding := stallFinding(stall.Key, state, types.ActionAskUser)
			sctx.PreviousFindings, _ = types.MarshalFindingsJSON(Findings{Items: []Finding{finding}})
			host := &completionSnapshotHost{checks: []scm.Check{stall.Check}}
			out, err := step.repairFromFindings(sctx, host, &scm.PR{Number: "42"})
			if err != nil || (revalidate && (out == nil || out.RestartFrom != types.StepReview)) || (!revalidate && out != nil) {
				t.Fatalf("repair=%#v err=%v", out, err)
			}
			if revalidate {
				gitCmd(t, f.dir, "push", "origin", "feature")
			}
			if f.remoteHead(t) != sctx.Run.HeadSHA || sctx.Run.HeadSHA == f.headSHA {
				t.Fatal("replacement head not published")
			}
			restored := &CIStep{}
			restored.loadRerunBudget(sctx)
			if restored.transientReruns.stalls[stall.Key].Phase != "human-repair" || restored.transientReruns.stalls[other.Stall.Key].Phase != "handed-off" {
				t.Fatalf("selected transition=%#v", restored.transientReruns.stalls)
			}
			pending := stall.Check
			pending.Bucket = scm.CheckBucketPending
			provider := &stallTestHost{allowed: true}
			resumed := now.Add(time.Hour)
			waiting, out, err := restored.recoverStalls(sctx, provider, &scm.PR{HeadSHA: sctx.Run.HeadSHA}, []scm.Check{pending}, resumed)
			if err != nil || !waiting || out != nil || provider.calls != 0 {
				t.Fatalf("replacement monitoring waiting=%v out=%#v err=%v", waiting, out, err)
			}
			provider.stalls = []scm.CIStall{stall}
			provider.stalls[0].HeadSHA = sctx.Run.HeadSHA
			_, out, err = restored.recoverStalls(sctx, provider, &scm.PR{HeadSHA: sctx.Run.HeadSHA}, []scm.Check{pending}, resumed.Add(stallReplacementGrace+time.Second))
			if err != nil || out == nil || !out.NeedsApproval || provider.calls != 0 || restored.transientReruns.stalls[stall.Key].Phase != "handed-off" || gitCmd(t, f.dir, "rev-parse", "HEAD") != sctx.Run.HeadSHA {
				t.Fatalf("spent allowances restored: out=%#v err=%v calls=%d", out, err, provider.calls)
			}
		})
	}
}

func TestCIStallRevalidationArmsGraceOnCurrentPublishedHead(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	now := time.Now()
	stall := scm.CIStall{Key: "ci/build", Class: "provider-no-start", HeadSHA: base, Check: scm.Check{Name: "build", WorkflowID: 1}}
	step := &CIStep{}
	if err := step.saveStall(sctx, stall.Key, stallRecoveryState{Stall: stall, Phase: "remedy", At: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	host := &stallTestHost{stalls: []scm.CIStall{stall}, allowed: true}
	pr := &scm.PR{HeadSHA: head}
	waiting, out, err := step.recoverStalls(sctx, host, pr, nil, now)
	if err != nil || !waiting || out != nil || host.calls != 0 {
		t.Fatalf("resumed waiting=%v out=%#v err=%v", waiting, out, err)
	}
	restored := &CIStep{}
	restored.loadRerunBudget(sctx)
	state := restored.transientReruns.stalls[stall.Key]
	if state.MonitorHead != head || !state.At.Equal(now) {
		t.Fatalf("monitoring grace not pinned: %#v", state)
	}
	waiting, out, err = restored.recoverStalls(sctx, host, pr, nil, now.Add(time.Minute))
	if err != nil || !waiting || out != nil {
		t.Fatalf("restart lost grace: %v %#v %v", waiting, out, err)
	}
	host.stalls = nil
	_, out, err = restored.recoverStalls(sctx, host, pr, nil, now.Add(stallReplacementGrace+time.Second))
	if err != nil || out == nil || !out.NeedsApproval || host.calls != 0 || restored.transientReruns.stalls[stall.Key].Stall.HeadSHA != base {
		t.Fatalf("registration failure=%#v err=%v calls=%d", out, err, host.calls)
	}
}

func TestCIStallRecoveredJobAfterOrdinaryRepairHandsOffNewHeadStall(t *testing.T) {
	f := newCIRepairFixture(t, false, writeCIFix)
	step := &CIStep{}
	now := time.Now()
	stall := scm.CIStall{Key: "ci/build", Class: "queued-no-runner", HeadSHA: f.headSHA, Check: scm.Check{Name: "build", WorkflowID: 1}}
	state := stallRecoveryState{Stall: stall, Phase: "remedy", MonitorHead: f.headSHA, At: now.Add(-time.Hour)}
	if err := step.saveStall(f.sctx, stall.Key, state); err != nil {
		t.Fatal(err)
	}
	provider := &stallTestHost{allowed: true}
	passed := stall.Check
	passed.Bucket = scm.CheckBucketPass
	if _, out, err := step.recoverStalls(f.sctx, provider, &scm.PR{HeadSHA: f.headSHA}, []scm.Check{passed}, now); err != nil || out != nil || step.transientReruns.stalls[stall.Key].Phase != "recovered" {
		t.Fatalf("replacement did not recover: out=%#v err=%v", out, err)
	}
	finding := Finding{ID: "ci-test", Category: types.FindingCategoryCICheck, Check: "test", Action: types.ActionAutoFix, Severity: types.FindingSeverityError}
	f.sctx.PreviousFindings, _ = types.MarshalFindingsJSON(Findings{Items: []Finding{finding}})
	if out, err := step.repairFromFindings(f.sctx, &completionSnapshotHost{}, &scm.PR{Number: "42"}); err != nil || out != nil {
		t.Fatalf("ordinary repair=%#v err=%v", out, err)
	}
	if f.remoteHead(t) == f.headSHA || f.remoteHead(t) != f.sctx.Run.HeadSHA {
		t.Fatal("ordinary repair was not published")
	}
	restored := &CIStep{}
	restored.loadRerunBudget(f.sctx)
	stall.HeadSHA = f.sctx.Run.HeadSHA
	stall.Check.Bucket = scm.CheckBucketPending
	provider.stalls = []scm.CIStall{stall}
	pr := &scm.PR{HeadSHA: stall.HeadSHA}
	resumed := now.Add(time.Hour)
	waiting, out, err := restored.recoverStalls(f.sctx, provider, pr, []scm.Check{stall.Check}, resumed)
	if err != nil || !waiting || out != nil || restored.transientReruns.stalls[stall.Key].MonitorHead != stall.HeadSHA {
		t.Fatalf("new-head monitoring=%v %#v %v", waiting, out, err)
	}
	_, out, err = restored.recoverStalls(f.sctx, provider, pr, []scm.Check{stall.Check}, resumed.Add(stallReplacementGrace+time.Second))
	if err != nil || out == nil || !out.NeedsApproval || provider.calls != 0 || f.localHead(t) != stall.HeadSHA {
		t.Fatalf("new-head handoff=%#v err=%v calls=%d", out, err, provider.calls)
	}
}

func TestCIStallRetainedProtectedRepairsMonitorAbsentReplacement(t *testing.T) {
	for _, workload := range []bool{false, true} {
		for _, revalidate := range []bool{false, true} {
			t.Run(fmt.Sprintf("workload=%t/revalidate=%t", workload, revalidate), func(t *testing.T) {
				f := newCIRepairFixture(t, revalidate, nil)
				p := ".github/workflows/ci.yml"
				if err := os.MkdirAll(filepath.Join(f.dir, ".github/workflows"), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(f.dir, p), []byte("name: CI\non: push\njobs:\n  build:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo test\n"), 0644); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, f.dir, "add", p)
				gitCmd(t, f.dir, "commit", "-m", "add workflow")
				head := f.localHead(t)
				if _, err := (&CIStep{}).recordRepair(f.sctx, head); err != nil {
					t.Fatal(err)
				}
				if revalidate {
					gitCmd(t, f.dir, "push", "origin", "feature")
				}
				f.sctx.Config.ProtectedPaths = []string{p}
				step := &CIStep{}
				now := time.Now()
				stall := scm.CIStall{Key: "ci/build", Class: "queued-no-runner", HeadSHA: head, WorkflowPath: p, JobKey: "build", Check: scm.Check{Name: "build", WorkflowID: 1}}
				phase := "handed-off"
				if workload {
					phase = "rerun"
				}
				state := stallRecoveryState{Stall: stall, Phase: phase, At: now.Add(-time.Hour)}
				if err := step.saveStall(f.sctx, stall.Key, state); err != nil {
					t.Fatal(err)
				}
				provider := &stallTestHost{allowed: true, stalls: []scm.CIStall{stall}}
				var out *pipeline.StepOutcome
				var err error
				if workload {
					_, out, err = step.recoverStalls(f.sctx, provider, &scm.PR{HeadSHA: head}, nil, now)
				} else {
					f.sctx.Agent = &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
						writeCIFix(f.dir)
						if err := os.WriteFile(filepath.Join(f.dir, p), []byte("protected edit\n"), 0644); err != nil {
							t.Fatal(err)
						}
						return &agent.Result{Output: json.RawMessage(`{"summary":"repair stalled check","code_change_needed":true}`)}, nil
					}}
					f.sctx.PreviousFindings, _ = types.MarshalFindingsJSON(Findings{Items: []Finding{stallFinding(stall.Key, state, types.ActionAskUser)}})
					out, err = step.repairFromFindings(f.sctx, &completionSnapshotHost{}, &scm.PR{Number: "42"})
				}
				if err != nil || out == nil || !pipeline.HasProtectedPathRefusal(out.Findings) || f.localHead(t) != head {
					t.Fatalf("protected repair=%#v err=%v", out, err)
				}
				persistCIRefusal(t, f, out)
				if workload {
					f.sctx.Config.ProtectedPaths = nil
				} else {
					gitCmd(t, f.dir, "restore", "--", p)
				}
				refusal, err := types.ParseFindingsJSON(out.Findings)
				if err != nil {
					t.Fatal(err)
				}
				selected := Findings{}
				for _, item := range refusal.Items {
					if item.ID == "protected-path-refusal" {
						selected.Items = append(selected.Items, item)
					}
				}
				if len(selected.Items) != 1 {
					t.Fatalf("missing retained-repair refusal: %#v", refusal)
				}
				f.sctx.PreviousFindings, _ = types.MarshalFindingsJSON(selected)
				retry := &CIStep{}
				retry.loadRerunBudget(f.sctx)
				repair, err := retry.retryProtectedPathRepair(f.sctx)
				if err != nil || !repair.HeadAdvanced || repair.Revalidate != revalidate {
					t.Fatalf("retained retry=%#v err=%v", repair, err)
				}
				if revalidate {
					gitCmd(t, f.dir, "push", "origin", "feature")
				}
				if f.remoteHead(t) != f.sctx.Run.HeadSHA || f.remoteHead(t) == head {
					t.Fatal("retained repair was not published")
				}
				restored := &CIStep{}
				restored.loadRerunBudget(f.sctx)
				provider.stalls = nil
				resumed := now.Add(time.Hour)
				pr := &scm.PR{HeadSHA: f.sctx.Run.HeadSHA}
				waiting, out, err := restored.recoverStalls(f.sctx, provider, pr, nil, resumed)
				state = restored.transientReruns.stalls[stall.Key]
				if err != nil || !waiting || out != nil || state.MonitorHead != pr.HeadSHA || !state.At.Equal(resumed) || !state.RemedySpent {
					t.Fatalf("replacement grace=%v %#v err=%v state=%#v", waiting, out, err, state)
				}
				_, out, err = restored.recoverStalls(f.sctx, provider, pr, nil, resumed.Add(stallReplacementGrace+time.Second))
				if err != nil || out == nil || !out.NeedsApproval || provider.calls != 0 || f.localHead(t) != pr.HeadSHA {
					t.Fatalf("replacement handoff=%#v err=%v calls=%d", out, err, provider.calls)
				}
			})
		}
	}
}
