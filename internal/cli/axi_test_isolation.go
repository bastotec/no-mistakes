package cli

import (
	"crypto/rand"
	"fmt"
	"os"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/spf13/cobra"
)

func runAxiIsolatedTest(cmd *cobra.Command, autoYes bool, intent, baseBranch string, wait time.Duration) error {
	return startIsolationLaunch(cmd, autoYes, intent, baseBranch, wait, ipc.MethodStartIsolatedTestRun, "test-isolation-v1", "--isolate-test", "start isolated Test run")
}

// runAxiConfinedRun launches a whole-run confined run. It shares the isolated
// Test launch's shape on purpose: no push, no hook, no fallback, and a distinct
// RPC so an old daemon refuses instead of silently launching an unconfined run.
func runAxiConfinedRun(cmd *cobra.Command, autoYes bool, intent, baseBranch string, wait time.Duration) error {
	return startIsolationLaunch(cmd, autoYes, intent, baseBranch, wait, ipc.MethodStartConfinedRun, "run-confinement-v1", "--isolate", "start confined run")
}

func startIsolationLaunch(cmd *cobra.Command, autoYes bool, intent, baseBranch string, wait time.Duration, method string, generation, flag, action string) error {
	ctx := cmd.Context()
	driveCtx, cancel, err := boundAxiWait(ctx, wait)
	if err != nil {
		return emitError(cmd, 2, err.Error())
	}
	defer cancel()
	env, err := openAxiRunEnv()
	if err != nil {
		return emitError(cmd, 1, err.Error(), repoInitHelp(err)...)
	}
	defer env.close()
	if env.globalConfigErr != nil {
		return emitError(cmd, 1, env.globalConfigErr.Error())
	}
	branch, err := git.CurrentBranch(ctx, ".")
	if err != nil || branch == "HEAD" || branch == env.repo.DefaultBranch {
		return emitError(cmd, 2, flag+" requires a checked-out feature branch")
	}
	head, err := git.HeadSHA(ctx, ".")
	if err != nil {
		return emitError(cmd, 1, err.Error())
	}
	wd, err := os.Getwd()
	if err != nil {
		return emitError(cmd, 1, err.Error())
	}
	var result ipc.StartFreshRunResult
	// No push, no hook, no fallback. An old daemon must reject this new method.
	err = env.client.Call(method, &ipc.StartHistoryRunParams{
		StartFreshRunParams: ipc.StartFreshRunParams{RepoID: env.repo.ID, Branch: branch, HeadSHA: head, Intent: intent, PRBaseBranch: baseBranch, LaunchNonce: rand.Text(), ValidationGeneration: generation},
		WorkDir:             wd, ReuseActive: true,
	}, &result)
	if err != nil {
		return emitError(cmd, 1, fmt.Sprintf("%s: %v", action, err), "Both CLI and daemon must support this isolation mode; no ordinary fallback or shared daemon restart is attempted")
	}
	emitLaunchReceipt(cmd, result.Receipt)
	run, ready, err := driveRun(driveCtx, cmd.ErrOrStderr(), env.client, env.p.Socket(), result.Receipt.RunID, autoYes)
	if err != nil {
		if isAxiWaitElapsed(ctx, driveCtx, err) {
			return emitAxiWaitElapsed(cmd, wait, "no-mistakes axi status")
		}
		return emitError(cmd, 1, fmt.Sprintf("drive run: %v", err))
	}
	return renderDriveResult(cmd, run, ready)
}
