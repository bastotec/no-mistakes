package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func isolatedRepairContext(t *testing.T) *StepContext {
	t.Helper()
	database, _, run, repo := setupTest(t)
	step, err := database.InsertStepResult(run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStepWithAutoFixLimit(step.ID, 1); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"test-1","severity":"error","action":"auto-fix","description":"fixture failure"}]}`
	round, err := database.InsertStepRound(step.ID, 1, "initial", &findings, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if decision, err := database.ReserveStepRepairWithSelection(step.ID, round.ID, 1, nil); err != nil || !decision.Granted {
		t.Fatalf("reserve repair: %+v %v", decision, err)
	}
	if err := database.ClaimStepRepair(step.ID, round.ID); err != nil {
		t.Fatal(err)
	}
	return &StepContext{DB: database, Run: run, Repo: repo, Fixing: true, StepResultID: step.ID, WorkDir: t.TempDir(), IsolatedRepairRound: func() string { return round.ID }}
}
func TestIsolatedTestRepairTerminalReplayDoesNotRepeatSideEffects(t *testing.T) {
	sctx := isolatedRepairContext(t)
	calls := 0
	work := func(mark func(int) error) (*StepOutcome, error) {
		calls++
		if err := mark(12345); err != nil {
			return nil, err
		}
		return &StepOutcome{FixSummary: "isolated fixture repaired", Findings: `{"findings":[]}`}, nil
	}
	first, err := sctx.RunIsolatedTestRepair(work)
	if err != nil {
		t.Fatal(err)
	}
	// New StepContext represents a recovered executor; the only replay source
	// is the durable invocation, never an agent's judgement about its old tools.
	recovered := *sctx
	second, err := recovered.RunIsolatedTestRepair(work)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || first.FixSummary != second.FixSummary || first.Findings != second.Findings {
		t.Fatalf("repair was replayed: calls=%d first=%+v second=%+v", calls, first, second)
	}
}
func TestIsolatedTestInterruptedRepairRefusesReplay(t *testing.T) {
	sctx := isolatedRepairContext(t)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("expected simulated process interruption")
			}
		}()
		sctx.RunIsolatedTestRepair(func(mark func(int) error) (*StepOutcome, error) {
			if err := mark(12345); err != nil {
				t.Fatal(err)
			}
			panic("simulated daemon death before terminal result")
		})
	}()
	called := false
	_, err := sctx.RunIsolatedTestRepair(func(func(int) error) (*StepOutcome, error) { called = true; return &StepOutcome{}, nil })
	if err == nil || !strings.Contains(err.Error(), "refusing to replay") || called {
		t.Fatalf("interrupted side effects were replayed: called=%v err=%v", called, err)
	}
}

func TestIsolatedTestRepairReplaysTypedFailures(t *testing.T) {
	for _, failure := range []error{fmt.Errorf("fix: %w", ErrTestAgentTimeout), fmt.Errorf("fix: %w", &ProtectedPathError{Path: "package.lock", Rule: "*.lock"})} {
		t.Run(failure.Error(), func(t *testing.T) {
			sctx := isolatedRepairContext(t)
			calls := 0
			work := func(mark func(int) error) (*StepOutcome, error) {
				calls++
				if err := mark(os.Getpid()); err != nil {
					return nil, err
				}
				payload, err := json.Marshal(EncodeIsolatedTestError(failure))
				if err != nil {
					return nil, err
				}
				var received IsolatedTestError
				if err := json.Unmarshal(payload, &received); err != nil {
					return nil, err
				}
				return nil, received.Restore()
			}
			for attempt := 0; attempt < 2; attempt++ {
				_, err := sctx.RunIsolatedTestRepair(work)
				if errors.Is(failure, ErrTestAgentTimeout) {
					if !errors.Is(err, ErrTestAgentTimeout) {
						t.Fatalf("timeout identity lost on attempt %d: %v", attempt, err)
					}
				} else {
					var refusal *ProtectedPathError
					if !errors.As(err, &refusal) || refusal.Path != "package.lock" || refusal.Rule != "*.lock" {
						t.Fatalf("refusal details lost on attempt %d: %v", attempt, err)
					}
					outcome := ProtectedPathOutcome(err)
					if outcome == nil || !outcome.NeedsApproval || !HasProtectedPathRefusal(outcome.Findings) {
						t.Fatalf("refusal no longer parks: %+v", outcome)
					}
				}
			}
			if calls != 1 {
				t.Fatalf("terminal failure reran worker %d times", calls)
			}
		})
	}
}

func TestExecutor_IsolatedTestTimeoutCapturesSurvivedWork(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("isolated custody directory inspection requires Linux")
	}
	database, p, run, repo := setupTest(t)
	if err := database.EnableTestIsolation(run.ID); err != nil {
		t.Fatal(err)
	}
	dir := survivedWorkRepo(t)
	marker := filepath.Join(t.TempDir(), "host-hook-called")
	gitRun(t, dir, "config", "core.hooksPath", ".githooks")
	step := &adaptiveCallStep{name: types.StepTest, fn: func(sctx *StepContext) (*StepOutcome, error) {
		if !sctx.Fixing {
			return &StepOutcome{NeedsApproval: true, AutoFixable: true, Findings: `{"findings":[{"id":"selected","severity":"error","description":"repair","action":"auto-fix"}]}`}, nil
		}
		work := func(mark func(int) error) (*StepOutcome, error) {
			if err := mark(os.Getpid()); err != nil {
				return nil, err
			}
			writeFile(t, dir, "progress.txt", "survived")
			if err := os.Mkdir(filepath.Join(dir, ".githooks"), 0o755); err != nil {
				return nil, err
			}
			if err := os.WriteFile(filepath.Join(dir, ".githooks", "post-index-change"), []byte("#!/bin/sh\nprintf called >> '"+marker+"'\n"), 0o755); err != nil {
				return nil, err
			}
			encoded, err := json.Marshal(EncodeIsolatedTestError(fmt.Errorf("isolated fix: %w", ErrTestAgentTimeout)))
			if err != nil {
				return nil, err
			}
			var received IsolatedTestError
			if err := json.Unmarshal(encoded, &received); err != nil {
				return nil, err
			}
			return nil, received.Restore()
		}
		if _, err := sctx.RunIsolatedTestRepair(work); !errors.Is(err, ErrTestAgentTimeout) {
			return nil, fmt.Errorf("initial timeout lost: %v", err)
		}
		return sctx.RunIsolatedTestRepair(func(func(int) error) (*StepOutcome, error) { t.Error("terminal repair reran"); return nil, nil })
	}}
	executor := NewExecutor(database, p, &config.Config{AutoFix: config.AutoFix{Test: 1}}, nil, []Step{step}, nil)
	_ = executor.Execute(context.Background(), run, repo, dir)
	work, err := database.GetRunSurvivedWork(run.ID)
	if err != nil || work == nil || work.State != types.SurvivedWorkCaptured {
		t.Fatalf("isolated timeout left no handoff: %+v %v", work, err)
	}
	if got := gitRun(t, dir, "show", work.Ref+":progress.txt"); got != "survived" {
		t.Fatalf("repair lost: %q", got)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("isolated repair hook executed on host: %v", err)
	}
}

func TestExecutor_IsolatedTestShutdownRetainsRecoverableRepairGate(t *testing.T) {
	database, p, run, repo := setupTest(t)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	step := &adaptiveCallStep{name: types.StepTest, fn: func(sctx *StepContext) (*StepOutcome, error) {
		if !sctx.Fixing {
			return &StepOutcome{NeedsApproval: true, AutoFixable: true, Findings: `{"findings":[{"id":"test-1","severity":"error","description":"repair","action":"auto-fix"}]}`}, nil
		}
		return sctx.RunIsolatedTestRepair(func(mark func(int) error) (*StepOutcome, error) {
			if err := mark(12345); err != nil {
				return nil, err
			}
			cancel(ErrDaemonShutdown)
			return nil, errors.Join(context.Cause(sctx.Ctx), ErrIsolatedTestHandoff)
		})
	}}
	executor := NewExecutor(database, p, &config.Config{}, nil, []Step{step}, nil)
	done := make(chan struct{})
	var executeErr error
	workdir := t.TempDir()
	go func() {
		executeErr = executor.Execute(ctx, run, repo, workdir)
		close(done)
	}()
	defer func() { cancel(nil); <-done }()
	waitForStepStatus(t, database, run.ID, types.StepTest, types.StepStatusAwaitingApproval)
	if err := executor.Respond(types.StepTest, types.ActionFix, []string{"test-1"}); err != nil {
		t.Fatal(err)
	}
	<-done
	if !errors.Is(executeErr, ErrDaemonShutdown) {
		t.Fatalf("shutdown classification lost: %v", executeErr)
	}
	results, err := database.GetStepsByRun(run.ID)
	if err != nil || len(results) != 1 || results[0].Status != types.StepStatusFixing {
		t.Fatalf("shutdown failed active Test repair: %+v %v", results, err)
	}
	rounds, err := database.GetRoundsByStep(results[0].ID)
	if err != nil || len(rounds) == 0 {
		t.Fatalf("repair round missing: %+v %v", rounds, err)
	}
	if _, err := database.MarkRepairInvocationUnresolved(results[0].ID, rounds[len(rounds)-1].ID); err != nil {
		t.Fatal(err)
	}
	stored, err := database.GetRun(run.ID)
	if err != nil || stored.Status != types.RunRunning {
		t.Fatalf("shutdown failed active run: %+v %v", stored, err)
	}
	gate, err := executor.recoveredGate(run.ID)
	if err != nil || gate == nil || !gate.repairUnresolved {
		t.Fatalf("interrupted repair gate is not recoverable: %+v %v", gate, err)
	}
}

func TestIsolatedTestIncompleteHandoffRetainsRepairCustody(t *testing.T) {
	sctx := isolatedRepairContext(t)
	_, err := sctx.RunIsolatedTestRepair(func(mark func(int) error) (*StepOutcome, error) {
		if err := mark(os.Getpid()); err != nil {
			return nil, err
		}
		return nil, errors.Join(ErrTestAgentTimeout, ErrIsolatedTestHandoff)
	})
	if !errors.Is(err, ErrTestAgentTimeout) || !errors.Is(err, ErrIsolatedTestHandoff) {
		t.Fatalf("handoff failure lost: %v", err)
	}
	called := false
	_, err = sctx.RunIsolatedTestRepair(func(func(int) error) (*StepOutcome, error) { called = true; return &StepOutcome{}, nil })
	if err == nil || !strings.Contains(err.Error(), "refusing to replay") || called {
		t.Fatalf("incomplete handoff replayed: called=%v err=%v", called, err)
	}
}

func TestExecutor_InterruptedProtectedPathRefusalDoesNotCompleteRepair(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cause  error
		cancel bool
	}{
		{name: "incomplete handoff", cause: ErrIsolatedTestHandoff},
		{name: "cancelled", cause: context.Canceled},
		{name: "deadline", cause: context.DeadlineExceeded},
		{name: "shutdown", cause: ErrDaemonShutdown},
		{name: "parent cancellation", cancel: true},
		{name: "shutdown during handoff", cause: errors.Join(ErrDaemonShutdown, ErrIsolatedTestHandoff)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			ctx, stop := context.WithTimeout(context.Background(), 2*time.Second)
			defer stop()
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			var stepID, roundID string
			step := &adaptiveCallStep{name: types.StepTest, fn: func(sctx *StepContext) (*StepOutcome, error) {
				if !sctx.Fixing {
					return &StepOutcome{NeedsApproval: true, AutoFixable: true, Findings: `{"findings":[{"id":"test-1","severity":"error","description":"repair","action":"auto-fix"}]}`}, nil
				}
				stepID, roundID = sctx.StepResultID, sctx.IsolatedRepairRound()
				return sctx.RunIsolatedTestRepair(func(mark func(int) error) (*StepOutcome, error) {
					if err := mark(os.Getpid()); err != nil {
						return nil, err
					}
					if tc.cancel {
						cancel()
					}
					return nil, errors.Join(&ProtectedPathError{Path: "package.lock", Rule: "*.lock"}, tc.cause)
				})
			}}
			executor := NewExecutor(database, p, &config.Config{AutoFix: config.AutoFix{Test: 1}}, nil, []Step{step}, nil)
			if err := executor.Execute(ctx, run, repo, t.TempDir()); err == nil {
				t.Fatal("interrupted repair passed")
			}
			rounds, err := database.GetRoundsByStep(stepID)
			if err != nil || len(rounds) != 1 {
				t.Fatalf("interrupted refusal recorded as a completed round: %+v %v", rounds, err)
			}
			if errors.Is(tc.cause, ErrIsolatedTestHandoff) {
				invocationID, err := database.RepairInvocationForRound(stepID, roundID)
				if err != nil {
					t.Fatal(err)
				}
				invocation, err := database.GetRepairInvocation(invocationID)
				if err != nil || invocation.State != "fixer_started" {
					t.Fatalf("incomplete handoff lost started custody: %+v %v", invocation, err)
				}
				active, err := database.HasStartedRepairDispatch(run.ID)
				if err != nil || !active {
					t.Fatalf("interrupted dispatch no longer held: active=%v err=%v", active, err)
				}
			}
		})
	}
}

func TestExecutor_IsolatedSurvivedWorkSuppressesContentFilters(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("callback fixtures require a POSIX shell")
	}
	for _, driver := range []string{"clean", "smudge", "process"} {
		for _, isolated := range []bool{false, true} {
			for _, moved := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/isolated=%v/moved=%v", driver, isolated, moved), func(t *testing.T) {
					database, p, run, _ := setupTest(t)
					if isolated {
						if err := database.EnableTestIsolation(run.ID); err != nil {
							t.Fatal(err)
						}
					}
					dir := survivedWorkRepo(t)
					writeFile(t, dir, ".gitattributes", "*.txt filter=CustOM diff=CustOM\n")
					gitRun(t, dir, "add", ".gitattributes")
					gitRun(t, dir, "commit", "-m", "attributes")
					leavePartialRepair(t, dir)
					callbacks := t.TempDir()
					marker := filepath.Join(callbacks, "filter-called")
					textconvMarker := filepath.Join(callbacks, "textconv-called")
					body := "#!/bin/sh\nprintf called >> '" + marker + "'\ncat\n"
					if driver == "process" {
						body = "#!/bin/sh\nprintf called >> '" + marker + "'\nexit 1\n"
					}
					filter := filepath.Join(callbacks, "filter")
					if err := os.WriteFile(filter, []byte(body), 0o755); err != nil {
						t.Fatal(err)
					}
					gitRun(t, dir, "config", "filter.CustOM."+driver, filter)
					// The deliberately failing process fixture must be required:
					// Git versions differ in whether an optional process failure
					// falls back to the unfiltered content.
					gitRun(t, dir, "config", "filter.CustOM.required", fmt.Sprint(isolated || driver == "process"))
					gitRun(t, dir, "config", "diff.CustOM.textconv", "printf called >> '"+textconvMarker+"'; cat")
					assertCallbacks := func() {
						t.Helper()
						_, err := os.Stat(marker)
						if isolated && !os.IsNotExist(err) || !isolated && err != nil {
							t.Fatalf("filter callback isolated=%v: %v", isolated, err)
						}
						if _, err := os.Stat(textconvMarker); !os.IsNotExist(err) {
							t.Fatalf("custody invoked textconv: %v", err)
						}
					}
					executor := NewExecutor(database, p, nil, nil, nil, nil)
					if err := executor.settleSurvivedWork(run.ID, dir, true, func(string) {}); err != nil {
						t.Fatal(err)
					}
					work, err := database.GetRunSurvivedWork(run.ID)
					if driver == "process" && !isolated {
						if err != nil || work == nil || work.State != types.SurvivedWorkInPlace || work.InspectionError == "" {
							t.Fatalf("failed ordinary process filter did not retain work: %+v %v", work, err)
						}
						assertCallbacks()
						return
					}
					if err != nil || work == nil || work.State != types.SurvivedWorkCaptured {
						t.Fatalf("custody capture failed: %+v %v", work, err)
					}
					assertCallbacks()
					if moved {
						writeFile(t, dir, "unrelated", "moved\n")
						g := survivedWorkGit{noCallbacks: true}
						for _, args := range [][]string{{"add", "unrelated"}, {"commit", "-m", "head moved"}} {
							if _, err := g.Run(context.Background(), dir, args...); err != nil {
								t.Fatal(err)
							}
						}
					}
					if _, err := executor.restoreSurvivedWork(run.ID, dir, func(string) {}); err != nil {
						t.Fatal(err)
					}
					assertCallbacks()
					if got := readFile(t, dir, "tracked.txt"); got != "working version\n" {
						t.Fatalf("unstaged work lost: %q", got)
					}
					if got := gitRun(t, dir, "show", ":tracked.txt"); got != "staged version" {
						t.Fatalf("index split lost: %q", got)
					}
				})
			}
		}
	}
}

func TestExecutor_IsolatedSurvivedWorkSuppressesGitCallbacks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("callback fixtures require a POSIX shell")
	}
	for _, mode := range []string{"ordinary", "test", "run"} {
		isolated := mode != "ordinary"
		for _, moved := range []bool{false, true} {
			t.Run(fmt.Sprintf("mode=%s/moved=%v", mode, moved), func(t *testing.T) {
				database, p, run, _ := setupTest(t)
				if mode == "test" {
					if err := database.EnableTestIsolation(run.ID); err != nil {
						t.Fatal(err)
					}
				} else if mode == "run" {
					if err := database.EnableRunConfinement(run.ID, run.HeadSHA, "fixture-digest"); err != nil {
						t.Fatal(err)
					}
				}
				dir := survivedWorkRepo(t)
				leavePartialRepair(t, dir)
				callbacks := t.TempDir()
				hookMarker := filepath.Join(callbacks, "hook-called")
				monitorMarker := filepath.Join(callbacks, "monitor-called")
				for name, body := range map[string]string{
					"post-index-change":     "#!/bin/sh\nprintf called >> '" + hookMarker + "'\n",
					"reference-transaction": "#!/bin/sh\nprintf called >> '" + hookMarker + "'\n",
					"monitor":               "#!/bin/sh\nprintf called >> '" + monitorMarker + "'\nprintf 'token\\000/\\000'\n",
				} {
					if err := os.WriteFile(filepath.Join(callbacks, name), []byte(body), 0o755); err != nil {
						t.Fatal(err)
					}
				}
				gitRun(t, dir, "config", "core.hooksPath", callbacks)
				gitRun(t, dir, "config", "core.fsmonitor", filepath.Join(callbacks, "monitor"))
				safeGit := func(args ...string) string {
					return gitRun(t, dir, append([]string{"-c", "core.hooksPath=" + os.DevNull, "-c", "core.fsmonitor=false"}, args...)...)
				}
				assertCallbacks := func() {
					t.Helper()
					for _, marker := range []string{hookMarker, monitorMarker} {
						_, err := os.Stat(marker)
						if isolated && !os.IsNotExist(err) {
							t.Fatalf("isolated custody ran callback %s: %v", marker, err)
						}
						if !isolated && err != nil {
							t.Fatalf("ordinary custody callback did not run %s: %v", marker, err)
						}
					}
				}
				executor := NewExecutor(database, p, nil, nil, nil, nil)
				if err := executor.settleSurvivedWork(run.ID, dir, true, func(string) {}); err != nil {
					t.Fatal(err)
				}
				work, err := database.GetRunSurvivedWork(run.ID)
				if err != nil || work == nil || work.State != types.SurvivedWorkCaptured {
					t.Fatalf("custody capture failed: %+v %v", work, err)
				}
				if got := safeGit("status", "--porcelain"); got != "" {
					t.Fatalf("capture left work exposed: %s", got)
				}
				assertCallbacks()
				if moved {
					writeFile(t, dir, "unrelated.txt", "moved\n")
					safeGit("add", "unrelated.txt")
					safeGit("commit", "-m", "head moved")
				}
				for _, marker := range []string{hookMarker, monitorMarker} {
					if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
						t.Fatal(err)
					}
				}
				if _, err := executor.restoreSurvivedWork(run.ID, dir, func(string) {}); err != nil {
					t.Fatal(err)
				}
				assertCallbacks()
				if got := readFile(t, dir, "tracked.txt"); got != "working version\n" {
					t.Fatalf("unstaged work lost: %q", got)
				}
				if got := safeGit("show", ":tracked.txt"); got != "staged version" {
					t.Fatalf("index split lost: %q", got)
				}
				if got := readFile(t, dir, "new.txt"); got != "untracked\n" {
					t.Fatalf("untracked work lost: %q", got)
				}
			})
		}
	}
}
