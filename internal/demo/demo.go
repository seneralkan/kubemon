// Package demo plays a scripted Argo Rollouts canary release through the real
// renderers, so the UI can be seen without a cluster.
package demo

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/seneralkan/kubemon/internal/logs"
	"github.com/seneralkan/kubemon/internal/model"
)

// Options configure the playback.
type Options struct {
	Fail     bool          // the analysis fails and the rollout is aborted
	Step     time.Duration // time between script steps (0 = 1.5s)
	KeepOpen bool          // keep the snapshot channel open after the script ends (TUI)
}

const (
	ns          = "payments"
	app         = "checkout"
	stableHash  = "6d8f9c5b7"
	canaryHash  = "7b4c2e9d1"
	stableImage = "registry.example.com/payments/checkout:1.41.0"
	canaryImage = "registry.example.com/payments/checkout:1.42.0"
	replicas    = 4
)

var steps = []string{"setWeight: 20", "pause: 30s", "setWeight: 50", "analysis", "setWeight: 100"}

// Play starts the script and returns its snapshot and log channels.
func Play(ctx context.Context, o Options) (<-chan model.Snapshot, <-chan logs.Line) {
	if o.Step == 0 {
		o.Step = 1500 * time.Millisecond
	}
	snaps := make(chan model.Snapshot, 1)
	lines := make(chan logs.Line, 256)
	s := newSim(lines)
	go func() {
		if !o.KeepOpen {
			defer close(snaps)
		}
		script := s.script(o.Fail)
		for i, step := range script {
			step()
			select {
			case snaps <- s.snapshot():
			case <-ctx.Done():
				return
			}
			if i == len(script)-1 {
				break
			}
			select {
			case <-time.After(o.Step):
			case <-ctx.Done():
				return
			}
		}
		if o.KeepOpen {
			<-ctx.Done()
		}
	}()
	return snaps, lines
}

type sim struct {
	lines   chan<- logs.Line
	status  model.RolloutStatus
	desired map[string]int32 // per revision hash
	pods    map[string]*model.Pod
	events  []model.Event
	runName string
}

func newSim(lines chan<- logs.Line) *sim {
	s := &sim{
		lines:   lines,
		desired: map[string]int32{stableHash: replicas, canaryHash: 0},
		pods:    map[string]*model.Pod{},
		runName: fmt.Sprintf("%s-%s-8-3", app, canaryHash),
		status: model.RolloutStatus{
			Kind: "Rollout", Strategy: "Canary", Phase: model.PhaseProgressing,
			Generation: 8, ObservedGeneration: 8, Desired: replicas,
			TotalSteps: len(steps), StableHash: stableHash, CurrentHash: canaryHash,
			Message: "more replicas need to be updated",
		},
	}
	old := time.Now().Add(-73 * time.Hour)
	for i, id := range []string{"q7m2x", "h4n8v", "c9t5r", "z2k6w"} {
		p := s.addPod(stableHash, id, stableImage, old.Add(time.Duration(i)*time.Second))
		running(p, true)
		p.Node = fmt.Sprintf("ip-10-0-1-%d.eu-west-1.compute.internal", 20+i)
	}
	return s
}

// script returns the release, one step per entry.
func (s *sim) script(fail bool) []func() {
	k2p := podName(canaryHash, "k2p9q")
	w8x := podName(canaryHash, "w8x4n")
	m3d := podName(canaryHash, "m3d7f")
	b5j := podName(canaryHash, "b5j1s")

	script := []func(){
		func() {
			s.setStep(0)
			s.event("Normal", "RolloutUpdated", "Rollout/"+app, "Rollout updated to revision 8")
			s.scale(canaryHash, 1)
		},
		func() { s.newPod(canaryHash, "k2p9q", "ip-10-0-2-41") },
		func() {
			creating(s.pods[k2p])
			s.event("Normal", "Pulling", "Pod/"+k2p, `Pulling image "`+canaryImage+`"`)
		},
		func() {
			s.event("Normal", "Pulled", "Pod/"+k2p, `Successfully pulled image "`+canaryImage+`" in 3.218s (3.218s including waiting). Image size: 48213405 bytes.`)
			s.event("Normal", "Started", "Pod/"+k2p, "Started container app")
			running(s.pods[k2p], false)
			s.log(k2p, "starting checkout 1.42.0 (commit 3f9a1c2, go1.27.4)")
			s.log(k2p, "connecting to postgres at checkout-db.payments.svc:5432")
		},
		func() {
			s.event("Warning", "Unhealthy", "Pod/"+k2p, "Readiness probe failed: HTTP probe failed with statuscode: 503")
			s.log(k2p, "waiting for migration lock (held by checkout-6d8f9c5b7-q7m2x)")
		},
		func() {
			s.event("Warning", "Unhealthy", "Pod/"+k2p, "Readiness probe failed: HTTP probe failed with statuscode: 503")
			s.log(k2p, "migrations up to date (version 2026112701)")
			s.log(k2p, "listening on :8080")
		},
		func() {
			running(s.pods[k2p], true)
			s.status.Weight = 20
			s.scale(stableHash, 3)
			s.terminate(podName(stableHash, "z2k6w"))
			s.setStep(1)
			s.status.Paused, s.status.Phase, s.status.Message = true, model.PhasePaused, "CanaryPauseStep"
			s.log(k2p, `GET /healthz 200 1ms`)
		},
		func() {
			s.remove(podName(stableHash, "z2k6w"))
			s.log(k2p, `POST /v1/orders 201 38ms order_id=ord_8KD2`)
			s.log(k2p, `POST /v1/orders 201 41ms order_id=ord_8KD3`)
		},
		func() {
			s.status.Paused, s.status.Phase, s.status.Message = false, model.PhaseProgressing, "more replicas need to be updated"
			s.setStep(2)
			s.scale(canaryHash, 2)
			s.newPod(canaryHash, "w8x4n", "ip-10-0-3-17")
			creating(s.pods[w8x])
		},
		func() {
			running(s.pods[w8x], false)
			s.log(w8x, "starting checkout 1.42.0 (commit 3f9a1c2, go1.27.4)")
			s.log(w8x, "warming product catalog cache (2.1 GB)")
			s.log(k2p, `POST /v1/orders 201 36ms order_id=ord_8KD4`)
		},
		func() {
			oom(s.pods[w8x])
			s.event("Warning", "BackOff", "Pod/"+w8x, "Back-off restarting failed container app in pod "+w8x+"_"+ns)
		},
		func() {
			crashloop(s.pods[w8x])
			s.prevLog(w8x, "warming product catalog cache (2.1 GB)")
			s.prevLog(w8x, "fatal error: runtime: out of memory")
			s.prevLog(w8x, "goroutine 1 [running]: catalog.(*Cache).Warm(...) cache.go:212")
		},
		func() {
			running(s.pods[w8x], false)
			s.log(w8x, "starting checkout 1.42.0 (commit 3f9a1c2, go1.27.4)")
			s.log(w8x, "catalog cache warm-up capped at 512 MB (CACHE_MAX_BYTES)")
			s.log(w8x, "listening on :8080")
		},
		func() {
			running(s.pods[w8x], true)
			s.status.Weight = 50
			s.scale(stableHash, 2)
			s.terminate(podName(stableHash, "c9t5r"))
			s.setStep(3)
			s.status.Analysis = []model.Analysis{{Name: s.runName, Phase: "Running", Metrics: []model.AnalysisMetric{
				{Name: "success-rate", Phase: "Running", Value: "99.1%", Successful: 1},
				{Name: "latency-p99", Phase: "Running", Value: "182ms", Successful: 1},
			}}}
			s.event("Normal", "AnalysisRunRunning", "Rollout/"+app, "Step Analysis Run '"+s.runName+"' Status New: '' Previous: ''")
		},
	}

	if fail {
		return append(script,
			func() {
				s.remove(podName(stableHash, "c9t5r"))
				s.status.Analysis[0].Metrics[0] = model.AnalysisMetric{Name: "success-rate", Phase: "Failed", Value: "91.2%", Successful: 1, Failed: 2}
				s.log(w8x, `POST /v1/orders 502 3012ms error="payment-gateway: context deadline exceeded"`)
				s.log(k2p, `POST /v1/orders 502 3008ms error="payment-gateway: context deadline exceeded"`)
			},
			func() {
				a := &s.status.Analysis[0]
				a.Phase, a.Message = "Failed", `Metric "success-rate" assessed Failed due to failed (2) > failureLimit (1)`
				s.status.Phase, s.status.Weight = model.PhaseAborted, 0
				s.status.Message = "RolloutAborted: Rollout aborted update to revision 8: " + a.Message
				s.event("Warning", "AnalysisRunFailed", "Rollout/"+app, "Analysis Run '"+s.runName+"' Status New: 'Failed' Previous: 'Running'")
				s.event("Warning", "RolloutAborted", "Rollout/"+app, "Rollout aborted update to revision 8")
				s.scale(canaryHash, 0)
				s.scale(stableHash, replicas)
				s.terminate(k2p)
				s.terminate(w8x)
				s.newPod(stableHash, "r1v8p", "ip-10-0-1-22")
				s.newPod(stableHash, "f6s3a", "ip-10-0-1-23")
			},
			func() {
				s.remove(k2p)
				s.remove(w8x)
				running(s.pods[podName(stableHash, "r1v8p")], true)
				running(s.pods[podName(stableHash, "f6s3a")], true)
			},
		)
	}

	return append(script,
		func() {
			s.remove(podName(stableHash, "c9t5r"))
			s.status.Analysis[0].Metrics = []model.AnalysisMetric{
				{Name: "success-rate", Phase: "Running", Value: "99.4%", Successful: 2},
				{Name: "latency-p99", Phase: "Running", Value: "176ms", Successful: 2},
			}
			s.log(w8x, `POST /v1/orders 201 35ms order_id=ord_8KD9`)
		},
		func() {
			a := &s.status.Analysis[0]
			a.Phase = "Successful"
			for i := range a.Metrics {
				a.Metrics[i].Phase = "Successful"
			}
			s.event("Normal", "AnalysisRunSuccessful", "Rollout/"+app, "Analysis Run '"+s.runName+"' Status New: 'Successful' Previous: 'Running'")
			s.setStep(4)
			s.scale(canaryHash, replicas)
			s.newPod(canaryHash, "m3d7f", "ip-10-0-2-44")
			s.newPod(canaryHash, "b5j1s", "ip-10-0-3-19")
			creating(s.pods[m3d])
			creating(s.pods[b5j])
		},
		func() {
			running(s.pods[m3d], true)
			running(s.pods[b5j], true)
			s.log(m3d, "listening on :8080")
			s.log(b5j, "listening on :8080")
			s.status.Weight = 100
			s.setStep(5)
			s.scale(stableHash, 0)
			s.terminate(podName(stableHash, "q7m2x"))
			s.terminate(podName(stableHash, "h4n8v"))
		},
		func() {
			s.remove(podName(stableHash, "q7m2x"))
			s.remove(podName(stableHash, "h4n8v"))
			s.status.Phase, s.status.Message, s.status.StableHash = model.PhaseHealthy, "", canaryHash
			s.event("Normal", "RolloutCompleted", "Rollout/"+app, "Rollout completed update to revision 8 ("+canaryHash+"): Completed all 5 canary steps")
		},
	)
}

func podName(hash, id string) string { return app + "-" + hash + "-" + id }

func (s *sim) setStep(i int) {
	s.status.CurrentStep = i
	s.status.StepDesc = ""
	if i < len(steps) {
		s.status.StepDesc = steps[i]
	}
}

func (s *sim) scale(hash string, n int32) {
	if s.desired[hash] == n {
		return
	}
	verb := "up"
	if n < s.desired[hash] {
		verb = "down"
	}
	s.desired[hash] = n
	s.event("Normal", "ScalingReplicaSet", "Rollout/"+app, fmt.Sprintf("Scaled %s ReplicaSet %s-%s (revision %d) to %d", verb, app, hash, revision(hash), n))
}

func revision(hash string) int64 {
	if hash == canaryHash {
		return 8
	}
	return 7
}

func (s *sim) addPod(hash, id, image string, created time.Time) *model.Pod {
	p := &model.Pod{
		Name: podName(hash, id), Namespace: ns, Hash: hash, Created: created, Total: 1,
		Phase: "Pending", Status: "Pending",
		Containers: []model.Container{{Name: "app", Image: image, State: "Waiting"}},
	}
	s.pods[p.Name] = p
	return p
}

func (s *sim) newPod(hash, id, node string) {
	image := stableImage
	if hash == canaryHash {
		image = canaryImage
	}
	p := s.addPod(hash, id, image, time.Now())
	p.Node, p.Scheduled = node, time.Now()
	s.event("Normal", "Scheduled", "Pod/"+p.Name, "Successfully assigned "+ns+"/"+p.Name+" to "+node)
}

func creating(p *model.Pod) {
	p.Status = "ContainerCreating"
	p.Containers[0].Reason = "ContainerCreating"
}

func running(p *model.Pod, ready bool) {
	c := &p.Containers[0]
	p.Phase, p.Status = "Running", "Running"
	if c.State != "Running" {
		c.StartedAt = time.Now()
	}
	c.State, c.Reason, c.Message, c.Started, c.Ready, c.Current = "Running", "", "", true, ready, nil
	p.Ready = 0
	if ready {
		p.Ready = 1
		p.ReadyAt = time.Now()
	}
}

func oom(p *model.Pod) {
	c := &p.Containers[0]
	t := &model.Termination{Reason: "OOMKilled", ExitCode: 137, StartedAt: c.StartedAt, FinishedAt: time.Now()}
	p.Status, p.Ready = "OOMKilled", 0
	c.State, c.Reason, c.Ready, c.Current = "Terminated", "OOMKilled", false, t
}

func crashloop(p *model.Pod) {
	c := &p.Containers[0]
	c.Last, c.Current = c.Current, nil
	c.RestartCount++
	p.Restarts = c.RestartCount
	p.Status = "CrashLoopBackOff"
	c.State, c.Reason, c.Started = "Waiting", "CrashLoopBackOff", false
	c.Message = "back-off 10s restarting failed container=app pod=" + p.Name + "_" + ns
}

func (s *sim) terminate(name string) {
	if p := s.pods[name]; p != nil {
		p.Deleting, p.Status = true, "Terminating"
		s.event("Normal", "Killing", "Pod/"+name, "Stopping container app")
	}
}

func (s *sim) remove(name string) { delete(s.pods, name) }

// event records an event, bumping the count of an identical earlier one.
func (s *sim) event(typ, reason, obj, msg string) {
	for i := range s.events {
		e := &s.events[i]
		if e.Object == obj && e.Reason == reason && e.Message == msg {
			e.Count++
			e.Time = time.Now()
			return
		}
	}
	s.events = append(s.events, model.Event{Time: time.Now(), Type: typ, Reason: reason, Message: msg, Object: obj, Count: 1})
}

func (s *sim) log(pod, text string) { s.emit(logs.Line{Pod: pod, Container: "app", Text: text}) }
func (s *sim) prevLog(pod, text string) {
	s.emit(logs.Line{Pod: pod, Container: "app", Text: text, Previous: true})
}

func (s *sim) emit(l logs.Line) {
	l.Time = time.Now()
	select {
	case s.lines <- l:
	default: // the renderer is behind; a demo can drop a line
	}
}

func (s *sim) snapshot() model.Snapshot {
	snap := model.Snapshot{
		Time:   time.Now(),
		Target: model.Target{Kind: "Rollout", Name: app, Namespace: ns, Selector: "app=" + app},
	}
	revs := map[string]*model.Revision{
		canaryHash: {Name: app + "-" + canaryHash, Hash: canaryHash, Number: 8, Images: []string{canaryImage}, Created: time.Now().Add(-time.Minute)},
		stableHash: {Name: app + "-" + stableHash, Hash: stableHash, Number: 7, Images: []string{stableImage}, Created: time.Now().Add(-73 * time.Hour)},
	}
	st := s.status
	st.Updated, st.Ready, st.Available = 0, 0, 0
	for _, p := range s.pods {
		snap.Pods = append(snap.Pods, clonePod(p))
		r := revs[p.Hash]
		if !p.Deleting {
			r.Current++
		}
		if p.Healthy() {
			r.Ready++
			r.Available++
			st.Ready++
			st.Available++
		}
		if p.Hash == canaryHash && !p.Deleting {
			st.Updated++
		}
	}
	sort.Slice(snap.Pods, func(i, j int) bool {
		if !snap.Pods[i].Created.Equal(snap.Pods[j].Created) {
			return snap.Pods[i].Created.After(snap.Pods[j].Created)
		}
		return snap.Pods[i].Name < snap.Pods[j].Name
	})
	for _, h := range []string{canaryHash, stableHash} {
		r := revs[h]
		r.Desired = s.desired[h]
		switch {
		case h == st.StableHash:
			r.Role = model.RoleStable
		case h == canaryHash:
			r.Role = model.RoleCanary
		default:
			r.Role = model.RoleOld
		}
		snap.Revisions = append(snap.Revisions, *r)
	}
	st.Analysis = append([]model.Analysis(nil), s.status.Analysis...)
	for i := range st.Analysis {
		st.Analysis[i].Metrics = append([]model.AnalysisMetric(nil), st.Analysis[i].Metrics...)
	}
	snap.Rollout = &st
	snap.Events = append([]model.Event(nil), s.events...)
	return snap
}

func clonePod(p *model.Pod) model.Pod {
	c := *p
	c.Containers = append([]model.Container(nil), p.Containers...)
	return c
}
