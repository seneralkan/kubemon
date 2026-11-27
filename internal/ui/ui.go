// Package ui renders snapshots and log lines: an interactive TUI, a plain
// line-oriented stream for CI, or JSON.
package ui

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/seneralkan/kubemon/internal/logs"
	"github.com/seneralkan/kubemon/internal/model"
)

// Mode selects the renderer.
type Mode string

// Renderer modes.
const (
	ModeTUI   Mode = "tui"
	ModePlain Mode = "plain"
	ModeJSON  Mode = "json"
)

// Options configure rendering.
type Options struct {
	Mode     Mode
	Out      io.Writer
	ShowLogs bool
}

// Input is what the renderer consumes. Snapshots are latest-wins: a slow
// renderer only ever sees the newest state. Logs may be nil.
type Input struct {
	Snapshots <-chan model.Snapshot
	Logs      <-chan logs.Line
}

// Run renders until ctx is done or Snapshots is closed (or the user quits the
// TUI). On ctx cancellation it must still render any snapshot already waiting
// in the channel, so the final state is shown before returning.
func Run(ctx context.Context, o Options, in Input) error {
	if o.Out == nil {
		o.Out = os.Stdout
	}
	switch o.Mode {
	case ModeTUI:
		return runTUI(ctx, o, in)
	case ModePlain, "":
		p := newPlain(o.Out, time.Now)
		pump(ctx, in, p.snapshot, eachLine(p.log), 0)
		p.final()
		return nil
	case ModeJSON:
		j := newJSON(o.Out)
		pump(ctx, in, j.snapshot, eachLine(j.log), 0)
		return nil
	default:
		return fmt.Errorf("ui: unknown mode %q", o.Mode)
	}
}

// maxDrain bounds how many already-buffered log lines are flushed on exit, so
// a producer that keeps writing cannot hold the renderer open.
const maxDrain = 1000

// maxBatch bounds how many log lines are delivered in one callback.
const maxBatch = 200

func eachLine(f func(logs.Line)) func([]logs.Line) {
	return func(ls []logs.Line) {
		for _, l := range ls {
			f(l)
		}
	}
}

// pump delivers snapshots and batches of log lines to the callbacks until ctx
// is done or Snapshots is closed. After a line arrives it waits up to linger
// for more (0: takes only what is already buffered), so a busy stream costs one
// callback per batch rather than per line. Before returning it flushes whatever
// is already waiting: buffered log lines first, then the pending snapshot, so
// the final state is the last thing rendered.
func pump(ctx context.Context, in Input, snap func(model.Snapshot), batch func([]logs.Line), linger time.Duration) {
	lines := in.Logs
	defer func() {
		for left := maxDrain; left > 0; {
			limit := min(maxBatch, left)
			b, open := collect(ctx, lines, nil, limit, 0)
			if len(b) > 0 {
				batch(b)
			}
			if !open || len(b) < limit {
				break
			}
			left -= len(b)
		}
		select {
		case s, ok := <-in.Snapshots:
			if ok {
				snap(s)
			}
		default:
		}
	}()
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
			return
		case s, ok := <-in.Snapshots:
			if !ok {
				return
			}
			snap(s)
		case l, ok := <-lines:
			if !ok {
				lines = nil
				continue
			}
			b, open := collect(ctx, lines, []logs.Line{l}, maxBatch, linger)
			batch(b)
			if !open {
				lines = nil
			}
		}
	}
}

// collect appends lines to batch until it holds limit lines, nothing more is
// buffered (linger 0), or linger has passed or ctx is done. open is false once
// the channel is closed.
func collect(ctx context.Context, lines <-chan logs.Line, batch []logs.Line, limit int, linger time.Duration) (_ []logs.Line, open bool) {
	var timeout <-chan time.Time
	if linger > 0 {
		t := time.NewTimer(linger)
		defer t.Stop()
		timeout = t.C
	}
	for len(batch) < limit {
		if timeout == nil {
			select {
			case l, ok := <-lines:
				if !ok {
					return batch, false
				}
				batch = append(batch, l)
				continue
			default:
				return batch, true
			}
		}
		select {
		case l, ok := <-lines:
			if !ok {
				return batch, false
			}
			batch = append(batch, l)
		case <-timeout:
			return batch, true
		case <-ctx.Done():
			return batch, true
		}
	}
	return batch, true
}
