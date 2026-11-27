package cmd

import (
	"time"

	"github.com/spf13/cobra"

	"github.com/seneralkan/kubemon/internal/demo"
	"github.com/seneralkan/kubemon/internal/ui"
)

func newDemoCmd() *cobra.Command {
	var o runOptions
	var fail bool
	var step time.Duration
	c := &cobra.Command{
		Use:   "demo",
		Short: "Play a simulated Argo Rollouts canary release to see the UI (no cluster needed)",
		Long: `Play a scripted canary release of a "checkout" Rollout through the real UI:
new pods starting, a failing readiness probe, a pod that is OOMKilled and goes
into CrashLoopBackOff, events and logs, analysis, and promotion to 100%.
With --fail the analysis fails and the rollout is aborted.`,
		Example: `  kubemon demo
  kubemon demo --fail
  kubemon demo -o plain --step 300ms`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := o.validate(); err != nil {
				return err
			}
			mode := ui.Mode(o.output)
			snaps, lines := demo.Play(cmd.Context(), demo.Options{Fail: fail, Step: step, KeepOpen: mode == ui.ModeTUI})
			return ui.Run(cmd.Context(), ui.Options{Mode: mode, Out: cmd.OutOrStdout(), ShowLogs: true},
				ui.Input{Snapshots: snaps, Logs: lines})
		},
	}
	c.Flags().StringVarP(&o.output, "output", "o", "", "tui, plain or json (default: tui on a terminal, plain otherwise)")
	c.Flags().BoolVar(&fail, "fail", false, "the analysis fails and the rollout is aborted")
	c.Flags().DurationVar(&step, "step", 1500*time.Millisecond, "time between steps of the script")
	o.logs = logsNone
	return c
}

func init() { rootCmd.AddCommand(newDemoCmd()) }
