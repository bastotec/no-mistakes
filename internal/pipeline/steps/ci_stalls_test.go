package steps

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type stallTestHost struct {
	scm.Host
	stalls  []scm.CIStall
	allowed bool
	calls   int
	err     error
}

func (h *stallTestHost) CIStalls(context.Context, []scm.Check, time.Time) ([]scm.CIStall, error) {
	return h.stalls, nil
}
func (h *stallTestHost) WorkloadRecoveryAllowed(context.Context) (bool, error) { return h.allowed, nil }
func (h *stallTestHost) RecoverCIStall(context.Context, scm.CIStall, time.Time) ([]string, error) {
	h.calls++
	return []string{"build"}, h.err
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
			waiting, out, err := step.recoverStalls(sctx, host, []scm.Check{stall.Check}, now)
			if err != nil || !waiting || out != nil || host.calls != 1 {
				t.Fatalf("first recovery = %v %#v %v calls=%d", waiting, out, err, host.calls)
			}
			restored := &CIStep{}
			restored.loadRerunBudget(sctx)
			_, out, err = restored.recoverStalls(sctx, host, []scm.Check{stall.Check}, now.Add(stallReplacementGrace+time.Second))
			if err != nil || out == nil || !out.NeedsApproval || host.calls != 1 {
				t.Fatalf("recovered budget = %#v %v calls=%d", out, err, host.calls)
			}
			if !strings.Contains(out.Findings, class) || !strings.Contains(out.Findings, "handed-off") || !strings.Contains(out.Findings, "build") {
				t.Fatal(out.Findings)
			}
		})
	}
}

func TestCIStallWorkloadRemedyThenHandoff(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	now := time.Now()
	stall := scm.CIStall{Key: "ci/build", Class: "provider-no-start", HeadSHA: head, JobKey: "build", Check: scm.Check{Name: "build"}}
	host := &stallTestHost{stalls: []scm.CIStall{stall}, allowed: true}
	step := &CIStep{}
	if _, _, err := step.recoverStalls(sctx, host, nil, now); err != nil {
		t.Fatal(err)
	}
	_, out, err := step.recoverStalls(sctx, host, nil, now.Add(stallReplacementGrace+time.Second))
	if err != nil || out == nil {
		t.Fatalf("remedy: %#v %v", out, err)
	}
	findings, err := types.ParseFindingsJSON(out.Findings)
	if err != nil || len(findings.Items) != 1 || findings.Items[0].Action != types.ActionAutoFix {
		t.Fatalf("remedy findings: %#v %v", findings, err)
	}
	// A published workload repair consumes the last automatic remedy. Its own
	// no-start is a handoff, never another rerun or another workflow edit.
	state := step.transientReruns.stalls[stall.Key]
	state.Phase = "remedy"
	state.At = now
	if err := step.saveStall(sctx, stall.Key, state); err != nil {
		t.Fatal(err)
	}
	_, out, err = step.recoverStalls(sctx, host, nil, now.Add(2*stallReplacementGrace))
	if err != nil || out == nil || !out.NeedsApproval || host.calls != 1 || !strings.Contains(out.Findings, "handed-off") {
		t.Fatalf("handoff: %#v %v calls=%d", out, err, host.calls)
	}
}

func TestCIStallUnsafeCancellationHandsOffWithoutRemedy(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	now := time.Now()
	stall := scm.CIStall{Key: "ci/build", Class: "queued-no-runner", HeadSHA: head, JobKey: "build", Check: scm.Check{Name: "build"}}
	host := &stallTestHost{stalls: []scm.CIStall{stall}, allowed: true, err: scm.ErrCIRecoveryUnsafe}
	step := &CIStep{}
	if _, _, err := step.recoverStalls(sctx, host, nil, now); err != nil {
		t.Fatal(err)
	}
	_, out, err := step.recoverStalls(sctx, host, nil, now.Add(time.Second))
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

func TestWorkloadRepairCommitsOnlyRunnerChangeAndRevalidates(t *testing.T) {
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
	state := stallRecoveryState{Phase: "remedy-requested", Stall: scm.CIStall{WorkflowPath: p, JobKey: "build", HeadSHA: head}}
	if err := step.saveStall(sctx, "build", state); err != nil {
		t.Fatal(err)
	}
	result, handled, err := step.workloadRepair(sctx, host, &scm.PR{}, Findings{Items: []Finding{{ID: workloadFindingPrefix + "build"}}})
	if err != nil || !handled || !result.HeadAdvanced || !result.Revalidate {
		t.Fatalf("repair=%#v handled=%v err=%v", result, handled, err)
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
}
