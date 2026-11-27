package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// Set at build time: -ldflags "-X github.com/seneralkan/kubemon/cmd.version=v1.2.3 ..."
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func init() {
	rootCmd.Version = version
	rootCmd.SetVersionTemplate("kubemon {{.Version}}\n")
	rootCmd.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print the kubemon version",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintf(cmd.OutOrStdout(), "kubemon %s (commit %s, built %s)\n", version, commit, date)
		},
	})
}
