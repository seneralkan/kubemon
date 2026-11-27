package cmd

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/seneralkan/kubemon/internal/model"
)

func healthy(name, hash string) model.Pod {
	return model.Pod{Name: name, Hash: hash, Phase: "Running", Ready: 1, Total: 1}
}

func TestFinishedStateBareSelector(t *testing.T) {
	rev := func(hash string, desired, ready int32) []model.Revision {
		return []model.Revision{{Hash: hash, Desired: desired, Ready: ready}}
	}
	notReady := healthy("b", "new")
	notReady.Ready = 0
	deleting := healthy("old-1", "new")
	deleting.Deleting = true
	job := model.Pod{Name: "migrate", Hash: "", Phase: "Succeeded", Total: 1}

	tests := []struct {
		name string
		snap model.Snapshot
		done bool
	}{
		{"no pods", model.Snapshot{}, false},
		{"one healthy revision at desired", model.Snapshot{Pods: []model.Pod{healthy("a", "new"), healthy("b", "new")}, Revisions: rev("new", 2, 2)}, true},
		{"mid rollout: old and new pods all healthy", model.Snapshot{Pods: []model.Pod{healthy("a", "new"), healthy("o", "old")}, Revisions: rev("new", 3, 1)}, false},
		{"new revision below desired", model.Snapshot{Pods: []model.Pod{healthy("a", "new")}, Revisions: rev("new", 3, 1)}, false},
		{"pod not ready", model.Snapshot{Pods: []model.Pod{healthy("a", "new"), notReady}, Revisions: rev("new", 2, 1)}, false},
		{"terminating pod", model.Snapshot{Pods: []model.Pod{healthy("a", "new"), deleting}, Revisions: rev("new", 1, 1)}, false},
		{"completed job pod ignored", model.Snapshot{Pods: []model.Pod{healthy("a", "new"), job}, Revisions: rev("new", 1, 1)}, true},
		{"pods of an older revision only", model.Snapshot{Pods: []model.Pod{healthy("o", "old")}, Revisions: rev("new", 1, 0)}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			done, failed := finishedState(tt.snap)
			if done != tt.done || failed {
				t.Errorf("done=%v failed=%v, want done=%v failed=false", done, failed, tt.done)
			}
		})
	}
}

func TestFinishedStateRollout(t *testing.T) {
	for phase, want := range map[string][2]bool{
		model.PhaseHealthy:     {true, false},
		model.PhaseProgressing: {false, false},
		model.PhasePaused:      {false, false},
		model.PhaseDegraded:    {false, true},
		model.PhaseAborted:     {false, true},
	} {
		done, failed := finishedState(model.Snapshot{Rollout: &model.RolloutStatus{Phase: phase}})
		if [2]bool{done, failed} != want {
			t.Errorf("%s: done=%v failed=%v, want %v", phase, done, failed, want)
		}
	}
}

func TestLogPods(t *testing.T) {
	pods := []model.Pod{healthy("n1", "new"), healthy("o1", "old"), healthy("n2", "new")}
	names := func(ps []model.Pod) string { return fmt.Sprint(len(ps), ps) }
	tests := []struct {
		name string
		snap model.Snapshot
		mode string
		want []string
	}{
		{"all", model.Snapshot{Pods: pods}, logsAll, []string{"n1", "o1", "n2"}},
		{"new via rollout hash", model.Snapshot{Pods: pods, Rollout: &model.RolloutStatus{CurrentHash: "new"}}, logsNew, []string{"n1", "n2"}},
		{"new revision not known yet", model.Snapshot{Pods: pods, Rollout: &model.RolloutStatus{}}, logsNew, nil},
		{"new via newest revision", model.Snapshot{Pods: pods, Revisions: []model.Revision{{Hash: "old"}}}, logsNew, []string{"o1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := logPods(tt.snap, tt.mode)
			if len(got) != len(tt.want) {
				t.Fatalf("got %s, want %v", names(got), tt.want)
			}
			for i, p := range got {
				if p.Name != tt.want[i] {
					t.Fatalf("got %s, want %v", names(got), tt.want)
				}
			}
		})
	}

	many := make([]model.Pod, 50)
	for i := range many {
		many[i] = healthy(fmt.Sprint(i), "h")
	}
	if got := logPods(model.Snapshot{Pods: many}, logsAll); len(got) != maxLogPods || got[0].Name != "0" {
		t.Errorf("cap: got %d pods starting %q, want %d newest", len(got), got[0].Name, maxLogPods)
	}
}

func TestStopped(t *testing.T) {
	boom := errors.New("boom")
	live := context.Background()
	timedOut, cancel := context.WithTimeout(live, 0)
	defer cancel()
	interrupted, stop := context.WithCancel(live)
	stop()

	code := func(err error) int {
		var ee *exitError
		if errors.As(err, &ee) {
			return ee.code
		}
		if err != nil {
			return 3
		}
		return 0
	}
	tests := []struct {
		name string
		wait bool
		ctx  context.Context
		err  error
		want int
	}{
		{"real error", true, live, boom, 3},
		{"timeout during startup", true, timedOut, boom, exitTimeout},
		{"ctrl+c with --wait", true, interrupted, boom, exitTimeout},
		{"ctrl+c without --wait", false, interrupted, boom, 0},
		{"no error", true, live, nil, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := runOptions{wait: tt.wait}
			if got := code(o.stopped(tt.ctx, tt.err)); got != tt.want {
				t.Errorf("exit = %d, want %d", got, tt.want)
			}
		})
	}
}
