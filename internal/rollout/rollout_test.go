package rollout

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/seneralkan/kubemon/internal/kube"
	"github.com/seneralkan/kubemon/internal/model"
)

const ns = "shop"

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func ptr[T any](v T) *T { return &v }

func newClients(kubeObjs []runtime.Object, dynObjs ...runtime.Object) *kube.Clients {
	listKinds := map[schema.GroupVersionResource]string{
		rolloutGVR:     "RolloutList",
		analysisRunGVR: "AnalysisRunList",
	}
	return &kube.Clients{
		Kube:    fake.NewClientset(kubeObjs...),
		Dynamic: dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, dynObjs...),
	}
}

func deployment(name string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID(name + "-uid"), Generation: 2},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr(int32(3)),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
		},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 3, UpdatedReplicas: 3, ReadyReplicas: 3, AvailableReplicas: 3},
	}
}

func rolloutObj(name string, spec, status map[string]any) *unstructured.Unstructured {
	if spec == nil {
		spec = map[string]any{}
	}
	if _, ok := spec["selector"]; !ok {
		spec["selector"] = map[string]any{"matchLabels": map[string]any{"app": name}}
	}
	if status == nil {
		status = map[string]any{}
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Rollout",
		"metadata":   map[string]any{"name": name, "namespace": ns, "uid": name + "-uid", "generation": int64(4)},
		"spec":       spec,
		"status":     status,
	}}
	return u
}

func analysisRun(name, owner, hash string, created time.Time, status map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "AnalysisRun",
		"metadata": map[string]any{
			"name": name, "namespace": ns,
			"labels":          map[string]any{"rollouts-pod-template-hash": hash},
			"ownerReferences": []any{map[string]any{"apiVersion": "argoproj.io/v1alpha1", "kind": "Rollout", "name": owner, "uid": owner + "-uid"}},
		},
		"status": status,
	}}
	u.SetCreationTimestamp(metav1.NewTime(created))
	return u
}

func replicaSet(name, hash string, rev, desired int32, owner string, created time.Time) *appsv1.ReplicaSet {
	r := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns,
			Labels:            map[string]string{"pod-template-hash": hash},
			Annotations:       map[string]string{annDeploymentRevision: itoa(rev)},
			CreationTimestamp: metav1.NewTime(created),
		},
		Spec: appsv1.ReplicaSetSpec{
			Replicas: ptr(desired),
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "app:" + itoa(rev)}}}},
		},
		Status: appsv1.ReplicaSetStatus{Replicas: desired, ReadyReplicas: desired, AvailableReplicas: desired},
	}
	if owner != "" {
		r.OwnerReferences = []metav1.OwnerReference{{Kind: "Deployment", Name: owner, UID: types.UID(owner + "-uid"), Controller: ptr(true)}}
	}
	return r
}

func itoa(n int32) string { return strconv.Itoa(int(n)) }

func names(revs []model.Revision) string {
	var s []string
	for _, r := range revs {
		s = append(s, r.Name+"="+r.Role)
	}
	return strings.Join(s, ",")
}

func TestParseRef(t *testing.T) {
	tests := []struct {
		ref, kind, name string
		wantErr         bool
	}{
		{ref: "web", name: "web"},
		{ref: "deploy/web", kind: kindDeployment, name: "web"},
		{ref: "deployment/web", kind: kindDeployment, name: "web"},
		{ref: "deployments/web", kind: kindDeployment, name: "web"},
		{ref: "rollout/web", kind: kindRollout, name: "web"},
		{ref: "rollouts/web", kind: kindRollout, name: "web"},
		{ref: "ro/web", kind: kindRollout, name: "web"},
		{ref: "Deploy/web", kind: kindDeployment, name: "web"},
		{ref: "sts/web", wantErr: true},
		{ref: "deploy/", wantErr: true},
		{ref: "", wantErr: true},
		{ref: "a/b/c", wantErr: true},
	}
	for _, tt := range tests {
		kind, name, err := parseRef(tt.ref)
		if (err != nil) != tt.wantErr || kind != tt.kind || name != tt.name {
			t.Errorf("parseRef(%q) = %q, %q, %v; want %q, %q, err=%v", tt.ref, kind, name, err, tt.kind, tt.name, tt.wantErr)
		}
	}
}

func TestResolve(t *testing.T) {
	both := func() *kube.Clients {
		return newClients([]runtime.Object{deployment("web"), deployment("api")}, rolloutObj("web", nil, nil))
	}
	tests := []struct {
		name    string
		clients *kube.Clients
		ref     string
		want    model.Target
		wantErr string
	}{
		{name: "bare prefers rollout", clients: both(), ref: "web",
			want: model.Target{Kind: kindRollout, Name: "web", Namespace: ns, Selector: "app=web"}},
		{name: "bare falls back to deployment", clients: both(), ref: "api",
			want: model.Target{Kind: kindDeployment, Name: "api", Namespace: ns, Selector: "app=api"}},
		{name: "explicit deployment skips rollout", clients: both(), ref: "deploy/web",
			want: model.Target{Kind: kindDeployment, Name: "web", Namespace: ns, Selector: "app=web"}},
		{name: "explicit rollout", clients: both(), ref: "ro/web",
			want: model.Target{Kind: kindRollout, Name: "web", Namespace: ns, Selector: "app=web"}},
		{name: "bare not found", clients: both(), ref: "nope",
			wantErr: "no Rollout or Deployment named nope in namespace shop"},
		{name: "explicit rollout not found", clients: both(), ref: "rollout/api",
			wantErr: "no Rollout named api in namespace shop"},
		{name: "explicit deployment not found", clients: both(), ref: "deploy/nope",
			wantErr: "no Deployment named nope in namespace shop"},
		{name: "no rollout CRD", clients: noCRD(newClients([]runtime.Object{deployment("api")})), ref: "api",
			want: model.Target{Kind: kindDeployment, Name: "api", Namespace: ns, Selector: "app=api"}},
		{name: "bare name with rollouts forbidden falls back", clients: argoForbidden(newClients([]runtime.Object{deployment("api")})), ref: "api",
			want: model.Target{Kind: kindDeployment, Name: "api", Namespace: ns, Selector: "app=api"}},
		{name: "explicit rollout forbidden is an error", clients: argoForbidden(newClients(nil)), ref: "rollout/api",
			wantErr: "get rollout shop/api: rollouts.argoproj.io \"api\" is forbidden: denied"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src, err := Resolve(context.Background(), tt.clients, ns, tt.ref)
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := src.Target(); got != tt.want {
				t.Errorf("target = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// argoForbidden makes every argoproj.io request fail with Forbidden.
func argoForbidden(c *kube.Clients) *kube.Clients {
	d := c.Dynamic.(*dynfake.FakeDynamicClient)
	d.PrependReactor("*", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "argoproj.io", Resource: a.GetResource().Resource}, "api", errors.New("denied"))
	})
	return c
}

// noCRD makes every argoproj.io request fail the way an API server without the CRDs does.
func noCRD(c *kube.Clients) *kube.Clients {
	d := c.Dynamic.(*dynfake.FakeDynamicClient)
	d.PrependReactor("*", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Group: "argoproj.io", Resource: a.GetResource().Resource}, "")
	})
	return c
}

func TestStartDeployment(t *testing.T) {
	d := deployment("web")
	c := newClients([]runtime.Object{d})
	src, err := Resolve(context.Background(), c, ns, "deploy/web")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var notified atomic.Int32
	if err := src.Start(ctx, func() { notified.Add(1) }); err != nil {
		t.Fatal(err)
	}
	rs := []*appsv1.ReplicaSet{replicaSet("web-a", "a", 1, 3, "web", t0)}
	st, revs := src.Status(rs, nil)
	if st.Phase != model.PhaseHealthy || st.CurrentHash != "a" || names(revs) != "web-a=new" {
		t.Errorf("status = %+v, revisions %s", st, names(revs))
	}
	for deadline := time.Now().Add(2 * time.Second); notified.Load() == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) { // handlers run asynchronously after cache sync
			t.Fatal("notify was not called for the initial add")
		}
	}
}

func TestStartRollout(t *testing.T) {
	ro := rolloutObj("web", nil, map[string]any{"phase": "Healthy", "observedGeneration": "4", "currentPodHash": "h2", "stableRS": "h2"})
	run := analysisRun("web-h2-1", "web", "h2", t0, map[string]any{"phase": "Successful"})
	other := analysisRun("api-h2-1", "api", "h2", t0, map[string]any{"phase": "Failed"})

	for _, tt := range []struct {
		name         string
		clients      *kube.Clients
		wantAnalysis int
	}{
		{name: "with analysis runs", clients: newClients(nil, ro.DeepCopy(), run.DeepCopy(), other.DeepCopy()), wantAnalysis: 1},
		{name: "analysisrun CRD missing", clients: noAnalysisCRD(newClients(nil, ro.DeepCopy())), wantAnalysis: 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			src, err := Resolve(context.Background(), tt.clients, ns, "web")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if err := src.Start(ctx, func() {}); err != nil {
				t.Fatal(err)
			}
			st, _ := src.Status(nil, nil)
			if st.Phase != model.PhaseHealthy || len(st.Analysis) != tt.wantAnalysis {
				t.Errorf("phase %s, analysis %+v", st.Phase, st.Analysis)
			}
		})
	}
}

func noAnalysisCRD(c *kube.Clients) *kube.Clients {
	c.Dynamic.(*dynfake.FakeDynamicClient).PrependReactor("list", "analysisruns", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Group: "argoproj.io", Resource: "analysisruns"}, "")
	})
	return c
}

func TestDeploymentStatus(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(d *appsv1.Deployment)
		phase   string
		message string
	}{
		{name: "healthy", mutate: func(*appsv1.Deployment) {}, phase: model.PhaseHealthy, message: "3 of 3 replicas available"},
		{name: "generation lag", mutate: func(d *appsv1.Deployment) { d.Generation = 3 },
			phase: model.PhaseProgressing, message: "waiting for deployment spec update to be observed"},
		{name: "updating", mutate: func(d *appsv1.Deployment) { d.Status.UpdatedReplicas = 1 },
			phase: model.PhaseProgressing, message: "1 of 3 new replicas updated"},
		{name: "old terminating", mutate: func(d *appsv1.Deployment) { d.Status.Replicas = 4 },
			phase: model.PhaseProgressing, message: "1 old replicas pending termination"},
		{name: "not available", mutate: func(d *appsv1.Deployment) { d.Status.AvailableReplicas = 2 },
			phase: model.PhaseProgressing, message: "2 of 3 updated replicas available"},
		{name: "deadline exceeded", mutate: func(d *appsv1.Deployment) {
			d.Status.UpdatedReplicas = 1
			d.Status.Conditions = []appsv1.DeploymentCondition{{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse,
				Reason: "ProgressDeadlineExceeded", Message: `ReplicaSet "web-b" has timed out progressing.`}}
		}, phase: model.PhaseDegraded, message: `ReplicaSet "web-b" has timed out progressing.`},
		{name: "paused", mutate: func(d *appsv1.Deployment) { d.Spec.Paused = true; d.Status.UpdatedReplicas = 1 },
			phase: model.PhasePaused, message: "paused: 1 of 3 replicas updated"},
		{name: "paused but fully rolled out is healthy", mutate: func(d *appsv1.Deployment) { d.Spec.Paused = true },
			phase: model.PhaseHealthy, message: "3 of 3 replicas available"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := deployment("web")
			tt.mutate(d)
			st, _ := deploymentStatus(d, nil, nil)
			if st.Phase != tt.phase || st.Message != tt.message {
				t.Errorf("got %s %q, want %s %q", st.Phase, st.Message, tt.phase, tt.message)
			}
			if st.Strategy != "RollingUpdate" || st.Desired != 3 {
				t.Errorf("strategy %q desired %d", st.Strategy, st.Desired)
			}
		})
	}
}

func TestDeploymentRevisions(t *testing.T) {
	rs := []*appsv1.ReplicaSet{
		replicaSet("web-1", "h1", 1, 0, "web", t0),
		replicaSet("web-3", "h3", 3, 2, "web", t0.Add(2*time.Hour)),
		replicaSet("web-2", "h2", 2, 0, "web", t0.Add(time.Hour)),
		replicaSet("other-9", "h9", 9, 1, "other", t0.Add(3*time.Hour)), // not owned
	}
	st, revs := deploymentStatus(deployment("web"), rs, nil)
	if got, want := names(revs), "web-3=new,web-2=old"; got != want {
		t.Errorf("revisions %s, want %s", got, want)
	}
	if st.CurrentHash != "h3" || st.StableHash != "" {
		t.Errorf("hashes current=%q stable=%q", st.CurrentHash, st.StableHash)
	}
	// RS cache lagging the Deployment (it already wants revision 4): no current hash yet.
	ahead := deployment("web")
	ahead.Annotations = map[string]string{annDeploymentRevision: "4"}
	if st, _ := deploymentStatus(ahead, rs, nil); st.CurrentHash != "" {
		t.Errorf("lagging cache: current=%q, want empty", st.CurrentHash)
	}
	// An old revision that still has pods is kept.
	_, revs = deploymentStatus(deployment("web"), rs, []model.Pod{{Name: "web-1-x", Hash: "h1"}})
	if got, want := names(revs), "web-3=new,web-2=old,web-1=old"; got != want {
		t.Errorf("with pods: revisions %s, want %s", got, want)
	}
}

func canarySpec(steps ...any) map[string]any {
	return map[string]any{"replicas": int64(5), "strategy": map[string]any{"canary": map[string]any{"steps": steps}}}
}

func TestRolloutCanary(t *testing.T) {
	steps := []any{
		map[string]any{"setWeight": int64(20)},
		map[string]any{"pause": map[string]any{"duration": "5m"}},
		map[string]any{"setWeight": int64(50)},
		map[string]any{"analysis": map[string]any{"templates": []any{map[string]any{"templateName": "success-rate"}}}},
		map[string]any{"pause": map[string]any{}},
		map[string]any{"setCanaryScale": map[string]any{"replicas": int64(2)}},
		map[string]any{"experiment": map[string]any{}},
		map[string]any{"pause": map[string]any{"duration": int64(30)}},
	}
	tests := []struct {
		name       string
		spec       map[string]any
		status     map[string]any
		phase      string
		step       int
		stepDesc   string
		weight     int32
		paused     bool
		observed   int64
		msgContain string
	}{
		{name: "pause with duration", spec: canarySpec(steps...),
			status: map[string]any{"phase": "Paused", "observedGeneration": "4", "currentStepIndex": int64(1),
				"pauseConditions": []any{map[string]any{"reason": "CanaryPauseStep"}}, "message": "CanaryPauseStep"},
			phase: model.PhasePaused, step: 1, stepDesc: "pause: 5m", weight: 20, paused: true, observed: 4, msgContain: "CanaryPauseStep"},
		{name: "analysis step", spec: canarySpec(steps...),
			status: map[string]any{"phase": "Progressing", "observedGeneration": "4", "currentStepIndex": int64(3)},
			phase:  model.PhaseProgressing, step: 3, stepDesc: "analysis: success-rate", weight: 50, observed: 4},
		{name: "setWeight at current step", spec: canarySpec(steps...),
			status: map[string]any{"phase": "Progressing", "observedGeneration": int64(4), "currentStepIndex": int64(2)},
			phase:  model.PhaseProgressing, step: 2, stepDesc: "setWeight: 50", weight: 50, observed: 4},
		{name: "indefinite pause", spec: canarySpec(steps...),
			status: map[string]any{"phase": "Paused", "observedGeneration": "4", "currentStepIndex": int64(4)},
			phase:  model.PhasePaused, step: 4, stepDesc: "pause (until promoted)", weight: 50, observed: 4},
		{name: "canary scale", spec: canarySpec(steps...),
			status: map[string]any{"phase": "Progressing", "observedGeneration": "4", "currentStepIndex": int64(5)},
			phase:  model.PhaseProgressing, step: 5, stepDesc: "setCanaryScale: 2 replicas", weight: 50, observed: 4},
		{name: "experiment", spec: canarySpec(steps...),
			status: map[string]any{"phase": "Progressing", "observedGeneration": "4", "currentStepIndex": int64(6)},
			phase:  model.PhaseProgressing, step: 6, stepDesc: "experiment", weight: 50, observed: 4},
		{name: "numeric pause duration", spec: canarySpec(steps...),
			status: map[string]any{"phase": "Progressing", "observedGeneration": "4", "currentStepIndex": int64(7)},
			phase:  model.PhaseProgressing, step: 7, stepDesc: "pause: 30s", weight: 50, observed: 4},
		{name: "traffic-routed weight wins", spec: canarySpec(steps...),
			status: map[string]any{"phase": "Progressing", "observedGeneration": "4", "currentStepIndex": int64(2),
				"canary": map[string]any{"weights": map[string]any{"canary": map[string]any{"weight": int64(35)}}}},
			phase: model.PhaseProgressing, step: 2, stepDesc: "setWeight: 50", weight: 35, observed: 4},
		{name: "all steps done", spec: canarySpec(steps...),
			status: map[string]any{"phase": "Healthy", "observedGeneration": "4", "currentStepIndex": int64(8)},
			phase:  model.PhaseHealthy, step: 8, weight: 100, observed: 4},
		{name: "aborted", spec: canarySpec(steps...),
			status: map[string]any{"phase": "Degraded", "abort": true, "observedGeneration": "4", "currentStepIndex": int64(0),
				"message": "RolloutAborted: metric success-rate failed"},
			phase: model.PhaseAborted, step: 0, stepDesc: "setWeight: 20", weight: 0, observed: 4, msgContain: "RolloutAborted"},
		{name: "spec paused", spec: func() map[string]any { s := canarySpec(steps...); s["paused"] = true; return s }(),
			status: map[string]any{"phase": "Progressing", "observedGeneration": "4", "currentStepIndex": int64(0)},
			phase:  model.PhasePaused, step: 0, stepDesc: "setWeight: 20", weight: 20, paused: true, observed: 4},
		{name: "string observedGeneration lagging", spec: canarySpec(steps...),
			status: map[string]any{"phase": "Healthy", "observedGeneration": "3", "currentStepIndex": int64(8)},
			phase:  model.PhaseProgressing, step: 8, weight: 100, observed: 3, msgContain: "spec update"},
		{name: "legacy hash observedGeneration ignored", spec: canarySpec(steps...),
			status: map[string]any{"phase": "Healthy", "observedGeneration": "7d9f8c6b5", "currentStepIndex": int64(8)},
			phase:  model.PhaseHealthy, step: 8, weight: 100},
		{name: "no steps", spec: canarySpec(),
			status: map[string]any{"phase": "Healthy", "observedGeneration": "4"},
			phase:  model.PhaseHealthy, step: 0, weight: 100, observed: 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, _ := rolloutStatus(rolloutObj("web", tt.spec, tt.status), nil, nil, nil)
			if st.Strategy != "Canary" || st.Phase != tt.phase || st.CurrentStep != tt.step || st.StepDesc != tt.stepDesc ||
				st.Weight != tt.weight || st.Paused != tt.paused || st.ObservedGeneration != tt.observed {
				t.Errorf("got strategy=%s phase=%s step=%d desc=%q weight=%d paused=%v observed=%d",
					st.Strategy, st.Phase, st.CurrentStep, st.StepDesc, st.Weight, st.Paused, st.ObservedGeneration)
			}
			if !strings.Contains(st.Message, tt.msgContain) {
				t.Errorf("message %q does not contain %q", st.Message, tt.msgContain)
			}
		})
	}
}

func rolloutRS(name, hash string, rev int32, desired int32, created time.Time) *appsv1.ReplicaSet {
	r := replicaSet(name, hash, rev, desired, "", created)
	r.Labels = map[string]string{"rollouts-pod-template-hash": hash}
	r.Annotations = map[string]string{annRolloutRevision: itoa(rev)}
	r.OwnerReferences = []metav1.OwnerReference{{Kind: "Rollout", Name: "web", UID: "web-uid", Controller: ptr(true)}}
	return r
}

func TestRolloutRoles(t *testing.T) {
	rs := []*appsv1.ReplicaSet{
		rolloutRS("web-h1", "h1", 1, 0, t0),
		rolloutRS("web-h2", "h2", 2, 0, t0.Add(time.Hour)),
		rolloutRS("web-h3", "h3", 3, 4, t0.Add(2*time.Hour)),
		rolloutRS("web-h4", "h4", 4, 1, t0.Add(3*time.Hour)),
	}

	t.Run("canary", func(t *testing.T) {
		u := rolloutObj("web", canarySpec(map[string]any{"setWeight": int64(20)}),
			map[string]any{"phase": "Progressing", "observedGeneration": "4", "currentStepIndex": int64(0),
				"stableRS": "h3", "currentPodHash": "h4", "replicas": int64(5), "updatedReplicas": int64(1),
				"readyReplicas": int64(5), "availableReplicas": int64(5)})
		st, revs := rolloutStatus(u, nil, rs, nil)
		if got, want := names(revs), "web-h4=canary,web-h3=stable,web-h2=old"; got != want {
			t.Errorf("revisions %s, want %s", got, want)
		}
		if st.StableHash != "h3" || st.CurrentHash != "h4" || st.Desired != 5 || st.Updated != 1 || st.Ready != 5 || st.Available != 5 {
			t.Errorf("status %+v", st)
		}
	})

	t.Run("blue-green", func(t *testing.T) {
		spec := map[string]any{"replicas": int64(4), "strategy": map[string]any{"blueGreen": map[string]any{
			"activeService": "web-active", "previewService": "web-preview"}}}
		u := rolloutObj("web", spec, map[string]any{"phase": "Paused", "observedGeneration": "4",
			"pauseConditions": []any{map[string]any{"reason": "BlueGreenPause"}},
			"stableRS":        "h3", "currentPodHash": "h4",
			"blueGreen": map[string]any{"activeSelector": "h3", "previewSelector": "h4"}})
		st, revs := rolloutStatus(u, nil, rs, nil)
		if got, want := names(revs), "web-h4=preview,web-h3=active,web-h2=old"; got != want {
			t.Errorf("revisions %s, want %s", got, want)
		}
		if st.Strategy != "BlueGreen" || st.ActiveService != "web-active" || st.PreviewService != "web-preview" ||
			st.Phase != model.PhasePaused || st.TotalSteps != 0 {
			t.Errorf("status %+v", st)
		}
	})
}

func TestRolloutAnalysis(t *testing.T) {
	metrics := map[string]any{
		"phase":   "Running",
		"message": "",
		"metricResults": []any{map[string]any{
			"name": "success-rate", "phase": "Running", "successful": int64(2), "failed": int64(1), "message": "",
			"measurements": []any{
				map[string]any{"phase": "Successful", "value": "0.99"},
				map[string]any{"phase": "Failed", "value": "0.42"},
			},
		}},
	}
	runs := []*unstructured.Unstructured{
		analysisRun("web-h4-1", "web", "h4", t0, map[string]any{"phase": "Successful"}),
		analysisRun("web-h4-2", "web", "h4", t0.Add(time.Minute), map[string]any{"phase": "Successful"}),
		analysisRun("web-h4-3", "web", "h4", t0.Add(2*time.Minute), map[string]any{"phase": "Failed", "message": "metric failed"}),
		analysisRun("web-h4-4", "web", "h4", t0.Add(3*time.Minute), metrics),
		analysisRun("web-h3-1", "web", "h3", t0.Add(4*time.Minute), map[string]any{"phase": "Successful"}), // other revision
		analysisRun("api-h4-1", "api", "h4", t0.Add(5*time.Minute), map[string]any{"phase": "Failed"}),     // other owner
	}
	u := rolloutObj("web", canarySpec(), map[string]any{"phase": "Progressing", "currentPodHash": "h4", "stableRS": "h3"})
	st, _ := rolloutStatus(u, runs, nil, nil)

	var got []string
	for _, a := range st.Analysis {
		got = append(got, a.Name+"="+a.Phase)
	}
	if want := "web-h4-4=Running,web-h4-3=Failed,web-h4-2=Successful"; strings.Join(got, ",") != want {
		t.Fatalf("analysis %v, want %s", got, want)
	}
	if st.Analysis[1].Message != "metric failed" {
		t.Errorf("message %q", st.Analysis[1].Message)
	}
	want := model.AnalysisMetric{Name: "success-rate", Phase: "Running", Value: "0.42", Successful: 2, Failed: 1}
	if len(st.Analysis[0].Metrics) != 1 || st.Analysis[0].Metrics[0] != want {
		t.Errorf("metrics %+v, want %+v", st.Analysis[0].Metrics, want)
	}
}

func TestGroupRevisions(t *testing.T) {
	rs := []*appsv1.ReplicaSet{
		replicaSet("web-2", "h2", 2, 0, "", t0.Add(time.Hour)),
		replicaSet("web-10", "h10", 10, 3, "", t0.Add(3*time.Hour)),
		replicaSet("web-1", "h1", 1, 0, "", t0),
		replicaSet("web-9", "h9", 9, 0, "", t0.Add(2*time.Hour)),
	}
	tests := []struct {
		name string
		rs   []*appsv1.ReplicaSet
		pods []model.Pod
		want string
	}{
		{name: "empty", want: ""},
		{name: "prunes scaled-down old revisions", rs: rs, want: "web-10=new,web-9=old"},
		{name: "keeps old revision with pods", rs: rs, pods: []model.Pod{{Hash: "h1"}},
			want: "web-10=new,web-9=old,web-1=old"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			revs := GroupRevisions(tt.rs, tt.pods)
			if got := names(revs); got != tt.want {
				t.Errorf("got %s, want %s", got, tt.want)
			}
		})
	}
	revs := GroupRevisions(rs, nil)
	if r := revs[0]; r.Number != 10 || r.Hash != "h10" || r.Desired != 3 || r.Ready != 3 || len(r.Images) != 1 || r.Images[0] != "app:10" {
		t.Errorf("revision %+v", r)
	}
}
