package watch

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func ptr[T any](v T) *T { return &v }

func pod(name string, mod func(*corev1.Pod)) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: map[string]string{"app": "web", LabelPodTemplateHash: "abc"}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "web:1"}}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	if mod != nil {
		mod(p)
	}
	return p
}

func TestDisplayStatus(t *testing.T) {
	tests := []struct {
		name string
		mod  func(*corev1.Pod)
		want string
	}{
		{"running", func(p *corev1.Pod) {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
		}, "Running"},
		{"crashloop shows waiting reason, not phase", func(p *corev1.Pod) {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}}
		}, "CrashLoopBackOff"},
		{"terminated without reason", func(p *corev1.Pod) {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 3}}}}
		}, "ExitCode:3"},
		{"init progress", func(p *corev1.Pod) {
			p.Status.Phase = corev1.PodPending
			p.Spec.InitContainers = []corev1.Container{{Name: "a"}, {Name: "b"}}
			p.Status.InitContainerStatuses = []corev1.ContainerStatus{
				{Name: "a", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
				{Name: "b", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
			}
		}, "Init:1/2"},
		{"init crash", func(p *corev1.Pod) {
			p.Status.Phase = corev1.PodPending
			p.Spec.InitContainers = []corev1.Container{{Name: "a"}}
			p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "a", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}}
		}, "Init:CrashLoopBackOff"},
		{"started sidecar does not block", func(p *corev1.Pod) {
			p.Spec.InitContainers = []corev1.Container{{Name: "proxy", RestartPolicy: ptr(corev1.ContainerRestartPolicyAlways)}}
			p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "proxy", Started: ptr(true), State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}}}
		}, "ContainerCreating"},
		{"terminating", func(p *corev1.Pod) { p.DeletionTimestamp = &metav1.Time{Time: time.Now()} }, "Terminating"},
		{"evicted uses pod reason", func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed; p.Status.Reason = "Evicted" }, "Evicted"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := displayStatus(pod("p", tt.mod)); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestConvertPod(t *testing.T) {
	ready := time.Date(2026, 1, 1, 10, 0, 30, 0, time.UTC)
	p := ConvertPod(pod("web-abc-1", func(p *corev1.Pod) {
		p.Labels[LabelRolloutsTemplateHash] = "roll"
		p.Spec.NodeName = "node-1"
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.Time{Time: ready}}}
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: "app", Ready: true, RestartCount: 2, Image: "docker.io/web:1",
			State:                corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}},
		}}
	}))
	if p.Hash != "roll" {
		t.Errorf("hash = %q, want rollouts hash to win", p.Hash)
	}
	if p.Ready != 1 || p.Total != 1 || p.Restarts != 2 || !p.Healthy() {
		t.Errorf("ready=%d total=%d restarts=%d healthy=%v", p.Ready, p.Total, p.Restarts, p.Healthy())
	}
	if !p.ReadyAt.Equal(ready) || p.Node != "node-1" {
		t.Errorf("readyAt=%v node=%q", p.ReadyAt, p.Node)
	}
	c := p.Containers[0]
	if c.Image != "web:1" || c.Last == nil || c.Last.Reason != "OOMKilled" || c.Last.ExitCode != 137 {
		t.Errorf("container = %+v last=%+v", c, c.Last)
	}
}

func TestWatcher(t *testing.T) {
	now := time.Now()
	client := fake.NewClientset(
		pod("web-old", func(p *corev1.Pod) { p.CreationTimestamp = metav1.Time{Time: now.Add(-time.Hour)} }),
		pod("web-new", func(p *corev1.Pod) { p.CreationTimestamp = metav1.Time{Time: now} }),
		pod("other", func(p *corev1.Pod) { p.Labels = map[string]string{"app": "other"} }),
		&corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: "e1", Namespace: "ns"},
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "web-new"},
			Type:           "Warning", Reason: "Unhealthy", Message: "Readiness probe failed",
			LastTimestamp: metav1.Time{Time: now},
		},
		&corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: "e2", Namespace: "ns"},
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "other"},
			LastTimestamp:  metav1.Time{Time: now},
		},
		&corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: "e3", Namespace: "ns"},
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "web-new"},
			LastTimestamp:  metav1.Time{Time: now.Add(-time.Hour)},
		},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := New(client, "ns", "app=web", nil)
	changed := make(chan struct{}, 10)
	if err := w.Start(ctx, func() {
		select {
		case changed <- struct{}{}:
		default:
		}
	}); err != nil {
		t.Fatal(err)
	}

	pods := w.Pods()
	if len(pods) != 2 || pods[0].Name != "web-new" || pods[1].Name != "web-old" {
		t.Fatalf("pods = %+v, want web-new then web-old", pods)
	}

	related := func(_, name string) bool { return name == "web-new" || name == "web-old" }
	ev := w.Events(now.Add(-time.Minute), related, 10)
	if len(ev) != 1 || ev[0].Reason != "Unhealthy" || ev[0].Object != "Pod/web-new" || ev[0].Count != 1 {
		t.Fatalf("events = %+v, want only the recent Unhealthy event", ev)
	}

	// A new matching pod must trigger notify and appear.
	for len(changed) > 0 {
		<-changed
	}
	if _, err := client.CoreV1().Pods("ns").Create(ctx, pod("web-newer", nil), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("no notify after pod create")
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(w.Pods()) != 3 {
		if time.Now().After(deadline) {
			t.Fatalf("pods = %d, want 3", len(w.Pods()))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWatcherForbiddenFailsFast(t *testing.T) {
	client := fake.NewClientset()
	client.PrependReactor("list", "events", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "events"}, "", errors.New("denied"))
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := New(client, "ns", "app=web", nil).Start(ctx, func() {})
	if err == nil || !strings.Contains(err.Error(), "not allowed to list events in namespace ns") {
		t.Fatalf("err = %v, want a clear forbidden error", err)
	}
}

func TestEventCount(t *testing.T) {
	if n := eventCount(&corev1.Event{}); n != 1 {
		t.Errorf("empty count = %d, want 1", n)
	}
	if n := eventCount(&corev1.Event{Count: 0, Series: &corev1.EventSeries{Count: 7}}); n != 7 {
		t.Errorf("series count = %d, want 7", n)
	}
}
