package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The 2026-10-24 incident: a live check failed, the CI fixer concluded no
// code change was needed and parked the CI step with the failing check
// re-labelled ask-user, and a driver loop's --yes approved it, ending the run
// passed-with-override without any human decision.
const redCIAskUserFindings = `{"summary":"The failing check is unrelated to this PR.","findings":[{"id":"ci-1","severity":"error","description":"CI check failing: Behavior portable serial 8 - provider reported fail","action":"ask-user","category":"ci-check","check":"Behavior portable serial 8"}]}`

const redCIAutoFixFindings = `{"findings":[{"id":"ci-1","severity":"error","description":"CI check failing: Behavior portable serial 8","action":"auto-fix","category":"ci-check","check":"Behavior portable serial 8"}]}`

func redCIRun(status types.StepStatus, findings string) *ipc.RunInfo {
	return &ipc.RunInfo{
		ID: "run-1", Status: types.RunRunning,
		Steps: []ipc.StepResultInfo{
			{StepName: types.StepPR, Status: types.StepStatusCompleted},
			{StepName: types.StepCI, Status: status, FindingsJSON: &findings},
		},
	}
}

func recordingRespondClient(t *testing.T) (*ipc.Client, func() []ipc.RespondParams) {
	t.Helper()
	socketPath := makeSocketSafeTempDir(t) + "/red-ci.sock"
	srv := ipc.NewServer()
	var mu sync.Mutex
	var calls []ipc.RespondParams
	srv.Handle(ipc.MethodRespond, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
		var params ipc.RespondParams
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
		mu.Lock()
		calls = append(calls, params)
		mu.Unlock()
		return &ipc.RespondResult{OK: true}, nil
	})
	startIPCServer(t, srv, socketPath)
	client := dialReady(t, socketPath)
	t.Cleanup(func() { client.Close() })
	return client, func() []ipc.RespondParams {
		mu.Lock()
		defer mu.Unlock()
		return append([]ipc.RespondParams(nil), calls...)
	}
}

func TestDriveRun_YesNeverApprovesOrSkipsRedCIGate(t *testing.T) {
	tests := []struct {
		name        string
		runs        []*ipc.RunInfo
		wantActions []types.ApprovalAction
	}{
		{
			name: "incident: ask-user failing check is fixed once, then left parked",
			runs: []*ipc.RunInfo{
				redCIRun(types.StepStatusAwaitingApproval, redCIAskUserFindings),
				redCIRun(types.StepStatusFixing, redCIAskUserFindings),
				redCIRun(types.StepStatusAwaitingApproval, redCIAskUserFindings),
			},
			wantActions: []types.ApprovalAction{types.ActionFix},
		},
		{
			name: "auto-fix failing check is fixed once, then left parked",
			runs: []*ipc.RunInfo{
				redCIRun(types.StepStatusAwaitingApproval, redCIAutoFixFindings),
				redCIRun(types.StepStatusRunning, redCIAutoFixFindings),
				redCIRun(types.StepStatusAwaitingApproval, redCIAutoFixFindings),
			},
			wantActions: []types.ApprovalAction{types.ActionFix},
		},
		{
			name: "fix review of a red CI gate is not accepted",
			runs: []*ipc.RunInfo{redCIRun(types.StepStatusFixReview, redCIAskUserFindings)},
		},
		{
			name: "cancelled check without a rerun and no finding ids is not approved",
			runs: []*ipc.RunInfo{redCIRun(types.StepStatusAwaitingApproval,
				`{"findings":[{"severity":"warning","description":"check cancelled and no rerun will replace it","action":"ask-user","category":"ci-transient"}]}`)},
		},
		{
			name: "informational-only CI findings are not approved",
			runs: []*ipc.RunInfo{redCIRun(types.StepStatusAwaitingApproval,
				`{"findings":[{"id":"ci-2","severity":"info","description":"checks not all green","action":"no-op"}]}`)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, calls := recordingRespondClient(t)
			source := &scriptedRunStateSource{
				subscriptions: []scriptedSubscription{{events: make(chan ipc.Event)}},
				runs:          tt.runs,
			}
			reconciler := newRunReconciler(source, "run-1")
			reconciler.heartbeatInterval = time.Millisecond
			defer reconciler.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var progress bytes.Buffer
			run, ciReady, err := driveRunWithReconciler(ctx, &progress, client, reconciler, "run-1", true)
			if err != nil {
				t.Fatalf("driveRun: %v (progress: %s)", err, progress.String())
			}
			if ciReady || run != tt.runs[len(tt.runs)-1] {
				t.Fatalf("driveRun returned run=%+v ciReady=%v, want the final parked gate", run, ciReady)
			}
			var got []types.ApprovalAction
			for _, call := range calls() {
				got = append(got, call.Action)
			}
			if !slices.Equal(got, tt.wantActions) {
				t.Fatalf("--yes responses = %v, want %v", got, tt.wantActions)
			}
			if !strings.Contains(progress.String(), "never approves or skips a red CI gate") {
				t.Fatalf("missing red-CI guidance: %s", progress.String())
			}
			var output bytes.Buffer
			cmd := newAxiRunCmd()
			cmd.SetOut(&output)
			if err := renderDriveResult(cmd, run, ciReady); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(output.String(), "gate:\n  step: ci") || strings.Contains(output.String(), "passed-with-override") {
				t.Fatalf("--yes output is not a parked CI gate:\n%s", output.String())
			}
		})
	}
}

func redCIRespondFixture(t *testing.T) *atomic.Bool {
	t.Helper()
	var responded atomic.Bool
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{responded: &responded})
	fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) {
		if responded.Load() {
			return fx.completed(), nil
		}
		run := fx.running()
		findings := redCIAskUserFindings
		run.Steps = []ipc.StepResultInfo{{StepName: types.StepCI, Status: types.StepStatusAwaitingApproval, FindingsJSON: &findings}}
		return run, nil
	})
	return &responded
}

func TestAxiRespond_YesRejectsApproveAndSkipOfRedCIGate(t *testing.T) {
	for _, action := range []string{"approve", "skip"} {
		t.Run(action, func(t *testing.T) {
			responded := redCIRespondFixture(t)
			out, err := executeCmd("axi", "respond", "--action", action, "--yes", "--wait", "3s")
			var ee *exitError
			if !errors.As(err, &ee) || ee.code != 2 {
				t.Fatalf("respond --action %s --yes error = %v, want usage exit 2\n%s", action, err, out)
			}
			if responded.Load() {
				t.Fatalf("respond --action %s --yes reached the daemon for a red CI gate", action)
			}
			if !strings.Contains(out, "--yes cannot "+action+" the ci gate: CI is not green") {
				t.Fatalf("missing refusal reason:\n%s", out)
			}
		})
	}
}

func TestAxiRespond_RedCIGateStillAcceptsExplicitApproveAndYesFix(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"explicit approve", []string{"axi", "respond", "--action", "approve", "--wait", "3s"}},
		{"explicit skip", []string{"axi", "respond", "--action", "skip", "--wait", "3s"}},
		{"fix with --yes", []string{"axi", "respond", "--action", "fix", "--finding", "ci-1", "--yes", "--wait", "3s"}},
	} {
		args := tc.args
		t.Run(tc.name, func(t *testing.T) {
			responded := redCIRespondFixture(t)
			out, err := executeCmd(args...)
			if err != nil {
				t.Fatalf("%v: %v\n%s", args, err, out)
			}
			if !responded.Load() {
				t.Fatalf("%v did not reach the daemon", args)
			}
		})
	}
}
