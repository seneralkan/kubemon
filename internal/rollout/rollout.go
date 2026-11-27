// Package rollout resolves a release target (Deployment or Argo Rollout) and
// computes its release progress. Argo CRDs are read through the dynamic client;
// the argo-rollouts Go module is deliberately not imported.
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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"github.com/seneralkan/kubemon/internal/kube"
	"github.com/seneralkan/kubemon/internal/model"
	"github.com/seneralkan/kubemon/internal/watch"
)

// Source tracks one release target and turns its live state into status.
type Source interface {
	// Target describes the resolved object, including the selector of its pods.
	Target() model.Target
	// Start runs informers for the target object (and Argo AnalysisRuns), calls
	// notify on every change, and returns once caches are synced.
	Start(ctx context.Context, notify func()) error
	// Status computes release progress from the target object plus the
	// ReplicaSets and pods the caller's watcher sees for Target().Selector.
	Status(rs []*appsv1.ReplicaSet, pods []model.Pod) (*model.RolloutStatus, []model.Revision)
}

const (
	kindDeployment = "Deployment"
	kindRollout    = "Rollout"

	annDeploymentRevision = "deployment.kubernetes.io/revision"
	annRolloutRevision    = "rollout.argoproj.io/revision"
)

// Resolve finds the target. ref is "deploy/NAME", "deployment/NAME",
// "rollout/NAME", "ro/NAME", or a bare "NAME" (Argo Rollout tried first when the
// CRD exists, then Deployment).
func Resolve(ctx context.Context, c *kube.Clients, namespace, ref string) (Source, error) {
	kind, name, err := parseRef(ref)
	if err != nil {
		return nil, err
	}
	if kind != kindDeployment {
		u, err := c.Dynamic.Resource(rolloutGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		switch {
		case err == nil:
			return newRolloutSource(c, u)
		case kind == "" && apierrors.IsForbidden(err):
			// no access to Argo CRDs: it may well be a Deployment
		case !apierrors.IsNotFound(err): // a missing CRD is also NotFound
			return nil, fmt.Errorf("get rollout %s/%s: %w", namespace, name, err)
		case kind == kindRollout:
			return nil, fmt.Errorf("no Rollout named %s in namespace %s", name, namespace)
		}
	}
	d, err := c.Kube.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err) && kind == kindDeployment:
		return nil, fmt.Errorf("no Deployment named %s in namespace %s", name, namespace)
	case apierrors.IsNotFound(err):
		return nil, fmt.Errorf("no Rollout or Deployment named %s in namespace %s", name, namespace)
	case err != nil:
		return nil, fmt.Errorf("get deployment %s/%s: %w", namespace, name, err)
	}
	return newDeploymentSource(c, d)
}

// parseRef splits "kind/name" into a canonical kind ("" for a bare name) and name.
func parseRef(ref string) (kind, name string, err error) {
	k, n, found := strings.Cut(ref, "/")
	if !found {
		k, n = "", ref
	}
	if n == "" || strings.Contains(n, "/") {
		return "", "", fmt.Errorf("invalid target %q: want NAME, deploy/NAME or rollout/NAME", ref)
	}
	switch strings.ToLower(k) {
	case "":
		return "", n, nil
	case "deploy", "deployment", "deployments":
		return kindDeployment, n, nil
	case "ro", "rollout", "rollouts":
		return kindRollout, n, nil
	}
	return "", "", fmt.Errorf("unsupported kind %q in %q: want deploy/NAME or rollout/NAME", k, ref)
}

// listable fails with a readable message when RBAC forbids listing what an
// informer is about to watch; otherwise the informer retries forever.
func listable(err error, what, namespace string) error {
	switch {
	case err == nil:
		return nil
	case apierrors.IsForbidden(err):
		return fmt.Errorf("not allowed to list %s in namespace %s (kubemon needs get/list/watch): %w", what, namespace, err)
	}
	return fmt.Errorf("list %s in namespace %s: %w", what, namespace, err)
}

func selectorString(sel *metav1.LabelSelector) (string, error) {
	if sel == nil {
		return "", fmt.Errorf("spec.selector is empty")
	}
	s, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		return "", fmt.Errorf("invalid spec.selector: %w", err)
	}
	return s.String(), nil
}

func unstructuredSelector(u *unstructured.Unstructured) (string, error) {
	m, ok, _ := unstructured.NestedMap(u.Object, "spec", "selector")
	if !ok {
		return selectorString(nil)
	}
	var sel metav1.LabelSelector
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(m, &sel); err != nil {
		return "", fmt.Errorf("invalid spec.selector: %w", err)
	}
	return selectorString(&sel)
}

// GroupRevisions builds revisions from ReplicaSets and pods without a target
// object (bare selector mode). Newest first; the highest revision number gets
// RoleNew, the rest RoleOld.
func GroupRevisions(rs []*appsv1.ReplicaSet, pods []model.Pod) []model.Revision {
	revs := revisions(rs)
	for i := range revs {
		revs[i].Role = model.RoleOld
		if i == 0 {
			revs[i].Role = model.RoleNew
		}
	}
	return prune(revs, pods)
}

// ownedBy returns the ReplicaSets controlled by the object with uid.
func ownedBy(rs []*appsv1.ReplicaSet, uid types.UID) []*appsv1.ReplicaSet {
	var out []*appsv1.ReplicaSet
	for _, r := range rs {
		if ref := metav1.GetControllerOf(r); ref != nil && ref.UID == uid {
			out = append(out, r)
		}
	}
	return out
}

// revisions converts ReplicaSets to revisions, newest first, without roles.
func revisions(rs []*appsv1.ReplicaSet) []model.Revision {
	revs := make([]model.Revision, 0, len(rs))
	for _, r := range rs {
		rev := model.Revision{
			Name:      r.Name,
			Hash:      watch.TemplateHash(r.Labels),
			Number:    revisionNumber(r.Annotations),
			Current:   r.Status.Replicas,
			Ready:     r.Status.ReadyReplicas,
			Available: r.Status.AvailableReplicas,
			Created:   r.CreationTimestamp.Time,
		}
		if r.Spec.Replicas != nil {
			rev.Desired = *r.Spec.Replicas
		}
		for _, c := range r.Spec.Template.Spec.Containers {
			rev.Images = append(rev.Images, c.Image)
		}
		revs = append(revs, rev)
	}
	sort.SliceStable(revs, func(i, j int) bool {
		if revs[i].Number != revs[j].Number {
			return revs[i].Number > revs[j].Number
		}
		return revs[i].Created.After(revs[j].Created)
	})
	return revs
}

func revisionNumber(ann map[string]string) int64 {
	for _, k := range []string{annDeploymentRevision, annRolloutRevision} {
		if n, err := strconv.ParseInt(ann[k], 10, 64); err == nil {
			return n
		}
	}
	return 0
}

// prune drops scaled-down old revisions without pods, keeping the most recent
// old one so the developer sees what is being replaced. revs must be newest first.
func prune(revs []model.Revision, pods []model.Pod) []model.Revision {
	podsPerHash := map[string]int{}
	for _, p := range pods {
		podsPerHash[p.Hash]++
	}
	out := revs[:0]
	keptOld := false
	for _, r := range revs {
		if r.Role == model.RoleOld {
			if r.Desired == 0 && podsPerHash[r.Hash] == 0 && keptOld {
				continue
			}
			keptOld = true
		}
		out = append(out, r)
	}
	return out
}
