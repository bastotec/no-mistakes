package cli

import (
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/spf13/cobra"
	toon "github.com/toon-format/toon-go"
)

func newAxiRecoverPublicationCmd() *cobra.Command {
	var runID string
	cmd := &cobra.Command{
		Use:   "recover-publication --run <id>",
		Short: "Retry only a terminal PR-content refusal using pinned validation evidence",
		Long: "Republish the same recorded head to the same pinned PR target after a terminal\n" +
			"PR-content refusal. Historical runs without prospective publication pins are\n" +
			"ineligible. This runs the ordinary final-content guard, never pushes the source branch,\n" +
			"never reruns validation, and grants no merge authority. An existing exact-head\n" +
			"PR is returned unchanged. Providers lacking complete PR identity reads refuse.\n" +
			"Forge CI runs normally on publication; this operation does not waive checks.",
		Args: cobra.NoArgs, SilenceErrors: true, SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(runID) == "" {
				return emitError(cmd, 2, "--run is required")
			}
			return trackAxiSurface("axi-recover-publication", "/axi/recover-publication", nil, func() error {
				// Never start/restart an older service to acquire a new capability.
				env, err := openAxiEnv(false)
				if err != nil {
					return emitError(cmd, 1, err.Error())
				}
				defer env.close()
				if env.repo == nil {
					return emitError(cmd, 2, "repository is not initialized")
				}
				client, err := ipc.Dial(env.p.Socket())
				if err != nil {
					return emitError(cmd, 1, err.Error())
				}
				defer client.Close()
				var result ipc.RecoverPublicationResult
				if err := client.Call(ipc.MethodRecoverPublication, &ipc.RecoverPublicationParams{RepoID: env.repo.ID, RunID: runID}, &result); err != nil {
					return emitError(cmd, 2, fmt.Sprintf("recover publication: %v", err))
				}
				emitDoc(cmd, toon.Field{Key: "run_id", Value: result.RunID}, toon.Field{Key: "pr_url", Value: result.PRURL}, toon.Field{Key: "validation", Value: "recorded evidence reused; no new validation or merge authority"})
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&runID, "run", "", "specific terminal publication-failed run identity")
	return cmd
}
