package app

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/seneralkan/kubemon/internal/kube"
	"github.com/seneralkan/kubemon/internal/model"
	"github.com/seneralkan/kubemon/internal/rollout"
)

func ptr[T any](v T) *T { return &v }

// TestStreamDeploymentRelease drives a Deployment release through the whole
// pipeline (Resolve → Source + watcher → snapshots) on fake clients.
func TestStreamDeploymentRelease(t *testing.T) {
	labels := map[string]string{"app": "web"}
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "ns", UID: "dep-uid", Generation: 2},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RollingUpdateDeploymentStrategyType},
		},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 2, UpdatedReplicas: 1, AvailableReplicas: 1},
	}
	owner := []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: "web", UID: "dep-uid", Controller: ptr(true)}}
	rs := func(hash, rev string, replicas int32) *appsv1.ReplicaSet {
		return &appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{
				Name: "web-" + hash, Namespace: "ns", OwnerReferences: owner,
				Labels:      map[string]string{"app": "web", "pod-template-hash": hash},
				Annotations: map[string]string{"deployment.kubernetes.io/revision": rev},
			},
			Spec: appsv1.ReplicaSetSpec{Replicas: ptr(replicas), Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "web:" + rev}}},
			}},
		}
	}
	newPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-new-1", Namespace: "ns", Labels: map[string]string{"app": "web", "pod-template-hash": "new"}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "app", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		}}},
	}
	probe := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "ev", Namespace: "ns"},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "web-new-1"},
		Type:           "Warning", Reason: "BackOff", Message: "Back-off restarting failed container",
		LastTimestamp: metav1.Now(),
	}
	unrelated := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "ev2", Namespace: "ns"},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "api-1"},
		LastTimestamp:  metav1.Now(),
	}

	kc := fake.NewClientset(dep, rs("old", "1", 1), rs("new", "2", 1), newPod, probe, unrelated)
	gvr := schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "rollouts"}
	c := &kube.Clients{
		Kube:      kc,
		Dynamic:   dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{gvr: "RolloutList"}),
		Namespace: "ns",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	src, err := rollout.Resolve(ctx, c, "ns", "web")
	if err != nil {
		t.Fatal(err)
	}
	snaps, err := Stream(ctx, Config{Clients: c, Source: src, EventsSince: time.Now().Add(-time.Minute), Debounce: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}

	first := next(t, snaps)
	if first.Rollout.Phase != model.PhaseProgressing || first.Rollout.CurrentHash != "new" {
		t.Fatalf("rollout = %+v, want Progressing with CurrentHash new", first.Rollout)
	}
	if len(first.Revisions) != 2 || first.Revisions[0].Role != model.RoleNew {
		t.Fatalf("revisions = %+v", first.Revisions)
	}
	if len(first.Pods) != 1 || first.Pods[0].Status != "CrashLoopBackOff" {
		t.Fatalf("pods = %+v", first.Pods)
	}
	if len(first.Events) != 1 || first.Events[0].Reason != "BackOff" {
		t.Fatalf("events = %+v, want only the new pod's BackOff", first.Events)
	}

	// Release finishes: old RS scaled down, status caught up.
	dep.Status = appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}
	if _, err := kc.AppsV1().Deployments("ns").UpdateStatus(ctx, dep, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	for {
		s := next(t, snaps)
		if s.Rollout.Done() {
			return
		}
	}
}

func next(t *testing.T, ch <-chan model.Snapshot) model.Snapshot {
	t.Helper()
	select {
	case s, ok := <-ch:
		if !ok {
			t.Fatal("stream closed")
		}
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("no snapshot")
	}
	return model.Snapshot{}
}
