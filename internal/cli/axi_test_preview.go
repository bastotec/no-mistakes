package cli

import (
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/spf13/cobra"
	toon "github.com/toon-format/toon-go"
)

func newAxiTestPreviewCmd() *cobra.Command {
	var runID, head string
	cmd := &cobra.Command{
		Use: "publish-test-preview", Short: "Publish an exact parked-Test head as a managed draft PR for PR validation",
		Long: "Publish a run-owned github.com draft PR while Test remains parked and gates unchanged.\nRequires explicit --run and full --head from axi status; never resolves the latest run.\nThe PR gets the pr-validation label for repository-owned preview automation.\nReports only successful preview deployments for that exact head; an empty preview_url means pending or not configured.\nReinvoke with the same evidence to reuse the same PR and discover the deployment.\nOnly the normal PR step after Test can ready this draft (unless configured to remain draft).\nBoth CLI and daemon must support this operation; an older daemon refuses with no fallback or restart.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackAxiSurface("axi-publish-test-preview", "/axi/publish-test-preview", nil, func() error {
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
				var result ipc.PublishTestPreviewResult
				err = env.client.CallWithContext(cmd.Context(), ipc.MethodPublishTestPreview, ipc.PublishTestPreviewParams{RepoID: env.repo.ID, RunID: runID, HeadSHA: head}, &result, 3*time.Minute)
				if err != nil {
					return emitError(cmd, 1, err.Error(), "No fallback, gate approval or daemon restart was attempted; inspect the run before retrying")
				}
				emitDoc(cmd, toon.Field{Key: "run_id", Value: result.RunID}, toon.Field{Key: "head_sha", Value: result.HeadSHA}, toon.Field{Key: "pr_url", Value: result.PRURL}, toon.Field{Key: "preview_url", Value: result.PreviewURL}, toon.Field{Key: "label", Value: result.Label}, toon.Field{Key: "test_status", Value: result.TestStatus}, toon.Field{Key: "help", Value: []string{"Test remains parked; validate the exact-head preview, then respond to the original Test gate. No Test approval or merge readiness is implied."}})
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&runID, "run", "", "exact active run ID (required)")
	cmd.Flags().StringVar(&head, "head", "", "exact current full head SHA (required)")
	return cmd
}
