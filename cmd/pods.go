package cmd

import (
	"github.com/spf13/cobra"
)

func newPodsCmd() *cobra.Command {
	var o runOptions
	var selector string
	c := &cobra.Command{
		Use:   "pods -l SELECTOR",
		Short: "Watch pods matching a label selector, grouped by revision, with their events",
		Long: `Watch pods matching a label selector, grouped by revision, with their events.

With --wait it exits once the pods have settled: one revision only, all
healthy, none terminating. There is no rollout object to report failure or to
say a new revision is coming, so prefer "kubemon rollout NAME --wait" in CI.`,
		Example: `  kubemon pods -n payments -l app=checkout
  kubemon pods -l app=checkout --logs
  kubemon pods -l app=checkout -o plain --wait --timeout 5m`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := o.validate(); err != nil {
				return err
			}
			c, err := newClients()
			if err != nil {
				return err
			}
			ctx, cancel := o.start(cmd.Context())
			defer cancel()
			return run(ctx, c, nil, selector, o)
		},
	}
	c.Flags().StringVarP(&selector, "selector", "l", "", "label selector, e.g. app=checkout (required)")
	_ = c.MarkFlagRequired("selector")
	addRunFlags(c, &o)
	return c
}

func init() { rootCmd.AddCommand(newPodsCmd()) }
