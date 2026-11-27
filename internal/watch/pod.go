package watch

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"

	"github.com/seneralkan/kubemon/internal/model"
)

// Hash labels set by the Deployment controller and by Argo Rollouts.
const (
	LabelPodTemplateHash      = "pod-template-hash"
	LabelRolloutsTemplateHash = "rollouts-pod-template-hash"
)

// TemplateHash returns the revision hash label of a pod or ReplicaSet.
func TemplateHash(labels map[string]string) string {
	if h := labels[LabelRolloutsTemplateHash]; h != "" {
		return h
	}
	return labels[LabelPodTemplateHash]
}

// ConvertPod turns a Kubernetes pod into a model.Pod.
func ConvertPod(p *corev1.Pod) model.Pod {
	out := model.Pod{
		Name:      p.Name,
		Namespace: p.Namespace,
		Node:      p.Spec.NodeName,
		Hash:      TemplateHash(p.Labels),
		Phase:     string(p.Status.Phase),
		Status:    displayStatus(p),
		Total:     len(p.Spec.Containers),
		Created:   p.CreationTimestamp.Time,
		Deleting:  p.DeletionTimestamp != nil,
	}
	for _, c := range p.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case corev1.PodScheduled:
			out.Scheduled = c.LastTransitionTime.Time
		case corev1.PodReady:
			out.ReadyAt = c.LastTransitionTime.Time
		}
	}

	images := map[string]string{}
	for _, c := range p.Spec.InitContainers {
		images[c.Name] = c.Image
	}
	for _, c := range p.Spec.Containers {
		images[c.Name] = c.Image
	}
	for _, s := range p.Status.InitContainerStatuses {
		c := convertContainer(s, images[s.Name])
		c.Init = true
		out.Containers = append(out.Containers, c)
	}
	for _, s := range p.Status.ContainerStatuses {
		c := convertContainer(s, images[s.Name])
		if c.Ready {
			out.Ready++
		}
		out.Restarts += c.RestartCount
		out.Containers = append(out.Containers, c)
	}
	return out
}

func convertContainer(s corev1.ContainerStatus, specImage string) model.Container {
	c := model.Container{
		Name:         s.Name,
		Image:        specImage,
		Ready:        s.Ready,
		Started:      s.Started != nil && *s.Started,
		RestartCount: s.RestartCount,
	}
	if c.Image == "" {
		c.Image = s.Image
	}
	switch {
	case s.State.Running != nil:
		c.State = "Running"
		c.StartedAt = s.State.Running.StartedAt.Time
	case s.State.Waiting != nil:
		c.State = "Waiting"
		c.Reason = s.State.Waiting.Reason
		c.Message = s.State.Waiting.Message
	case s.State.Terminated != nil:
		c.State = "Terminated"
		c.Current = convertTermination(s.State.Terminated)
		c.Reason = c.Current.Reason
		c.Message = c.Current.Message
	}
	if s.LastTerminationState.Terminated != nil {
		c.Last = convertTermination(s.LastTerminationState.Terminated)
	}
	return c
}

func convertTermination(t *corev1.ContainerStateTerminated) *model.Termination {
	return &model.Termination{
		Reason:     t.Reason,
		ExitCode:   t.ExitCode,
		Signal:     t.Signal,
		Message:    t.Message,
		StartedAt:  t.StartedAt.Time,
		FinishedAt: t.FinishedAt.Time,
	}
}

// displayStatus mirrors the STATUS column of `kubectl get pods`.
func displayStatus(p *corev1.Pod) string {
	reason := string(p.Status.Phase)
	if p.Status.Reason != "" {
		reason = p.Status.Reason
	}

	sidecars := map[string]bool{}
	for _, c := range p.Spec.InitContainers {
		if c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			sidecars[c.Name] = true
		}
	}

	initializing := false
	for i, c := range p.Status.InitContainerStatuses {
		switch {
		case c.State.Terminated != nil && c.State.Terminated.ExitCode == 0:
			continue
		case sidecars[c.Name] && c.Started != nil && *c.Started:
			continue
		case c.State.Terminated != nil:
			switch t := c.State.Terminated; {
			case t.Reason != "":
				reason = "Init:" + t.Reason
			case t.Signal != 0:
				reason = fmt.Sprintf("Init:Signal:%d", t.Signal)
			default:
				reason = fmt.Sprintf("Init:ExitCode:%d", t.ExitCode)
			}
		case c.State.Waiting != nil && c.State.Waiting.Reason != "" && c.State.Waiting.Reason != "PodInitializing":
			reason = "Init:" + c.State.Waiting.Reason
		default:
			reason = fmt.Sprintf("Init:%d/%d", i, len(p.Spec.InitContainers))
		}
		initializing = true
		break
	}

	if !initializing {
		hasRunning := false
		for i := len(p.Status.ContainerStatuses) - 1; i >= 0; i-- {
			c := p.Status.ContainerStatuses[i]
			switch {
			case c.State.Waiting != nil && c.State.Waiting.Reason != "":
				reason = c.State.Waiting.Reason
			case c.State.Terminated != nil && c.State.Terminated.Reason != "":
				reason = c.State.Terminated.Reason
			case c.State.Terminated != nil && c.State.Terminated.Signal != 0:
				reason = fmt.Sprintf("Signal:%d", c.State.Terminated.Signal)
			case c.State.Terminated != nil:
				reason = fmt.Sprintf("ExitCode:%d", c.State.Terminated.ExitCode)
			case c.Ready && c.State.Running != nil:
				hasRunning = true
			}
		}
		if reason == "Completed" && hasRunning {
			reason = "Running"
		}
	}

	if p.DeletionTimestamp != nil {
		if p.Status.Reason == "NodeLost" {
			return "Unknown"
		}
		return "Terminating"
	}
	return reason
}
