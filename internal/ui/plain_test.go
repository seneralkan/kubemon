package ui

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/seneralkan/kubemon/internal/logs"
	"github.com/seneralkan/kubemon/internal/model"
)

func TestPlainPrintsOnlyChanges(t *testing.T) {
	var buf bytes.Buffer
	p := newPlain(&buf, func() time.Time { return at(40) })

	p.snapshot(canarySnapshot(0))

	s := canarySnapshot(10) // new pod becomes ready; the Scheduled event repeats unchanged
	s.Pods[0] = runningPod("checkout-abc12-x", "abc12")
	s.Revisions[0].Ready = 1
	p.snapshot(s)

	s = canarySnapshot(20) // new pod crashes, rollout pauses
	s.Pods[0] = crashingPod("checkout-abc12-x", "abc12", 1, at(18))
	s.Rollout.Paused = true
	backoff := model.Event{Time: at(19), Type: "Warning", Reason: "BackOff", Message: "Back-off restarting failed container", Object: "Pod/checkout-abc12-x", Count: 1}
	s.Events = append(s.Events, backoff)
	p.snapshot(s)

	p.log(logs.Line{Time: at(21), Pod: "checkout-abc12-x", Container: "app", Text: "panic: out of memory", Previous: true})
	p.log(logs.Line{Time: at(22), Pod: "checkout-abc12-x", Container: "app", Text: "starting"})

	s = canarySnapshot(30) // warning repeats, a stable pod goes away, rollout degrades
	s.Pods = []model.Pod{crashingPod("checkout-abc12-x", "abc12", 1, at(18)), runningPod("checkout-def34-a", "def34")}
	s.Rollout.Phase, s.Rollout.Message = model.PhaseDegraded, "ProgressDeadlineExceeded"
	backoff.Count = 3
	s.Events = append(s.Events, backoff)
	p.snapshot(s)

	p.final()

	want := `10:00:00 Rollout shop/checkout · Canary · Progressing · step 3/8 · setWeight: 40 · weight 40%
10:00:00 revision canary abc12 #12 checkout:v2 ready 0/1
10:00:00 revision stable def34 #11 checkout:v1 ready 2/2
10:00:00 pod checkout-abc12-x [abc12] ContainerCreating 0/1 ready, 0 restarts — app: ContainerCreating
10:00:00 pod checkout-def34-a [def34] Running 1/1 ready, 0 restarts, node-1
10:00:00 pod checkout-def34-b [def34] Running 1/1 ready, 0 restarts, node-1
09:59:55 event Pod/checkout-abc12-x Scheduled: assigned to node-1
10:00:10 revision abc12 ready 0/1 → 1/1
10:00:10 pod checkout-abc12-x ContainerCreating → Running · ready 0/1 → 1/1
10:00:20 rollout paused
10:00:20 revision abc12 ready 1/1 → 0/1
10:00:20 pod checkout-abc12-x Running → CrashLoopBackOff · ready 1/1 → 0/1 · restarted (OOMKilled, exit 137), 1 total — app: CrashLoopBackOff (last: OOMKilled, exit 137)
10:00:19 WARN Pod/checkout-abc12-x BackOff: Back-off restarting failed container
10:00:21 [checkout-abc12-x/app] (previous) panic: out of memory
10:00:22 [checkout-abc12-x/app] starting
10:00:30 rollout phase Progressing → Degraded · resumed · message: ProgressDeadlineExceeded
10:00:30 pod checkout-def34-b deleted
10:00:19 WARN Pod/checkout-abc12-x BackOff: Back-off restarting failed container (x3)
10:00:40 final Rollout shop/checkout: Degraded · 2/3 ready · 2 pods · 1 restarts · 1 warnings
`
	if got := buf.String(); got != want {
		t.Errorf("output mismatch\n--- got\n%s--- want\n%s", got, want)
	}
}

func TestPlainDrainsPendingSnapshotOnCancel(t *testing.T) {
	snaps := make(chan model.Snapshot, 1)
	lines := make(chan logs.Line, 1) // never closed, like a log follower
	snaps <- canarySnapshot(0)
	lines <- logs.Line{Time: at(1), Pod: "checkout-abc12-x", Container: "app", Text: "last words"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var buf bytes.Buffer
	if err := Run(ctx, Options{Mode: ModePlain, Out: &buf}, Input{Snapshots: snaps, Logs: lines}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"Rollout shop/checkout · Canary", "[checkout-abc12-x/app] last words", "final Rollout shop/checkout: Progressing · 2/3 ready"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Index(out, "last words") > strings.Index(out, "Rollout shop/checkout · Canary") {
		t.Errorf("buffered logs should be flushed before the final snapshot:\n%s", out)
	}
}

func TestPlainSummaryWhenChannelClosesEmpty(t *testing.T) {
	snaps := make(chan model.Snapshot)
	close(snaps)
	var buf bytes.Buffer
	if err := Run(context.Background(), Options{Mode: ModePlain, Out: &buf}, Input{Snapshots: snaps}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "final: no data received") {
		t.Errorf("got %q", buf.String())
	}
}

func TestPlainSanitizesLogText(t *testing.T) {
	var buf bytes.Buffer
	p := newPlain(&buf, func() time.Time { return at(0) })
	p.log(logs.Line{Time: at(1), Pod: "p", Container: "app", Text: "\x1b[31merr\x1b[0m\r done\x1b]0;t\x07"})
	p.log(logs.Line{Time: at(2), Pod: "p", Container: "app", Text: "logs unavailable: forbidden", Err: true})
	if got, want := buf.String(), "10:00:01 [p/app] err done\n10:00:02 WARN [p/app] logs unavailable: forbidden\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
