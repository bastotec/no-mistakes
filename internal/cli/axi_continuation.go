package cli

import (
	"fmt"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/scm/github"
	"github.com/spf13/cobra"
	"os"
	"time"
)

func runAxiContinuation(cmd *cobra.Command, autoYes bool, intent, url string, wait time.Duration) error {
	if _, _, err := github.ExistingPRTarget(url); err != nil {
		return emitError(cmd, 2, err.Error())
	}
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
		return emitError(cmd, 2, "continuation requires a checked-out feature branch")
	}
	head, err := git.HeadSHA(ctx, ".")
	if err != nil {
		return emitError(cmd, 1, err.Error())
	}
	wd, err := os.Getwd()
	if err != nil {
		return emitError(cmd, 1, err.Error())
	}
	var result ipc.RerunResult
	if err := env.client.Call(ipc.MethodStartContinuationRun, &ipc.StartContinuationRunParams{StartExistingPRRunParams: ipc.StartExistingPRRunParams{RepoID: env.repo.ID, Branch: branch, HeadSHA: head, Intent: intent, URL: url}, WorkDir: wd}, &result); err != nil {
		return emitError(cmd, existingPRLaunchExitCode(err), fmt.Sprintf("start continuation: %v", err), "Both CLI and daemon must support --continue-pr; no fallback or restart is attempted")
	}
	run, ready, err := driveRun(driveCtx, cmd.ErrOrStderr(), env.client, env.p.Socket(), result.RunID, autoYes)
	if err != nil {
		if isAxiWaitElapsed(ctx, driveCtx, err) {
			return emitAxiWaitElapsed(cmd, wait, "no-mistakes axi status")
		}
		return emitError(cmd, 1, err.Error())
	}
	return renderDriveResult(cmd, run, ready)
}
