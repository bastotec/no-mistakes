package cli

import (
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/spf13/cobra"
	toon "github.com/toon-format/toon-go"
)

func newAxiRefreshPublicationCmd() *cobra.Command {
	var run, head, pr string
	cmd := &cobra.Command{
		Use:   "refresh-publication",
		Short: "Append evidence-bound current-head repair disposition during CI monitoring",
		Long:  "Refresh an active run's existing PR publication between CI polls, without replaying validation. Requires explicit --run, --pr and full --head matching the recorded, last-pushed, remote source and live PR head. Historical findings, risk, statuses, skips and timeouts are preserved. Live CI evidence is separately attributed; it never certifies a review finding fixed or establishes live product validation, relabels the PR, or grants merge authority. Refuses repairs, approval gates, terminal runs, unsupported identity/content readers and binding mismatches. Older daemons refuse without fallback or restart.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackAxiSurface("axi-refresh-publication", "/axi/refresh-publication", nil, func() error {
				if run == "" || pr == "" {
					return emitError(cmd, 2, "--run and --pr are required")
				}
				if err := steps.ValidateHistoryBase(head); err != nil {
					return emitError(cmd, 2, "--head requires a full commit SHA")
				}
				env, err := openAxiDaemonEnv()
				if err != nil {
					return emitError(cmd, 1, err.Error())
				}
				defer env.close()
				var result ipc.RefreshPublicationResult
				err = env.client.CallWithContext(cmd.Context(), ipc.MethodRefreshPublication, ipc.RefreshPublicationParams{RepoID: env.repo.ID, RunID: run, HeadSHA: head, PRURL: pr}, &result, 3*time.Minute)
				if err != nil {
					return emitError(cmd, 1, err.Error(), "No fallback or validation replay was attempted; inspect status before retrying")
				}
				emitDoc(cmd, toon.Field{Key: "run_id", Value: result.RunID}, toon.Field{Key: "head_sha", Value: result.HeadSHA}, toon.Field{Key: "pr_url", Value: result.PRURL}, toon.Field{Key: "disposition", Value: result.Disposition})
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&run, "run", "", "exact active run ID (required)")
	cmd.Flags().StringVar(&head, "head", "", "exact recorded and pushed full SHA (required)")
	cmd.Flags().StringVar(&pr, "pr", "", "exact recorded PR URL (required)")
	return cmd
}
