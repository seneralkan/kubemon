// Package model holds the plain data types every other package exchanges.
// No Kubernetes client code lives here: renderers and tests can build these by hand.
package model

import "time"

// Target is what is being watched.
type Target struct {
	Kind      string `json:"kind,omitempty"` // "Deployment", "Rollout", or "" when watching a bare label selector
	Name      string `json:"name,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Selector  string `json:"selector,omitempty"` // label selector string matching the target's pods and ReplicaSets
}

// Termination describes a container's previous (or current) termination.
type Termination struct {
	Reason     string    `json:"reason,omitempty"` // OOMKilled, Error, Completed, ...
	ExitCode   int32     `json:"exitCode"`
	Signal     int32     `json:"signal,omitempty"`
	Message    string    `json:"message,omitempty"`
	StartedAt  time.Time `json:"startedAt,omitzero"`
	FinishedAt time.Time `json:"finishedAt,omitzero"`
}

// Container is the live state of one container in a pod.
type Container struct {
	Name         string       `json:"name,omitempty"`
	Image        string       `json:"image,omitempty"`
	Init         bool         `json:"init,omitempty"` // init or sidecar container
	Ready        bool         `json:"ready"`
	Started      bool         `json:"started,omitempty"`
	RestartCount int32        `json:"restartCount,omitempty"`
	State        string       `json:"state"`            // Running, Waiting, Terminated
	Reason       string       `json:"reason,omitempty"` // waiting/terminated reason, e.g. CrashLoopBackOff, ImagePullBackOff
	Message      string       `json:"message,omitempty"`
	StartedAt    time.Time    `json:"startedAt,omitzero"` // when the current Running state began
	Current      *Termination `json:"current,omitempty"`  // set when State == Terminated
	Last         *Termination `json:"last,omitempty"`     // previous termination (why it restarted), nil if never restarted
}

// Pod is a pod as a developer needs to see it during a release.
type Pod struct {
	Name       string      `json:"name,omitempty"`
	Namespace  string      `json:"namespace,omitempty"`
	Node       string      `json:"node,omitempty"`
	Hash       string      `json:"hash"`   // pod-template-hash or rollouts-pod-template-hash: which revision it belongs to
	Phase      string      `json:"phase"`  // raw pod phase
	Status     string      `json:"status"` // kubectl-style display status: Running, CrashLoopBackOff, Init:0/1, Terminating, ...
	Ready      int         `json:"ready"`  // ready app containers
	Total      int         `json:"total"`  // app containers
	Restarts   int32       `json:"restarts"`
	Created    time.Time   `json:"created,omitzero"`
	Scheduled  time.Time   `json:"scheduled,omitzero"` // PodScheduled condition true, zero if not yet
	ReadyAt    time.Time   `json:"readyAt,omitzero"`   // Ready condition true, zero if not ready
	Deleting   bool        `json:"deleting,omitempty"`
	Containers []Container `json:"containers,omitempty"`
}

// Healthy reports whether the pod is running with all containers ready.
func (p Pod) Healthy() bool {
	return !p.Deleting && p.Phase == "Running" && p.Total > 0 && p.Ready == p.Total
}

// Revision roles.
const (
	RoleNew     = "new"     // newest revision (rolling update target, canary, or blue-green preview)
	RoleStable  = "stable"  // currently serving revision (Argo stable / blue-green active)
	RoleOld     = "old"     // anything else
	RoleCanary  = "canary"  // Argo canary revision
	RolePreview = "preview" // Argo blue-green preview revision
	RoleActive  = "active"  // Argo blue-green active revision
)

// Revision is one ReplicaSet of the target.
type Revision struct {
	Name      string    `json:"name,omitempty"` // ReplicaSet name
	Hash      string    `json:"hash"`
	Number    int64     `json:"number,omitempty"` // deployment.kubernetes.io/revision or rollout.argoproj.io/revision
	Images    []string  `json:"images,omitempty"`
	Desired   int32     `json:"desired"`
	Current   int32     `json:"current,omitempty"`
	Ready     int32     `json:"ready"`
	Available int32     `json:"available"`
	Created   time.Time `json:"created,omitzero"`
	Role      string    `json:"role,omitempty"` // one of the Role* constants
}

// Event is a Kubernetes event related to the target.
type Event struct {
	Time    time.Time `json:"time,omitzero"`
	Type    string    `json:"type"` // Normal, Warning
	Reason  string    `json:"reason,omitempty"`
	Message string    `json:"message,omitempty"`
	Object  string    `json:"object"` // "Pod/foo-abc12"
	Count   int32     `json:"count,omitempty"`
}

// Rollout phases.
const (
	PhaseProgressing = "Progressing"
	PhaseHealthy     = "Healthy"
	PhaseDegraded    = "Degraded"
	PhasePaused      = "Paused"
	PhaseAborted     = "Aborted"
)

// AnalysisMetric is one metric of an Argo AnalysisRun.
type AnalysisMetric struct {
	Name       string `json:"name,omitempty"`
	Phase      string `json:"phase"`
	Value      string `json:"value,omitempty"` // latest measurement value
	Successful int32  `json:"successful,omitempty"`
	Failed     int32  `json:"failed,omitempty"`
	Message    string `json:"message,omitempty"`
}

// Analysis is an Argo AnalysisRun summary.
type Analysis struct {
	Name    string           `json:"name,omitempty"`
	Phase   string           `json:"phase"` // Pending, Running, Successful, Failed, Error, Inconclusive
	Message string           `json:"message,omitempty"`
	Metrics []AnalysisMetric `json:"metrics,omitempty"`
}

// RolloutStatus is the release progress of a Deployment or Argo Rollout.
type RolloutStatus struct {
	Kind     string `json:"kind,omitempty"`     // Deployment, Rollout
	Strategy string `json:"strategy,omitempty"` // RollingUpdate, Recreate, Canary, BlueGreen
	Phase    string `json:"phase"`              // one of the Phase* constants
	Message  string `json:"message,omitempty"`

	Generation         int64 `json:"generation,omitempty"`
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	Desired            int32 `json:"desired"`
	Updated            int32 `json:"updated"`
	Ready              int32 `json:"ready"`
	Available          int32 `json:"available"`

	// Argo canary
	CurrentStep int    `json:"currentStep,omitempty"` // 0-based index of the current step; == TotalSteps when all steps done
	TotalSteps  int    `json:"totalSteps,omitempty"`
	StepDesc    string `json:"stepDesc,omitempty"` // e.g. "setWeight: 20", "pause: 5m", "analysis"
	Weight      int32  `json:"weight,omitempty"`   // current canary traffic weight

	Paused      bool   `json:"paused,omitempty"`
	StableHash  string `json:"stableHash,omitempty"`
	CurrentHash string `json:"currentHash,omitempty"` // hash of the newest revision

	// Argo blue-green
	ActiveService  string `json:"activeService,omitempty"`
	PreviewService string `json:"previewService,omitempty"`

	Analysis []Analysis `json:"analysis,omitempty"`
}

// Done reports a finished, healthy release.
func (r *RolloutStatus) Done() bool { return r != nil && r.Phase == PhaseHealthy }

// Failed reports a release that will not finish on its own.
func (r *RolloutStatus) Failed() bool {
	return r != nil && (r.Phase == PhaseDegraded || r.Phase == PhaseAborted)
}

// Snapshot is the full state at one point in time.
type Snapshot struct {
	Time      time.Time      `json:"time,omitzero"`
	Target    Target         `json:"target"`
	Rollout   *RolloutStatus `json:"rollout,omitempty"`   // nil when watching a bare selector
	Revisions []Revision     `json:"revisions,omitempty"` // newest first
	Pods      []Pod          `json:"pods,omitempty"`      // newest first
	Events    []Event        `json:"events,omitempty"`    // oldest first
}
