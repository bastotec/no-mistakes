package steps

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestContinuation_ExecutorAcceptsTerminalCIOnlyWithPinnedGreenEvidence(t *testing.T) {
	for _, state := range []string{"CLOSED", "MERGED"} {
		for _, scenario := range []string{"green", "skip-only", "passing-plus-skipped", "pending", "empty", "stale-checks-head", "retargeted", "moved-base", "moved-source"} {
			passes := scenario == "green" || scenario == "passing-plus-skipped"
			t.Run(state+"/"+scenario, func(t *testing.T) {
				f := newContinuationFixture(t)
				f.publish(t)
				if err := f.s.DB.UpdateRunPRState(f.s.Run.ID, "open"); err != nil {
					t.Fatal(err)
				}
				checks := `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`
				if scenario == "pending" {
					checks = `[{"name":"build","state":"PENDING","bucket":"pending"}]`
				} else if scenario == "empty" {
					checks = `[]`
				} else if scenario == "skip-only" {
					checks = `[{"name":"build","state":"SKIPPED","bucket":"skip"}]`
				} else if scenario == "passing-plus-skipped" {
					checks = `[{"name":"build","state":"SUCCESS","bucket":"pass"},{"name":"optional","state":"SKIPPED","bucket":"skip"}]`
				}
				for _, entry := range continuationCIEnv(f, fakeCIGH(t, "OPEN", checks)) {
					key, value, _ := strings.Cut(entry, "=")
					t.Setenv(key, value)
				}
				ci := (&CIStep{}).SetBaseBranchTip(func(context.Context) (string, bool) { return f.tip, true })
				ci.SetWaitForNextPoll(func(context.Context, time.Duration) error {
					t.Fatal("terminal CI kept polling")
					return nil
				})
				enteredCI := false
				executor := pipeline.NewExecutor(f.s.DB, paths.WithRoot(t.TempDir()), f.s.Config, f.s.Agent,
					[]pipeline.Step{&RebaseStep{}, ci}, func(event ipc.Event) {
						if event.Type != ipc.EventStepStarted || event.StepName == nil || *event.StepName != types.StepCI {
							return
						}
						enteredCI = true
						base := "main"
						switch scenario {
						case "stale-checks-head":
							t.Setenv("FAKE_CLI_PR_HEAD_SHA", f.published)
						case "retargeted":
							base = "other"
						case "moved-base":
							gitCmd(t, f.upstream, "update-ref", "refs/heads/main", f.base)
						case "moved-source":
							gitCmd(t, f.fork, "update-ref", "refs/heads/feature", f.published)
						}
						f.setPRState(f.submitted, base, state)
						for _, entry := range f.s.Env {
							if key, value, _ := strings.Cut(entry, "="); key == "FAKE_CLI_EXISTING_PR_JSON" {
								t.Setenv(key, value)
							}
						}
						t.Setenv("FAKE_CLI_STATE", state)
					})
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				err := executor.Execute(ctx, f.s.Run, f.s.Repo, f.s.WorkDir)
				if !enteredCI || (err == nil) != passes {
					t.Fatalf("entered CI = %v, execution error = %v", enteredCI, err)
				}
				stored, err := f.s.DB.GetRun(f.s.Run.ID)
				if err != nil {
					t.Fatal(err)
				}
				steps, err := f.s.DB.GetStepsByRun(f.s.Run.ID)
				if err != nil || len(steps) != 2 {
					t.Fatalf("steps = %+v, err = %v", steps, err)
				}
				if passes {
					if stored.Status != types.RunCompleted || steps[1].Status != types.StepStatusCompleted || stored.PRState == nil || *stored.PRState != strings.ToLower(state) {
						t.Fatalf("terminal completion lost: run=%+v steps=%+v", stored, steps)
					}
				} else if stored.Status != types.RunFailed || steps[1].Status != types.StepStatusFailed || stored.CIReadyAt != nil || (stored.PRState != nil && *stored.PRState != "open") {
					t.Fatalf("invalid terminal evidence accepted: run=%+v steps=%+v", stored, steps)
				}
				if got := gitCmd(t, f.s.WorkDir, "rev-parse", "HEAD"); got != f.submitted {
					t.Fatal("CI changed pinned history")
				}
			})
		}
	}
}

func TestContinuation_ExecutorMonitorWaitsForChecksObservedAtFinalHead(t *testing.T) {
	f := newContinuationFixture(t)
	f.publish(t)
	for _, entry := range continuationCIEnv(f, fakeCIGH(t, "OPEN", `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`)) {
		key, value, _ := strings.Cut(entry, "=")
		t.Setenv(key, value)
	}
	t.Setenv("FAKE_CLI_PR_HEAD_SHA", f.published)
	polls, readyEvents := 0, 0
	ci := (&CIStep{}).SetBaseBranchTip(func(context.Context) (string, bool) { return f.tip, true })
	ci.SetWaitForNextPoll(func(context.Context, time.Duration) error {
		polls++
		stored, err := f.s.DB.GetRun(f.s.Run.ID)
		if err != nil {
			t.Fatal(err)
		}
		switch polls {
		case 1:
			if stored.CIReadyAt != nil || readyEvents != 0 {
				t.Fatal("stale checks certified the REST/source head")
			}
			t.Setenv("FAKE_CLI_PR_HEAD_SHA", f.submitted)
		case 2:
			if stored.CIReadyAt == nil || readyEvents != 1 {
				t.Fatal("fresh final-head checks did not become ready")
			}
			f.setPRState(f.submitted, "main", "CLOSED")
			for _, entry := range f.s.Env {
				if key, value, _ := strings.Cut(entry, "="); key == "FAKE_CLI_EXISTING_PR_JSON" {
					t.Setenv(key, value)
				}
			}
			t.Setenv("FAKE_CLI_STATE", "CLOSED")
		default:
			t.Fatal("monitor did not complete after exact-head green close")
		}
		return nil
	})
	forbiddenAgent := &mockAgent{name: "forbidden", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		t.Fatal("stale checks launched a repair agent")
		return nil, nil
	}}
	executor := pipeline.NewExecutor(f.s.DB, paths.WithRoot(t.TempDir()), f.s.Config, forbiddenAgent,
		[]pipeline.Step{&RebaseStep{}, ci}, func(event ipc.Event) {
			if event.CIReady != nil && *event.CIReady {
				readyEvents++
			}
		})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := executor.Execute(ctx, f.s.Run, f.s.Repo, f.s.WorkDir); err != nil {
		t.Fatal(err)
	}
	stored, err := f.s.DB.GetRun(f.s.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if polls != 2 || readyEvents != 1 || stored.Status != types.RunCompleted || stored.PRState == nil || *stored.PRState != "closed" {
		t.Fatalf("polls=%d ready events=%d run=%+v", polls, readyEvents, stored)
	}
}
