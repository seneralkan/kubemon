package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
)

// configFlags are the kubectl connection flags: --kubeconfig, --context, -n, ...
var configFlags = genericclioptions.NewConfigFlags(true)

var rootCmd = &cobra.Command{
	Use:   "kubemon",
	Short: "A command line tool for monitoring kubernetes resources while deploying releases",
	Long: `kubemon watches a release as it happens: rollout progress, pods grouped by
revision, the events behind them and the logs of the new pods, in one view.
Run it interactively, or in CI with --wait to block until the release finishes.`,
	SilenceUsage:  true,
	SilenceErrors: true,
}

// exitError carries a process exit code out of a command.
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

// Execute runs the root command and exits with its code.
func Execute() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := rootCmd.ExecuteContext(ctx)
	stop()
	if err == nil {
		return
	}
	var ee *exitError
	if errors.As(err, &ee) {
		if ee.msg != "" {
			fmt.Fprintln(os.Stderr, ee.msg)
		}
		os.Exit(ee.code)
	}
	fmt.Fprintln(os.Stderr, "Error:", err)
	os.Exit(3)
}

func init() {
	configFlags.AddFlags(rootCmd.PersistentFlags())
}
