// Package app wires the watcher and the rollout source into a stream of snapshots.
package app

import (
	"context"
	"strings"
	"time"

	"github.com/seneralkan/kubemon/internal/kube"
	"github.com/seneralkan/kubemon/internal/model"
	"github.com/seneralkan/kubemon/internal/rollout"
	"github.com/seneralkan/kubemon/internal/watch"
)

// Config selects what to stream.
type Config struct {
	Clients     *kube.Clients
	Namespace   string
	Selector    string         // used when Source is nil
	Source      rollout.Source // optional: Deployment / Argo Rollout target
	EventsSince time.Time      // ignore events older than this
	EventLimit  int            // keep at most this many events per snapshot (0 = 100)
	Debounce    time.Duration  // coalesce bursts of changes (0 = 150ms)
}

// Stream starts watching and returns a latest-wins channel of snapshots. The
// channel is closed when ctx is done. Returns after all caches are synced.
func Stream(ctx context.Context, cfg Config) (<-chan model.Snapshot, error) {
	if cfg.EventLimit == 0 {
		cfg.EventLimit = 100
	}
	if cfg.Debounce == 0 {
		cfg.Debounce = 150 * time.Millisecond
	}
	target := model.Target{Namespace: cfg.Namespace, Selector: cfg.Selector}
	if cfg.Source != nil {
		target = cfg.Source.Target()
	}

	changed := make(chan struct{}, 1)
	notify := func() {
		select {
		case changed <- struct{}{}:
		default:
		}
	}

	// The target itself and objects it creates by name (AnalysisRuns, Experiments).
	targetRelated := func(kind, name string) bool {
		if target.Name == "" {
			return false
		}
		if name == target.Name {
			return true
		}
		return (kind == "AnalysisRun" || kind == "Experiment") && strings.HasPrefix(name, target.Name+"-")
	}
	w := watch.New(cfg.Clients.Kube, target.Namespace, target.Selector, targetRelated)
	if err := w.Start(ctx, notify); err != nil {
		return nil, err
	}
	if cfg.Source != nil {
		if err := cfg.Source.Start(ctx, notify); err != nil {
			return nil, err
		}
	}

	out := make(chan model.Snapshot, 1)
	go func() {
		defer close(out)
		seen := map[string]bool{} // object names ever owned by the target, so events of deleted pods stay visible
		related := func(kind, name string) bool { return seen[name] || targetRelated(kind, name) }
		notify() // initial snapshot
		for {
			select {
			case <-ctx.Done():
				return
			case <-changed:
			}
			select { // let a burst settle
			case <-ctx.Done():
				return
			case <-time.After(cfg.Debounce):
			}

			snap := model.Snapshot{Time: time.Now(), Target: target, Pods: w.Pods()}
			rs := w.ReplicaSets()
			for _, p := range snap.Pods {
				seen[p.Name] = true
			}
			for _, r := range rs {
				seen[r.Name] = true
			}
			if cfg.Source != nil {
				snap.Rollout, snap.Revisions = cfg.Source.Status(rs, snap.Pods)
			} else {
				snap.Revisions = rollout.GroupRevisions(rs, snap.Pods)
			}
			snap.Events = w.Events(cfg.EventsSince, related, cfg.EventLimit)
			Send(out, snap)
		}
	}()
	return out, nil
}

// Send delivers snap, replacing an unread older snapshot. Single producer only.
func Send(out chan model.Snapshot, snap model.Snapshot) {
	select {
	case out <- snap:
		return
	default:
	}
	select {
	case <-out:
	default:
	}
	out <- snap
}
