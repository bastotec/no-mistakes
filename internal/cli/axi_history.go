package cli

import (
	"crypto/rand"
	"fmt"
	"os"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/spf13/cobra"
)

func runAxiPreserveHistory(cmd *cobra.Command, autoYes bool, skip []types.StepName, intent, baseBranch, baseSHA, nonce, generation string, wait time.Duration) error {
	if err := steps.ValidateHistoryBase(baseSHA); err != nil {
		return emitError(cmd, 2, err.Error())
	}
	baseBranch, err := steps.ValidateRunPRBaseBranchName(baseBranch)
	if err != nil || baseBranch == "" {
		return emitError(cmd, 2, "--preserve-history requires an explicit valid --base-branch")
	}
	if len(skip) != 0 {
		return emitError(cmd, 2, "--preserve-history cannot be combined with --skip")
	}
	if err := validateAxiWait(wait); err != nil {
		return emitError(cmd, 2, err.Error())
	}
	if (nonce == "") != (generation == "") {
		return emitError(cmd, 2, "--launch-nonce and --validation-generation must be supplied together")
	}
	reuseActive := nonce == ""
	if reuseActive {
		nonce, generation = rand.Text(), "preserve-history-v1"
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
	if err != nil || branch == "HEAD" || branch == baseBranch {
		return emitError(cmd, 2, "preserve-history requires a checked-out feature branch distinct from --base-branch")
	}
	head, err := git.HeadSHA(ctx, ".")
	if err != nil {
		return emitError(cmd, 1, err.Error())
	}
	wd, err := os.Getwd()
	if err != nil {
		return emitError(cmd, 1, err.Error())
	}
	// No gate push and no fallback on this path. Older daemons must reject
	// the unknown method before any hook could start an unconstrained run.
	var result ipc.StartFreshRunResult
	err = env.client.Call(ipc.MethodStartHistoryRun, &ipc.StartHistoryRunParams{
		StartFreshRunParams: ipc.StartFreshRunParams{
			RepoID: env.repo.ID, Branch: branch, HeadSHA: head, Intent: intent,
			LaunchNonce: nonce, ValidationGeneration: generation, PRBaseBranch: baseBranch,
		},
		WorkDir: wd, PreserveHistoryBaseSHA: baseSHA, ReuseActive: reuseActive,
	}, &result)
	if err != nil {
		return emitError(cmd, 1, fmt.Sprintf("start preserve-history run: %v", err),
			"Both CLI and daemon must support preserve-history; no unconstrained fallback or daemon restart is attempted")
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
