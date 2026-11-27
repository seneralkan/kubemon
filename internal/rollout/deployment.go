package rollout

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	appslisters "k8s.io/client-go/listers/apps/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/seneralkan/kubemon/internal/kube"
	"github.com/seneralkan/kubemon/internal/model"
)

// progressDeadlineExceeded is the Progressing condition reason set by the
// Deployment controller when spec.progressDeadlineSeconds is exceeded.
const progressDeadlineExceeded = "ProgressDeadlineExceeded"

type deploymentSource struct {
	kube   kubernetes.Interface
	target model.Target
	lister appslisters.DeploymentLister // set by Start
}

func newDeploymentSource(c *kube.Clients, d *appsv1.Deployment) (*deploymentSource, error) {
	sel, err := selectorString(d.Spec.Selector)
	if err != nil {
		return nil, fmt.Errorf("deployment %s/%s: %w", d.Namespace, d.Name, err)
	}
	return &deploymentSource{
		kube:   c.Kube,
		target: model.Target{Kind: kindDeployment, Name: d.Name, Namespace: d.Namespace, Selector: sel},
	}, nil
}

func (s *deploymentSource) Target() model.Target { return s.target }

func (s *deploymentSource) Start(ctx context.Context, notify func()) error {
	byName := fields.OneTermEqualSelector("metadata.name", s.target.Name).String()
	_, err := s.kube.AppsV1().Deployments(s.target.Namespace).List(ctx, metav1.ListOptions{FieldSelector: byName, Limit: 1})
	if err := listable(err, "deployments", s.target.Namespace); err != nil {
		return err
	}
	f := informers.NewSharedInformerFactoryWithOptions(s.kube, 0,
		informers.WithNamespace(s.target.Namespace),
		informers.WithTweakListOptions(func(o *metav1.ListOptions) {
			o.FieldSelector = byName
		}),
	)
	inf := f.Apps().V1().Deployments()
	if _, err := inf.Informer().AddEventHandler(notifyHandler(notify)); err != nil {
		return fmt.Errorf("add event handler: %w", err)
	}
	f.Start(ctx.Done())
	for typ, ok := range f.WaitForCacheSync(ctx.Done()) {
		if !ok {
			return fmt.Errorf("sync %v cache: %w", typ, ctx.Err())
		}
	}
	s.lister = inf.Lister()
	return nil
}

func (s *deploymentSource) Status(rs []*appsv1.ReplicaSet, pods []model.Pod) (*model.RolloutStatus, []model.Revision) {
	var d *appsv1.Deployment
	if s.lister != nil {
		d, _ = s.lister.Deployments(s.target.Namespace).Get(s.target.Name)
	}
	if d == nil {
		return &model.RolloutStatus{Kind: kindDeployment, Phase: model.PhaseDegraded,
			Message: fmt.Sprintf("deployment %s not found", s.target.Name)}, GroupRevisions(rs, pods)
	}
	return deploymentStatus(d, rs, pods)
}

// deploymentStatus follows the checks of `kubectl rollout status`.
func deploymentStatus(d *appsv1.Deployment, rs []*appsv1.ReplicaSet, pods []model.Pod) (*model.RolloutStatus, []model.Revision) {
	st := &model.RolloutStatus{
		Kind:               kindDeployment,
		Strategy:           string(d.Spec.Strategy.Type),
		Generation:         d.Generation,
		ObservedGeneration: d.Status.ObservedGeneration,
		Desired:            1,
		Updated:            d.Status.UpdatedReplicas,
		Ready:              d.Status.ReadyReplicas,
		Available:          d.Status.AvailableReplicas,
		Paused:             d.Spec.Paused,
	}
	if st.Strategy == "" {
		st.Strategy = string(appsv1.RollingUpdateDeploymentStrategyType)
	}
	if d.Spec.Replicas != nil {
		st.Desired = *d.Spec.Replicas
	}

	var progressing *appsv1.DeploymentCondition
	for i := range d.Status.Conditions {
		if d.Status.Conditions[i].Type == appsv1.DeploymentProgressing {
			progressing = &d.Status.Conditions[i]
		}
	}

	st.Phase = model.PhaseProgressing
	switch {
	case d.Status.ObservedGeneration < d.Generation:
		st.Message = "waiting for deployment spec update to be observed"
	case progressing != nil && progressing.Reason == progressDeadlineExceeded:
		st.Phase = model.PhaseDegraded
		st.Message = "progress deadline exceeded"
		if progressing.Message != "" {
			st.Message = progressing.Message
		}
	case d.Spec.Paused && st.Updated < st.Desired: // like kubectl, a finished paused rollout is healthy
		st.Phase = model.PhasePaused
		st.Message = fmt.Sprintf("paused: %d of %d replicas updated", st.Updated, st.Desired)
	case st.Updated < st.Desired:
		st.Message = fmt.Sprintf("%d of %d new replicas updated", st.Updated, st.Desired)
	case d.Status.Replicas > st.Updated:
		st.Message = fmt.Sprintf("%d old replicas pending termination", d.Status.Replicas-st.Updated)
	case st.Available < st.Updated:
		st.Message = fmt.Sprintf("%d of %d updated replicas available", st.Available, st.Updated)
	default:
		st.Phase = model.PhaseHealthy
		st.Message = fmt.Sprintf("%d of %d replicas available", st.Available, st.Desired)
	}

	revs := revisions(ownedBy(rs, d.UID))
	for i := range revs {
		revs[i].Role = model.RoleOld
		if i == 0 {
			revs[i].Role = model.RoleNew
			// The ReplicaSet cache can lag the Deployment's: only trust the
			// newest RS once its revision matches the Deployment's.
			if want := revisionNumber(d.Annotations); want == 0 || want == revs[i].Number {
				st.CurrentHash = revs[i].Hash
			}
		}
	}
	return st, prune(revs, pods)
}

func notifyHandler(notify func()) cache.ResourceEventHandlerFuncs {
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { notify() },
		UpdateFunc: func(any, any) { notify() },
		DeleteFunc: func(any) { notify() },
	}
}
