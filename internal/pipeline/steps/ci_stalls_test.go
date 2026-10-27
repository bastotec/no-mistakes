package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type stallTestHost struct {
	scm.Host
	stalls        []scm.CIStall
	executed      []scm.CIStall
	plan          []scm.CIStall
	beforeRecover func([]scm.CIStall)
	allowed       bool
	calls         int
	err           error
}

func (h *stallTestHost) CIStalls(context.Context, []scm.Check, time.Time) (scm.CIStallInspection, error) {
	return scm.CIStallInspection{Stalls: h.stalls, Executed: h.executed}, nil
}
func (h *stallTestHost) PlanCIStallRecovery(_ context.Context, stall scm.CIStall, _ time.Time) ([]scm.CIStall, error) {
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

func TestCIStallUnsafeCancellationHandsOffWithoutRemedy(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	now := time.Now()
	stall := scm.CIStall{Key: "ci/build", Class: "queued-no-runner", HeadSHA: head, JobKey: "build", Check: scm.Check{Name: "build"}}
	host := &stallTestHost{stalls: []scm.CIStall{stall}, allowed: true, err: scm.ErrCIRecoveryUnsafe}
	step := &CIStep{}
	if _, _, err := step.recoverStalls(sctx, host, &scm.PR{}, nil, now); err != nil {
		t.Fatal(err)
	}
	_, out, err := step.recoverStalls(sctx, host, &scm.PR{}, nil, now.Add(time.Second))
	if err != nil || out == nil || !out.NeedsApproval || !strings.Contains(out.Findings, "non-stalled") {
		t.Fatalf("unsafe: %#v %v", out, err)
	}
}

func TestRewriteWorkloadRunnerOnlyChangesSelectedLine(t *testing.T) {
	original := "name: CI\non: push\njobs:\n  build:\n    runs-on: ubuntu-latest # keep comment\n    steps:\n      - run: go test ./...\n  other:\n    runs-on: windows-latest\n"
	got, err := rewriteWorkloadRunner([]byte(original), "build")
	want := strings.Replace(original, "runs-on: ubuntu-latest", "runs-on: [self-hosted, linux, workload]", 1)
	if err != nil || string(got) != want {
		t.Fatalf("rewrite %q %v, want %q", got, err, want)
	}
	for _, runner := range []string{"\n      - ubuntu-latest", "${{ matrix.os }}", "&runner ubuntu-latest", "{group: private}", "[self-hosted, linux, workload]"} {
		data := strings.Replace(original, "ubuntu-latest", runner, 1)
		if _, err := rewriteWorkloadRunner([]byte(data), "build"); err == nil {
			t.Fatalf("accepted unsupported runner %q", runner)
		}
	}
	if _, err := rewriteWorkloadRunner([]byte(original), "missing"); err == nil {
		t.Fatal("accepted missing job")
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
	_, out, err = step.recoverStalls(sctx, host, &scm.PR{}, nil, now.Add(2*stallReplacementGrace))
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
