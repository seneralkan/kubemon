// Package watch keeps a live, informer-backed view of the pods, ReplicaSets
// and events that belong to one label selector in one namespace.
package watch

import (
	"context"
	"fmt"
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	appslisters "k8s.io/client-go/listers/apps/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/seneralkan/kubemon/internal/model"
)

// Watcher watches pods and ReplicaSets matching a selector, and the events in
// the namespace that concern them.
type Watcher struct {
	client    kubernetes.Interface
	namespace string
	selector  string
	related   func(kind, name string) bool

	objects informers.SharedInformerFactory
	events  informers.SharedInformerFactory

	pods     corelisters.PodLister
	rs       appslisters.ReplicaSetLister
	eventInf cache.SharedIndexInformer
}

// New creates a watcher. selector is a label selector string ("" matches
// everything). related optionally marks extra event objects as relevant (e.g.
// the Rollout itself); events of the watched pods and ReplicaSets always are.
func New(c kubernetes.Interface, namespace, selector string, related func(kind, name string) bool) *Watcher {
	objects := informers.NewSharedInformerFactoryWithOptions(c, 0,
		informers.WithNamespace(namespace),
		informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.LabelSelector = selector }),
	)
	events := informers.NewSharedInformerFactoryWithOptions(c, 0, informers.WithNamespace(namespace))
	if related == nil {
		related = func(string, string) bool { return false }
	}
	return &Watcher{
		client:    c,
		namespace: namespace,
		selector:  selector,
		related:   related,
		objects:   objects,
		events:    events,
		pods:      objects.Core().V1().Pods().Lister(),
		rs:        objects.Apps().V1().ReplicaSets().Lister(),
		eventInf:  events.Core().V1().Events().Informer(),
	}
}

// Start runs the informers, calls notify on every change, and returns once caches are synced.
func (w *Watcher) Start(ctx context.Context, notify func()) error {
	if err := w.checkAccess(ctx); err != nil {
		return err
	}
	h := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { notify() },
		UpdateFunc: func(any, any) { notify() },
		DeleteFunc: func(any) { notify() },
	}
	for _, inf := range []cache.SharedIndexInformer{
		w.objects.Core().V1().Pods().Informer(),
		w.objects.Apps().V1().ReplicaSets().Informer(),
	} {
		if _, err := inf.AddEventHandler(h); err != nil {
			return fmt.Errorf("add event handler: %w", err)
		}
	}
	// A busy namespace has a steady stream of events: only wake up for ours.
	onEvent := func(o any) {
		if e, ok := o.(*corev1.Event); ok && w.concerns(e.InvolvedObject.Kind, e.InvolvedObject.Name) {
			notify()
		}
	}
	if _, err := w.eventInf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    onEvent,
		UpdateFunc: func(_, o any) { onEvent(o) },
	}); err != nil {
		return fmt.Errorf("add event handler: %w", err)
	}
	w.objects.Start(ctx.Done())
	w.events.Start(ctx.Done())
	for typ, ok := range w.objects.WaitForCacheSync(ctx.Done()) {
		if !ok {
			return fmt.Errorf("sync %v cache: %w", typ, ctx.Err())
		}
	}
	for typ, ok := range w.events.WaitForCacheSync(ctx.Done()) {
		if !ok {
			return fmt.Errorf("sync %v cache: %w", typ, ctx.Err())
		}
	}
	return nil
}

// checkAccess lists each resource once so that missing RBAC fails fast with a
// clear message; an informer would otherwise retry a forbidden list forever.
func (w *Watcher) checkAccess(ctx context.Context) error {
	sel := metav1.ListOptions{LabelSelector: w.selector, Limit: 1}
	checks := []struct {
		what string
		list func() error
	}{
		{"pods", func() error { _, err := w.client.CoreV1().Pods(w.namespace).List(ctx, sel); return err }},
		{"replicasets", func() error { _, err := w.client.AppsV1().ReplicaSets(w.namespace).List(ctx, sel); return err }},
		{"events", func() error {
			_, err := w.client.CoreV1().Events(w.namespace).List(ctx, metav1.ListOptions{Limit: 1})
			return err
		}},
	}
	for _, c := range checks {
		err := c.list()
		switch {
		case err == nil:
		case apierrors.IsForbidden(err):
			return fmt.Errorf("not allowed to list %s in namespace %s (kubemon needs get/list/watch): %w", c.what, w.namespace, err)
		default:
			return fmt.Errorf("list %s in namespace %s: %w", c.what, w.namespace, err)
		}
	}
	return nil
}

// concerns reports whether an event's object is a watched pod or ReplicaSet,
// or marked related by the caller.
func (w *Watcher) concerns(kind, name string) bool {
	switch kind {
	case "Pod":
		if _, err := w.pods.Pods(w.namespace).Get(name); err == nil {
			return true
		}
	case "ReplicaSet":
		if _, err := w.rs.ReplicaSets(w.namespace).Get(name); err == nil {
			return true
		}
	}
	return w.related(kind, name)
}

// Pods returns the matching pods, newest first.
func (w *Watcher) Pods() []model.Pod {
	list, _ := w.pods.List(labels.Everything()) // lister errors only on bad selectors; Everything is fine
	out := make([]model.Pod, 0, len(list))
	for _, p := range list {
		out = append(out, ConvertPod(p))
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Created.Equal(out[j].Created) {
			return out[i].Created.After(out[j].Created)
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// ReplicaSets returns the matching ReplicaSets.
func (w *Watcher) ReplicaSets() []*appsv1.ReplicaSet {
	list, _ := w.rs.List(labels.Everything())
	return list
}

// Events returns events newer than since whose involved object passes related,
// oldest first, keeping at most limit (the newest ones).
func (w *Watcher) Events(since time.Time, related func(kind, name string) bool, limit int) []model.Event {
	var out []model.Event
	for _, o := range w.eventInf.GetStore().List() {
		e, ok := o.(*corev1.Event)
		if !ok || !related(e.InvolvedObject.Kind, e.InvolvedObject.Name) {
			continue
		}
		t := EventTime(e)
		if t.Before(since) {
			continue
		}
		out = append(out, model.Event{
			Time:    t,
			Type:    e.Type,
			Reason:  e.Reason,
			Message: e.Message,
			Object:  e.InvolvedObject.Kind + "/" + e.InvolvedObject.Name,
			Count:   eventCount(e),
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// eventCount handles both core/v1 counts and events.k8s.io series.
func eventCount(e *corev1.Event) int32 {
	n := max(e.Count, 1)
	if e.Series != nil {
		n = max(n, e.Series.Count)
	}
	return n
}

// EventTime is the most recent time an event happened.
func EventTime(e *corev1.Event) time.Time {
	switch {
	case e.Series != nil && !e.Series.LastObservedTime.IsZero():
		return e.Series.LastObservedTime.Time
	case !e.LastTimestamp.IsZero():
		return e.LastTimestamp.Time
	case !e.EventTime.IsZero():
		return e.EventTime.Time
	}
	return e.CreationTimestamp.Time
}
