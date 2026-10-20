package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestIsolatedRepairCompletedHandoffShutdownStaysUnresolved(t *testing.T) {
	sctx := isolatedRepairContext(t)
	ctx, cancel := context.WithCancelCause(context.Background())
	sctx.Ctx = ctx
	_, err := sctx.RunIsolatedTestRepair(func(mark func(int) error) (*StepOutcome, error) {
		if err := mark(12345); err != nil {
			return nil, err
		}
		// Final index import succeeded: no incomplete-handoff sentinel remains.
		cancel(ErrDaemonShutdown)
		return nil, ErrDaemonShutdown
	})
	if !errors.Is(err, ErrDaemonShutdown) {
		t.Fatalf("shutdown lost: %v", err)
	}
	active, err := sctx.DB.HasStartedRepairDispatch(sctx.Run.ID)
	if err != nil || !active {
		t.Fatalf("shutdown became terminal: active=%v err=%v", active, err)
	}
	called := false
	_, err = sctx.RunIsolatedTestRepair(func(func(int) error) (*StepOutcome, error) { called = true; return nil, nil })
	if err == nil || called {
		t.Fatalf("interrupted side effects replayed: %v %v", called, err)
	}
}

func TestIsolatedRepairTerminalResultClearsActivePID(t *testing.T) {
	sctx := isolatedRepairContext(t)
	sctx.Ctx = context.Background()
	_, err := sctx.RunIsolatedTestRepair(func(mark func(int) error) (*StepOutcome, error) {
		if err := mark(12345); err != nil {
			return nil, err
		}
		return &StepOutcome{Findings: `{"findings":[]}`}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil || len(steps) != 1 || steps[0].AgentPID != nil {
		t.Fatalf("terminal result retains active identity: %+v %v", steps, err)
	}
}

func TestIsolatedCustodyRefusesNestedRepositoriesBeforeHostInspection(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX callback canary")
	}
	for _, kind := range []string{"gitlink", "untracked", "git-file", "bare"} {
		t.Run(kind, func(t *testing.T) {
			dir := survivedWorkRepo(t)
			nested := filepath.Join(dir, "nested")
			switch kind {
			case "bare":
				gitRun(t, dir, "init", "--bare", nested)
			case "git-file":
				gitRun(t, dir, "init", "--separate-git-dir", filepath.Join(t.TempDir(), "metadata"), nested)
			default:
				gitRun(t, dir, "init", nested)
			}
			marker := filepath.Join(t.TempDir(), "host-callback")
			if kind != "bare" {
				writeFile(t, nested, "tracked", "original\n")
				writeFile(t, nested, ".gitattributes", "tracked filter=custom\n")
				gitRun(t, nested, "add", "-A")
				gitRun(t, nested, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "nested")
				if kind == "gitlink" {
					gitRun(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+gitRun(t, nested, "rev-parse", "HEAD")+",nested")
				}
				writeFile(t, nested, "clean.sh", "#!/bin/sh\nprintf called >> '"+marker+"'\ncat\n")
				gitRun(t, nested, "config", "filter.custom.clean", "sh ./clean.sh")
				writeFile(t, nested, "tracked", "modified\n")
			}
			// All layouts carry callback canaries, including bare repositories.
			// No worktree-reading Git command enters it after arming them.
			callbacks := t.TempDir()
			callback := "#!/bin/sh\nprintf called >> '" + marker + "'\nexit 1\n"
			if err := os.WriteFile(filepath.Join(callbacks, "monitor"), []byte(callback), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(callbacks, "post-index-change"), []byte(callback), 0700); err != nil {
				t.Fatal(err)
			}
			gitRun(t, nested, "config", "core.fsmonitor", filepath.Join(callbacks, "monitor"))
			gitRun(t, nested, "config", "core.hooksPath", callbacks)
			gitRun(t, nested, "config", "filter.custom.process", filepath.Join(callbacks, "monitor"))
			g := survivedWorkGit{noCallbacks: true}
			recorded := false
			work, err := g.CaptureSurvivedWork(dir, "isolated-nested", func(work *types.SurvivedWork) error {
				recorded = work != nil && work.State == types.SurvivedWorkInPlace
				return nil
			})
			if !recorded {
				t.Fatal("unsupported custody was not handed to its durable recorder")
			}
			if err != nil || work == nil || work.State != types.SurvivedWorkInPlace || work.Ref != "" {
				t.Fatalf("unsupported work not retained: %+v %v", work, err)
			}
			if !SurvivedWorkHoldsWorktree(work, dir, true) {
				t.Fatal("cleanup would delete unsupported work")
			}
			if err := g.RestoreSurvivedWork(dir, &types.SurvivedWork{Ref: "HEAD", BaseSHA: gitRun(t, dir, "rev-parse", "HEAD")}); err == nil {
				t.Fatal("restore admitted nested repository")
			}
			if _, err := os.Stat(nested); err != nil {
				t.Fatal("nested work disappeared:", err)
			}
			if kind != "bare" {
				if got, err := os.ReadFile(filepath.Join(nested, "tracked")); err != nil || string(got) != "modified\n" {
					t.Fatalf("nested bytes changed: %q %v", got, err)
				}
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("callback ran on host: %v", err)
			}
		})
	}
}
