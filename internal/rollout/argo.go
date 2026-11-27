package rollout

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"

	"github.com/seneralkan/kubemon/internal/kube"
	"github.com/seneralkan/kubemon/internal/model"
	"github.com/seneralkan/kubemon/internal/watch"
)

var (
	rolloutGVR     = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "rollouts"}
	analysisRunGVR = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "analysisruns"}
)

const maxAnalysisRuns = 3

type rolloutSource struct {
	dyn    dynamic.Interface
	target model.Target
	// set by Start
	rollouts cache.GenericNamespaceLister
	runs     cache.GenericNamespaceLister // nil when the AnalysisRun CRD is not served
}

func newRolloutSource(c *kube.Clients, u *unstructured.Unstructured) (*rolloutSource, error) {
	sel, err := unstructuredSelector(u)
	if err != nil {
		return nil, fmt.Errorf("rollout %s/%s: %w", u.GetNamespace(), u.GetName(), err)
	}
	return &rolloutSource{
		dyn:    c.Dynamic,
		target: model.Target{Kind: kindRollout, Name: u.GetName(), Namespace: u.GetNamespace(), Selector: sel},
	}, nil
}

func (s *rolloutSource) Target() model.Target { return s.target }

func (s *rolloutSource) Start(ctx context.Context, notify func()) error {
	ns := s.target.Namespace
	_, err := s.dyn.Resource(rolloutGVR).Namespace(ns).List(ctx, metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("metadata.name", s.target.Name).String(), Limit: 1})
	if err := listable(err, "rollouts.argoproj.io", ns); err != nil {
		return err
	}
	byName := dynamicinformer.NewFilteredDynamicSharedInformerFactory(s.dyn, 0, ns, func(o *metav1.ListOptions) {
		o.FieldSelector = fields.OneTermEqualSelector("metadata.name", s.target.Name).String()
	})
	inf := byName.ForResource(rolloutGVR)
	if _, err := inf.Informer().AddEventHandler(notifyHandler(notify)); err != nil {
		return fmt.Errorf("add event handler: %w", err)
	}
	factories := []dynamicinformer.DynamicSharedInformerFactory{byName}

	var runs cache.GenericNamespaceLister
	_, err = s.dyn.Resource(analysisRunGVR).Namespace(ns).List(ctx, metav1.ListOptions{Limit: 1})
	switch {
	case err == nil:
		all := dynamicinformer.NewFilteredDynamicSharedInformerFactory(s.dyn, 0, ns, nil)
		arInf := all.ForResource(analysisRunGVR)
		if _, err := arInf.Informer().AddEventHandler(notifyHandler(notify)); err != nil {
			return fmt.Errorf("add event handler: %w", err)
		}
		runs = arInf.Lister().ByNamespace(ns)
		factories = append(factories, all)
	case apierrors.IsNotFound(err) || apierrors.IsForbidden(err):
		// AnalysisRuns not served or not readable: analysis is optional.
	default:
		return fmt.Errorf("list analysisruns: %w", err)
	}

	for _, f := range factories {
		f.Start(ctx.Done())
	}
	for _, f := range factories {
		for gvr, ok := range f.WaitForCacheSync(ctx.Done()) {
			if !ok {
				return fmt.Errorf("sync %v cache: %w", gvr, ctx.Err())
			}
		}
	}
	s.rollouts = inf.Lister().ByNamespace(ns)
	s.runs = runs
	return nil
}

func (s *rolloutSource) Status(rs []*appsv1.ReplicaSet, pods []model.Pod) (*model.RolloutStatus, []model.Revision) {
	var u *unstructured.Unstructured
	if s.rollouts != nil {
		if obj, err := s.rollouts.Get(s.target.Name); err == nil {
			u, _ = obj.(*unstructured.Unstructured)
		}
	}
	if u == nil {
		return &model.RolloutStatus{Kind: kindRollout, Phase: model.PhaseDegraded,
			Message: fmt.Sprintf("rollout %s not found", s.target.Name)}, GroupRevisions(rs, pods)
	}
	var runs []*unstructured.Unstructured
	if s.runs != nil {
		objs, _ := s.runs.List(labels.Everything())
		for _, o := range objs {
			if r, ok := o.(*unstructured.Unstructured); ok {
				runs = append(runs, r)
			}
		}
	}
	return rolloutStatus(u, runs, rs, pods)
}

// rolloutStatus reads an Argo Rollout, its AnalysisRuns and ReplicaSets.
func rolloutStatus(u *unstructured.Unstructured, runs []*unstructured.Unstructured, rs []*appsv1.ReplicaSet, pods []model.Pod) (*model.RolloutStatus, []model.Revision) {
	obj := u.Object
	st := &model.RolloutStatus{
		Kind:        kindRollout,
		Generation:  u.GetGeneration(),
		Desired:     1,
		Updated:     int32(num(obj, "status", "updatedReplicas")),
		Ready:       int32(num(obj, "status", "readyReplicas")),
		Available:   int32(num(obj, "status", "availableReplicas")),
		StableHash:  str(obj, "status", "stableRS"),
		CurrentHash: str(obj, "status", "currentPodHash"),
		Message:     str(obj, "status", "message"),
	}
	if n, ok := intField(obj, "spec", "replicas"); ok {
		st.Desired = int32(n)
	}
	// Argo stores observedGeneration as a string; very old versions stored a spec hash.
	observed, observedKnown := intField(obj, "status", "observedGeneration")
	st.ObservedGeneration = observed

	paused, _, _ := unstructured.NestedBool(obj, "spec", "paused")
	pauseConds, _, _ := unstructured.NestedSlice(obj, "status", "pauseConditions")
	st.Paused = paused || len(pauseConds) > 0
	aborted, _, _ := unstructured.NestedBool(obj, "status", "abort")

	st.Phase = str(obj, "status", "phase")
	switch {
	case aborted:
		st.Phase = model.PhaseAborted
		if st.Message == "" {
			st.Message = "rollout aborted"
		}
	case observedKnown && observed < st.Generation:
		st.Phase = model.PhaseProgressing
		st.Message = "waiting for rollout spec update to be observed"
	case st.Paused && st.Phase != model.PhaseDegraded:
		st.Phase = model.PhasePaused
	case st.Phase == "":
		st.Phase = model.PhaseProgressing
	}
	if st.Message == "" {
		st.Message = fmt.Sprintf("%d of %d updated replicas available", st.Available, st.Updated)
	}

	revs := revisions(ownedBy(rs, u.GetUID()))
	if _, ok, _ := unstructured.NestedMap(obj, "spec", "strategy", "blueGreen"); ok {
		st.Strategy = "BlueGreen"
		st.ActiveService = str(obj, "spec", "strategy", "blueGreen", "activeService")
		st.PreviewService = str(obj, "spec", "strategy", "blueGreen", "previewService")
		active := str(obj, "status", "blueGreen", "activeSelector")
		preview := str(obj, "status", "blueGreen", "previewSelector")
		for i := range revs {
			revs[i].Role = role(revs[i].Hash, active, model.RoleActive, preview, model.RolePreview)
		}
	} else {
		st.Strategy = "Canary"
		canaryStatus(obj, st, aborted)
		for i := range revs {
			revs[i].Role = role(revs[i].Hash, st.StableHash, model.RoleStable, st.CurrentHash, model.RoleCanary)
		}
	}

	st.Analysis = analyses(runs, u, st.CurrentHash)
	return st, prune(revs, pods)
}

// role returns primaryRole for hash == primary, secondaryRole for hash ==
// secondary (when it differs from primary), else RoleOld.
func role(hash, primary, primaryRole, secondary, secondaryRole string) string {
	switch hash {
	case "":
		return model.RoleOld
	case primary:
		return primaryRole
	case secondary:
		return secondaryRole
	}
	return model.RoleOld
}

func canaryStatus(obj map[string]any, st *model.RolloutStatus, aborted bool) {
	steps, _, _ := unstructured.NestedSlice(obj, "spec", "strategy", "canary", "steps")
	st.TotalSteps = len(steps)
	st.CurrentStep = st.TotalSteps
	if n, ok := intField(obj, "status", "currentStepIndex"); ok && int(n) < st.TotalSteps {
		st.CurrentStep = int(n)
	}
	if st.CurrentStep < st.TotalSteps {
		st.StepDesc = describeStep(steps[st.CurrentStep])
	}

	if w, ok := intField(obj, "status", "canary", "weights", "canary", "weight"); ok {
		st.Weight = int32(w)
		return
	}
	switch {
	case aborted:
		st.Weight = 0
	case st.CurrentStep >= st.TotalSteps:
		st.Weight = 100
	default:
		for _, s := range steps[:st.CurrentStep+1] {
			if m, ok := s.(map[string]any); ok {
				if w, ok := intField(m, "setWeight"); ok {
					st.Weight = int32(w)
				}
			}
		}
	}
}

// describeStep renders one canary step, e.g. "setWeight: 20" or "pause: 5m".
func describeStep(step any) string {
	m, ok := step.(map[string]any)
	if !ok || len(m) == 0 {
		return ""
	}
	switch {
	case m["setWeight"] != nil:
		return fmt.Sprintf("setWeight: %d", num(m, "setWeight"))
	case m["pause"] != nil:
		if d, ok, _ := unstructured.NestedFieldNoCopy(m, "pause", "duration"); ok {
			if s, isStr := d.(string); isStr {
				return "pause: " + s
			}
			return fmt.Sprintf("pause: %ds", num(m, "pause", "duration"))
		}
		return "pause (until promoted)"
	case m["analysis"] != nil:
		return withTemplates("analysis", m, "analysis")
	case m["experiment"] != nil:
		return withTemplates("experiment", m, "experiment")
	case m["setCanaryScale"] != nil:
		sc, _ := m["setCanaryScale"].(map[string]any)
		switch {
		case sc["replicas"] != nil:
			return fmt.Sprintf("setCanaryScale: %d replicas", num(sc, "replicas"))
		case sc["weight"] != nil:
			return fmt.Sprintf("setCanaryScale: %d%%", num(sc, "weight"))
		}
		return "setCanaryScale: match traffic weight"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys[0]
}

// withTemplates appends the step's template names: "analysis: success-rate".
func withTemplates(label string, m map[string]any, key string) string {
	tmpls, _, _ := unstructured.NestedSlice(m, key, "templates")
	var names []string
	for _, t := range tmpls {
		if tm, ok := t.(map[string]any); ok {
			if n := str(tm, "templateName"); n != "" {
				names = append(names, n)
			} else if n := str(tm, "name"); n != "" {
				names = append(names, n)
			}
		}
	}
	if len(names) == 0 {
		return label
	}
	return label + ": " + strings.Join(names, ", ")
}

// analyses summarizes the newest AnalysisRuns of the rollout's current revision.
func analyses(runs []*unstructured.Unstructured, owner *unstructured.Unstructured, hash string) []model.Analysis {
	var mine []*unstructured.Unstructured
	for _, r := range runs {
		if hash == "" || r.GetLabels()[watch.LabelRolloutsTemplateHash] != hash {
			continue
		}
		for _, ref := range r.GetOwnerReferences() {
			if ref.UID == owner.GetUID() {
				mine = append(mine, r)
				break
			}
		}
	}
	sort.SliceStable(mine, func(i, j int) bool {
		ti, tj := mine[i].GetCreationTimestamp(), mine[j].GetCreationTimestamp()
		if !ti.Equal(&tj) {
			return tj.Before(&ti)
		}
		return mine[i].GetName() > mine[j].GetName()
	})
	if len(mine) > maxAnalysisRuns {
		mine = mine[:maxAnalysisRuns]
	}

	var out []model.Analysis
	for _, r := range mine {
		a := model.Analysis{
			Name:    r.GetName(),
			Phase:   str(r.Object, "status", "phase"),
			Message: str(r.Object, "status", "message"),
		}
		results, _, _ := unstructured.NestedSlice(r.Object, "status", "metricResults")
		for _, res := range results {
			m, ok := res.(map[string]any)
			if !ok {
				continue
			}
			metric := model.AnalysisMetric{
				Name:       str(m, "name"),
				Phase:      str(m, "phase"),
				Successful: int32(num(m, "successful")),
				Failed:     int32(num(m, "failed")),
				Message:    str(m, "message"),
			}
			if ms, _, _ := unstructured.NestedSlice(m, "measurements"); len(ms) > 0 {
				if last, ok := ms[len(ms)-1].(map[string]any); ok {
					metric.Value = str(last, "value")
				}
			}
			a.Metrics = append(a.Metrics, metric)
		}
		out = append(out, a)
	}
	return out
}

// intField reads a number that may be encoded as an integer, float or string.
func intField(obj map[string]any, path ...string) (int64, bool) {
	v, ok, _ := unstructured.NestedFieldNoCopy(obj, path...)
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case int32:
		return int64(n), true
	case float64:
		return int64(n), true
	case string:
		i, err := strconv.ParseInt(n, 10, 64)
		return i, err == nil
	}
	return 0, false
}

func num(obj map[string]any, path ...string) int64 {
	n, _ := intField(obj, path...)
	return n
}

func str(obj map[string]any, path ...string) string {
	s, _, _ := unstructured.NestedString(obj, path...)
	return s
}
