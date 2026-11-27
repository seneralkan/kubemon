package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	"k8s.io/klog/v2"

	"github.com/seneralkan/kubemon/internal/app"
	"github.com/seneralkan/kubemon/internal/kube"
	"github.com/seneralkan/kubemon/internal/logs"
	"github.com/seneralkan/kubemon/internal/model"
	"github.com/seneralkan/kubemon/internal/rollout"
	"github.com/seneralkan/kubemon/internal/ui"
)

// Exit codes.
const (
	exitFailed  = 1 // release degraded or aborted
	exitTimeout = 2 // --wait timed out
)

// Log modes.
const (
	logsNone = "none"
	logsNew  = "new"
	logsAll  = "all"
)

type runOptions struct {
	output      string
	logs        string
	container   string
	tail        int64
	wait        bool
	timeout     time.Duration
	eventsSince time.Duration
}

func addRunFlags(c *cobra.Command, o *runOptions) {
	f := c.Flags()
	f.StringVarP(&o.output, "output", "o", "", "tui, plain or json (default: tui on a terminal, plain otherwise)")
	f.StringVar(&o.logs, "logs", logsNone, "follow container logs: --logs (= new, newest revision only) or --logs=all; at most 20 pods")
	f.Lookup("logs").NoOptDefVal = logsNew
	f.StringVarP(&o.container, "container", "c", "", "only follow logs of this container")
	f.Int64Var(&o.tail, "tail", 20, "log lines to show when a container's stream starts (-1 = all)")
	f.BoolVarP(&o.wait, "wait", "w", false, "exit when the release finishes: 0 healthy, 1 failed/aborted, 2 timeout")
	f.DurationVar(&o.timeout, "timeout", 15*time.Minute, "give up waiting after this long (with --wait)")
	f.DurationVar(&o.eventsSince, "events-since", 10*time.Minute, "also show events up to this long before start")
}

func (o *runOptions) validate() error {
	switch o.logs {
	case logsNone, logsNew, logsAll:
	default:
		return fmt.Errorf("--logs must be none, new or all, got %q", o.logs)
	}
	if o.output == "" {
		o.output = string(ui.ModePlain)
		if term.IsTerminal(int(os.Stdout.Fd())) {
			o.output = string(ui.ModeTUI)
		}
	}
	switch ui.Mode(o.output) {
	case ui.ModeTUI, ui.ModePlain, ui.ModeJSON:
	default:
		return fmt.Errorf("--output must be tui, plain or json, got %q", o.output)
	}
	return nil
}

func newClients() (*kube.Clients, error) { return kube.New(configFlags) }

// start returns the context the whole command runs under: with --wait it ends
// at --timeout, so slow lookups and cache syncs count against it too.
func (o *runOptions) start(parent context.Context) (context.Context, context.CancelFunc) {
	if o.output == string(ui.ModeTUI) {
		// client-go logs watch errors to stderr, which would scribble over the TUI.
		klog.LogToStderr(false)
		klog.SetOutput(io.Discard)
	}
	if o.wait {
		return context.WithTimeout(parent, o.timeout)
	}
	return context.WithCancel(parent)
}

// stopped turns an error caused by the run context ending (timeout or Ctrl+C)
// into the matching exit code; other errors pass through.
func (o *runOptions) stopped(ctx context.Context, err error) error {
	switch {
	case err == nil || ctx.Err() == nil:
		return err
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return &exitError{code: exitTimeout, msg: fmt.Sprintf("timed out after %s waiting for the release", o.timeout)}
	case o.wait:
		return &exitError{code: exitTimeout, msg: "stopped before the release finished"}
	}
	return nil // interrupted without --wait: a normal way to quit
}

// run streams snapshots for src (or a bare selector when src is nil) into the UI.
// waitCtx comes from o.start.
func run(waitCtx context.Context, c *kube.Clients, src rollout.Source, selector string, o runOptions) error {
	ctx, cancel := context.WithCancel(waitCtx)
	defer cancel()

	snaps, err := app.Stream(ctx, app.Config{
		Clients:     c,
		Namespace:   c.Namespace,
		Selector:    selector,
		Source:      src,
		EventsSince: time.Now().Add(-o.eventsSince),
	})
	if err != nil {
		return o.stopped(waitCtx, err)
	}

	var follower *logs.Follower
	var lines <-chan logs.Line
	if o.logs != logsNone {
		follower = logs.NewFollower(c.Kube, c.Namespace, logs.Options{Container: o.container, TailLines: o.tail})
		lines = follower.Lines()
	}

	toUI := make(chan model.Snapshot, 1)
	var outcome int // 0 running/healthy, exitFailed
	finished := false
	teeDone := make(chan struct{})
	go func() {
		defer close(teeDone)
		defer close(toUI)
		for s := range snaps {
			if follower != nil {
				follower.Sync(ctx, logPods(s, o.logs))
			}
			app.Send(toUI, s)
			if o.wait {
				if done, failed := finishedState(s); done || failed {
					finished = true
					if failed {
						outcome = exitFailed
					}
					cancel()
					return
				}
			}
		}
	}()

	uiErr := ui.Run(ctx, ui.Options{Mode: ui.Mode(o.output), Out: os.Stdout, ShowLogs: follower != nil}, ui.Input{Snapshots: toUI, Logs: lines})
	cancel()
	<-teeDone
	if uiErr != nil && !errors.Is(uiErr, context.Canceled) && !errors.Is(uiErr, context.DeadlineExceeded) {
		return uiErr
	}

	switch {
	case !o.wait:
		return nil
	case finished && outcome == exitFailed:
		return &exitError{code: exitFailed, msg: "release failed"}
	case finished:
		return nil
	case waitCtx.Err() != nil:
		return o.stopped(waitCtx, waitCtx.Err())
	default: // user quit the TUI
		return &exitError{code: exitTimeout, msg: "stopped before the release finished"}
	}
}

// finishedState reports whether a snapshot is terminal. With a rollout target
// that is its phase. A bare selector has no rollout object to ask, so done
// means settled: pods of a single revision only, none terminating, all healthy,
// and that revision at its desired count. Completed (Job) pods are ignored.
// It can never report failure.
func finishedState(s model.Snapshot) (done, failed bool) {
	if s.Rollout != nil {
		return s.Rollout.Done(), s.Rollout.Failed()
	}
	hashes := map[string]bool{}
	for _, p := range s.Pods {
		if p.Phase == "Succeeded" {
			continue
		}
		if !p.Healthy() {
			return false, false
		}
		hashes[p.Hash] = true
	}
	if len(hashes) != 1 {
		return false, false
	}
	if len(s.Revisions) > 0 {
		r := s.Revisions[0]
		if !hashes[r.Hash] || r.Desired == 0 || r.Ready < r.Desired {
			return false, false
		}
	}
	return true, false
}

// maxLogPods caps concurrent log streams; pods are newest first.
const maxLogPods = 20

// logPods picks which pods to follow logs for.
func logPods(s model.Snapshot, mode string) []model.Pod {
	newest := ""
	if mode == logsNew {
		switch {
		case s.Rollout != nil:
			newest = s.Rollout.CurrentHash
			if newest == "" {
				return nil // the new revision is not known yet
			}
		case len(s.Revisions) > 0:
			newest = s.Revisions[0].Hash
		}
	}
	var out []model.Pod
	for _, p := range s.Pods {
		if len(out) == maxLogPods {
			break
		}
		if newest == "" || p.Hash == newest {
			out = append(out, p)
		}
	}
	return out
}
