package cli

import (
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/spf13/cobra"
	toon "github.com/toon-format/toon-go"
)

func newAxiRepublishPublicationCmd() *cobra.Command {
	var runID, head string
	cmd := &cobra.Command{
		Use:   "republish-publication",
		Short: "Continue a terminal run's failed PR publication without re-running validation",
		Long: "Continue the publication of an ALREADY-TERMINAL run whose PR step failed, without\n" +
			"re-running any validation step. Requires explicit --run and the exact recorded --head\n" +
			"from axi status; never resolves the latest run.\n" +
			"The daemon re-drafts and republishes the PR text for that recorded head through the\n" +
			"ordinary guarded PR path: the publication validator, identifier redaction, and the\n" +
			"bounded sanitize-and-retry loop all apply exactly as during the run. Recorded step\n" +
			"statuses are rendered as they were recorded - no step is re-run, skipped, invented or\n" +
			"waived, and this never grants merge authority or CI readiness.\n" +
			"Refused for: non-terminal runs, an active run on the branch, a head mismatch, a run\n" +
			"whose PR step did not fail, preserve-history runs, or a head absent from the gate\n" +
			"(recover custody first). Nothing is pushed and no validation step executes: the\n" +
			"run's head is already on the remote.\n" +
			"Both CLI and daemon must support this operation; an older daemon refuses with no\n" +
			"fallback or restart.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackAxiSurface("axi-republish-publication", "/axi/republish-publication", nil, func() error {
				if runID == "" {
					return emitError(cmd, 2, "--run is required")
				}
				if err := steps.ValidateHistoryBase(head); err != nil {
					return emitError(cmd, 2, "--head requires a full commit SHA")
				}
				env, err := openAxiDaemonEnv()
				if err != nil {
					return emitError(cmd, 1, err.Error())
				}
				defer env.close()
				var result ipc.RepublishPublicationResult
				err = env.client.CallWithContext(cmd.Context(), ipc.MethodRepublishPublication, ipc.RepublishPublicationParams{RepoID: env.repo.ID, RunID: runID, HeadSHA: head}, &result, 35*time.Minute)
				if err != nil {
					return emitError(cmd, 1, err.Error(), "No fallback, rerun, daemon restart or validation re-run was attempted; inspect the run before retrying")
				}
				emitDoc(cmd,
					toon.Field{Key: "run_id", Value: result.RunID},
					toon.Field{Key: "head_sha", Value: result.HeadSHA},
					toon.Field{Key: "pr_url", Value: result.PRURL},
					toon.Field{Key: "recorded_steps", Value: result.Steps},
					toon.Field{Key: "help", Value: []string{
						"Recorded validation evidence was republished verbatim; no step was re-run or waived.",
						"This does not establish CI readiness or merge authority; ordinary CI monitoring still applies.",
					}},
				)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&runID, "run", "", "exact terminal run ID (required)")
	cmd.Flags().StringVar(&head, "head", "", "run's exact recorded full head SHA (required)")
	return cmd
}
