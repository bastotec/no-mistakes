package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type failingResumedCIStep struct {
	reconcilingCIStep
	failure error
}

func (s *failingResumedCIStep) Execute(*StepContext) (*StepOutcome, error) {
	s.executed = true
	return nil, s.failure
}

// Once the resumed PR read succeeds, a later monitor failure is an ordinary
// failure, not the ci_monitor_interrupted fallback for an unreadable PR.
func TestResumeCIWatch_LaterFailureRemainsFailed(t *testing.T) {
	for _, postRepair := range []bool{false, true} {
		name := "plain"
		if postRepair {
			name = "published-repair"
		}
		t.Run(name, func(t *testing.T) {
			database, p, run, repo := setupCIWatch(t)
			ci := stepResultByName(t, database, run.ID, types.StepCI)
			if postRepair {
				findings := `{"findings":[{"id":"ci-1","severity":"error","description":"build failed","action":"auto-fix"}],"summary":"build failed"}`
				if err := database.SetStepFindings(ci.ID, findings); err != nil {
					t.Fatal(err)
				}
				if err := database.UpdateStepStatusWithDuration(ci.ID, types.StepStatusRunning, 1); err != nil {
					t.Fatal(err)
				}
				round, err := database.InsertStepRound(ci.ID, 1, "auto_fix", &findings, nil, 1)
				if err != nil {
					t.Fatal(err)
				}
				inv, err := database.RegisterRepairInvocation(ci.ID, round.ID, 1, t.TempDir(), "claude", "ci", 0)
				if err != nil {
					t.Fatal(err)
				}
				if err := database.FinishRepairInvocation(inv.ID, []byte(`{}`), true, nil, nil); err != nil {
					t.Fatal(err)
				}
				if err := database.MarkRepairPostMonitor(ci.ID, round.ID); err != nil {
					t.Fatal(err)
				}
			}
			reconciled := false
			recordedPR, recordedHead := *run.PRURL, run.HeadSHA
			failure := errors.New("monitor failed after its PR read succeeded")
			step := &failingResumedCIStep{
				reconcilingCIStep: reconcilingCIStep{reconcile: func(sctx *StepContext) (bool, error) {
					reconciled = true
					if sctx.Run.PRURL == nil || *sctx.Run.PRURL != recordedPR || sctx.Run.HeadSHA != recordedHead {
						t.Fatal("resumed PR read lost the recorded PR or head")
					}
					return false, nil
				}},
				failure: failure,
			}
			exec := NewExecutor(database, p, &config.Config{}, nil, []Step{&adaptiveCallStep{name: types.StepReview}, step}, nil)
			resume := exec.ResumeCIWatch
			if postRepair {
				resume = exec.Resume
			}
			if err := resume(context.Background(), run, repo, t.TempDir()); err == nil || !strings.Contains(err.Error(), failure.Error()) {
				t.Fatalf("resume error = %v, want the later monitor failure", err)
			}
			if !reconciled || !step.executed {
				t.Fatal("the resumed watch did not read its PR and execute the monitor")
			}
			if got, err := database.GetRun(run.ID); err != nil || got.Status != types.RunFailed {
				t.Fatalf("run = %v (%v), want failed rather than ci_monitor_interrupted", got, err)
			}
			if got, err := database.GetStepResult(ci.ID); err != nil || got.Status != types.StepStatusFailed {
				t.Fatalf("CI step = %v (%v), want failed", got, err)
			}
		})
	}
}
