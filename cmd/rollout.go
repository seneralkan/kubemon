package cmd

import (
	"github.com/spf13/cobra"

	"github.com/seneralkan/kubemon/internal/rollout"
)

func newRolloutCmd() *cobra.Command {
	var o runOptions
	c := &cobra.Command{
		Use:     "rollout [deploy/|rollout/]NAME",
		Aliases: []string{"release", "ro"},
		Short:   "Watch a Deployment or Argo Rollout release: progress, revisions, pods, events, logs",
		Example: `  kubemon rollout checkout                 # Argo Rollout or Deployment named checkout
  kubemon rollout deploy/checkout --logs   # follow logs of the new revision's pods
  kubemon rollout checkout --logs=all -c app
  kubemon rollout rollout/checkout -o plain --wait --timeout 20m   # CI: exit 0/1/2`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := o.validate(); err != nil {
				return err
			}
			c, err := newClients()
			if err != nil {
				return err
			}
			ctx, cancel := o.start(cmd.Context())
			defer cancel()
			src, err := rollout.Resolve(ctx, c, c.Namespace, args[0])
			if err != nil {
				return o.stopped(ctx, err)
			}
			return run(ctx, c, src, "", o)
		},
	}
	addRunFlags(c, &o)
	return c
}

func init() { rootCmd.AddCommand(newRolloutCmd()) }
