package cli

import (
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/spf13/cobra"
	toon "github.com/toon-format/toon-go"
)

func newAxiMonitorStopCmd() *cobra.Command {
	var p db.MonitorDisposition
	cmd := &cobra.Command{
		Use: "monitor-stop", Short: "Freeze exact-run monitoring without completing validation or returning custody",
		Long: "Durably freeze post-green CI polling, or preserve an unfinished parked Review without responding. Requires every observed identity pin. No abort, repair cancellation, Git or forge edits occur. The branch remains pipeline-owned. Older daemons refuse this distinct RPC; a matching daemon must already be running.",
		Args: cobra.NoArgs, SilenceErrors: true, SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := paths.New()
			if err != nil {
				return emitError(cmd, 1, err.Error())
			}
			client, err := ipc.Dial(root.Socket())
			if err != nil {
				return emitError(cmd, 1, "monitor cessation unconfirmed: "+err.Error())
			}
			defer client.Close()
			var result ipc.StopMonitorResult
			err = client.CallWithContext(cmd.Context(), ipc.MethodStopMonitor, &p, &result, 125*time.Second)
			if err != nil {
				return emitError(cmd, 1, "monitor cessation unconfirmed: "+err.Error())
			}
			if !result.MonitorStopped {
				return emitError(cmd, 1, "monitor cessation unconfirmed: daemon did not acknowledge")
			}
			emitDoc(cmd, toon.Field{Key: "monitor_stopped", Value: true}, toon.Field{Key: "run", Value: p.RunID}, toon.Field{Key: "validation_finished", Value: false}, toon.Field{Key: "custody_returned", Value: false}, toon.Field{Key: "disposition", Value: p.State})
			return nil
		},
	}
	cmd.Flags().StringVar(&p.RunID, "run", "", "exact run id")
	cmd.Flags().StringVar(&p.RepoID, "repo-id", "", "exact registered repository id")
	cmd.Flags().StringVar(&p.PRURL, "pr-url", "", "exact recorded publication URL")
	cmd.Flags().StringVar(&p.PublishedHead, "published-head", "", "exact last-published full SHA")
	cmd.Flags().StringVar(&p.ObservedHead, "observed-head", "", "exact current pipeline full SHA")
	cmd.Flags().StringVar(&p.State, "state", "", "observed polling or parked-review state")
	for _, flag := range []string{"run", "repo-id", "pr-url", "published-head", "observed-head", "state"} {
		_ = cmd.MarkFlagRequired(flag)
	}
	return cmd
}
