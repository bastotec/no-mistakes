package steps

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// newHistoryMonitorFixture is the preserve-history fixture driven through the
// CI monitor instead of a repair, with the monitor's view of the live PR base
// under the test's control. Auto-fix is off so the poll loop's own decision is
// what the outcome reports.
func newHistoryMonitorFixture(t *testing.T, env ...string) (*ciRepairFixture, string) {
	t.Helper()
	f, base, _ := newHistoryFixture(t, false)
	f.sctx.Config.AutoFix.CI = 0
	f.sctx.Env = append(f.sctx.Env, env...)
	return f, base
}

func historyRefusalFinding(t *testing.T, outcome *pipeline.StepOutcome) Finding {
	t.Helper()
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("outcome = %#v, want a parked decision", outcome)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatalf("parse parked findings: %v", err)
	}
	for _, item := range findings.Items {
		if item.ID == "ci-preserve-history-refusal" {
			if item.Action != types.ActionAskUser {
				t.Fatalf("history refusal action = %q, want ask-user", item.Action)
			}
			return item
		}
	}
	t.Fatalf("no history refusal in parked findings: %s", outcome.Findings)
	return Finding{}
}

func assertNoHistoryRefusal(t *testing.T, outcome *pipeline.StepOutcome) {
	t.Helper()
	if outcome == nil {
		return
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		return
	}
	for _, item := range findings.Items {
		if item.ID == "ci-preserve-history-refusal" {
			t.Fatalf("monitor refused the pinned base: %s", item.Description)
		}
	}
}

// A base nobody could read is unknown, not mismatched: only the GitHub host
// answers GetPRBaseBranch at all, so every other forge reports "" and a run
// that is correctly targeted must keep monitoring.
func TestPreserveHistory_MonitorTreatsAnUnverifiableBaseAsUnknown(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     []string
		wantLog string
	}{
		{name: "forge_reports_no_base", env: []string{"FAKE_CLI_PR_BASE="}},
		{
			name:    "base_read_fails_transiently",
			env:     []string{"FAKE_CLI_PR_BASE_ERR=connection reset by peer"},
			wantLog: "could not verify live PR base",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := newHistoryMonitorFixture(t, tc.env...)
			outcome, err := f.run(t)
			if err != nil {
				t.Fatalf("monitor failed on an unverifiable base: %v\nlog:\n%s", err, f.log())
			}
			assertNoHistoryRefusal(t, outcome)
			if outcome == nil || !outcome.NeedsApproval {
				t.Fatalf("outcome = %#v, want the failing-check observation\nlog:\n%s", outcome, f.log())
			}
			if tc.wantLog != "" && !strings.Contains(f.log(), tc.wantLog) {
				t.Fatalf("expected the read failure to be reported as retryable, log:\n%s", f.log())
			}
		})
	}
}

// A PR genuinely retargeted away from the pin parks for a decision: the PR is
// open and its checks may be green, and nothing is about to mutate history.
func TestPreserveHistory_MonitorParksOnALiveBaseThatIsNotThePin(t *testing.T) {
	f, _ := newHistoryMonitorFixture(t, "FAKE_CLI_PR_BASE=release")
	if err := f.sctx.DB.SetRunCIReady(f.sctx.Run.ID, true); err != nil {
		t.Fatal(err)
	}
	outcome, err := f.run(t)
	if err != nil {
		t.Fatalf("monitor ended the run instead of parking: %v\nlog:\n%s", err, f.log())
	}
	finding := historyRefusalFinding(t, outcome)
	if !strings.Contains(finding.Description, "not the pinned integration branch") {
		t.Fatalf("refusal lost its diagnostic: %s", finding.Description)
	}
	run, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.CIReadyAt != nil {
		t.Fatal("the refusal left a stale checks-passed readiness flag behind")
	}
}

// The pinned base advancing mid-monitor is the same refusal, and it must not
// discard the run either.
func TestPreserveHistory_MonitorParksWhenThePinnedBaseAdvancesMidRun(t *testing.T) {
	f, base := newHistoryMonitorFixture(t,
		"FAKE_CLI_PR_BASE=main",
		`FAKE_CLI_CHECKS=[{"name":"test","status":"IN_PROGRESS","bucket":"pending"}]`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.sctx.Ctx = ctx
	polls := 0
	step := &CIStep{waitForNextPoll: func(ctx context.Context, d time.Duration) error {
		polls++
		if polls == 1 {
			tree := gitCmd(t, f.dir, "rev-parse", base+"^{tree}")
			moved := gitCmd(t, f.dir, "commit-tree", tree, "-p", base, "-m", "later main")
			gitCmd(t, f.dir, "push", "origin", moved+":refs/heads/main")
		}
		if polls >= 3 {
			cancel()
		}
		return ctx.Err()
	}}
	outcome, err := driveCI(t, step, f.sctx)
	if err != nil {
		t.Fatalf("monitor ended the run instead of parking: %v\nlog:\n%s", err, f.log())
	}
	finding := historyRefusalFinding(t, outcome)
	if !strings.Contains(finding.Description, "moved") {
		t.Fatalf("refusal lost its diagnostic: %s", finding.Description)
	}
	if polls < 1 {
		t.Fatal("the monitor never polled, so it did not observe the advance")
	}
}

// A base that moved while the executor was between the PR and CI steps is the
// same refusal the poll loop parks on: the PR is already open and its checks
// may be green, and this run can never be restarted in place.
func TestPreserveHistory_MonitorParksWhenThePinnedBaseMovedBeforeTheStepStarts(t *testing.T) {
	f, base := newHistoryMonitorFixture(t, "FAKE_CLI_PR_BASE=main")
	tree := gitCmd(t, f.dir, "rev-parse", base+"^{tree}")
	moved := gitCmd(t, f.dir, "commit-tree", tree, "-p", base, "-m", "later main")
	gitCmd(t, f.dir, "push", "origin", moved+":refs/heads/main")
	outcome, err := f.run(t)
	if err != nil {
		t.Fatalf("CI step ended the run instead of parking: %v\nlog:\n%s", err, f.log())
	}
	finding := historyRefusalFinding(t, outcome)
	if !strings.Contains(finding.Description, "moved") {
		t.Fatalf("refusal lost its diagnostic: %s", finding.Description)
	}
}

// Approving a parked preserve-history refusal is the operator's call and always
// proceeds, but it must land as an override with the refusal named - green
// checks are not evidence that the pinned relationship survived.
func TestPreserveHistory_ApprovingARefusalIsNeverASilentCleanPass(t *testing.T) {
	for _, tc := range []struct {
		name     string
		prBase   string
		moveBase bool
		want     string
	}{
		{name: "pr_retargeted_off_the_pin", prBase: "release", want: "not the pinned integration branch"},
		{name: "pinned_base_advanced", prBase: "main", moveBase: true, want: "moved"},
		{name: "pins_still_honored", prBase: "main"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, base := newHistoryMonitorFixture(t,
				"FAKE_CLI_PR_BASE="+tc.prBase,
				`FAKE_CLI_CHECKS=[{"name":"build","state":"SUCCESS","bucket":"pass"}]`)
			if tc.moveBase {
				tree := gitCmd(t, f.dir, "rev-parse", base+"^{tree}")
				moved := gitCmd(t, f.dir, "commit-tree", tree, "-p", base, "-m", "later main")
				gitCmd(t, f.dir, "push", "origin", moved+":refs/heads/main")
			}
			reason, err := (&CIStep{}).VerifyApprovalOverride(f.sctx)
			if err != nil {
				t.Fatalf("VerifyApprovalOverride() error = %v", err)
			}
			if tc.want == "" {
				if reason != "" {
					t.Fatalf("an honored run was recorded as an override: %s", reason)
				}
				return
			}
			if !strings.Contains(reason, tc.want) {
				t.Fatalf("override reason = %q, want it to name the refusal (%q)", reason, tc.want)
			}
		})
	}
}
