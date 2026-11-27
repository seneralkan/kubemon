package ui

import (
	"time"

	"github.com/seneralkan/kubemon/internal/model"
)

var t0 = time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)

func at(sec int) time.Time { return t0.Add(time.Duration(sec) * time.Second) }

func runningPod(name, hash string) model.Pod {
	return model.Pod{
		Name: name, Hash: hash, Phase: "Running", Status: "Running", Ready: 1, Total: 1,
		Node: "node-1", Created: t0.Add(-time.Hour),
		Containers: []model.Container{{Name: "app", Ready: true, State: "Running"}},
	}
}

func crashingPod(name, hash string, restarts int32, finished time.Time) model.Pod {
	p := runningPod(name, hash)
	p.Status, p.Ready, p.Restarts = "CrashLoopBackOff", 0, restarts
	p.Containers = []model.Container{{
		Name: "app", State: "Waiting", Reason: "CrashLoopBackOff", RestartCount: restarts,
		Last: &model.Termination{Reason: "OOMKilled", ExitCode: 137, FinishedAt: finished},
	}}
	return p
}

// canarySnapshot is a canary at step 3/8 with one new pod and two stable pods.
func canarySnapshot(sec int) model.Snapshot {
	return model.Snapshot{
		Time:   at(sec),
		Target: model.Target{Kind: "Rollout", Name: "checkout", Namespace: "shop"},
		Rollout: &model.RolloutStatus{
			Kind: "Rollout", Strategy: "Canary", Phase: model.PhaseProgressing,
			CurrentStep: 2, TotalSteps: 8, StepDesc: "setWeight: 40", Weight: 40,
			Desired: 3, Ready: 2,
		},
		Revisions: []model.Revision{
			{Hash: "abc12", Number: 12, Images: []string{"reg.io/team/checkout:v2"}, Desired: 1, Role: model.RoleCanary},
			{Hash: "def34", Number: 11, Images: []string{"reg.io/team/checkout:v1"}, Desired: 2, Ready: 2, Role: model.RoleStable},
			{Hash: "old99", Number: 10, Images: []string{"reg.io/team/checkout:v0"}, Role: model.RoleOld},
		},
		Pods: []model.Pod{
			{
				Name: "checkout-abc12-x", Hash: "abc12", Phase: "Pending", Status: "ContainerCreating", Total: 1,
				Created:    at(sec - 10),
				Containers: []model.Container{{Name: "app", State: "Waiting", Reason: "ContainerCreating"}},
			},
			runningPod("checkout-def34-a", "def34"),
			runningPod("checkout-def34-b", "def34"),
		},
		Events: []model.Event{
			{Time: at(sec - 5), Type: "Normal", Reason: "Scheduled", Message: "assigned to node-1", Object: "Pod/checkout-abc12-x", Count: 1},
		},
	}
}
