package ui

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/seneralkan/kubemon/internal/model"
)

// severity classifies a pod status for coloring and change reporting.
type severity int

const (
	sevOK   severity = iota // running and ready
	sevWarn                 // on its way, or running but not ready
	sevBad                  // broken until someone acts
)

// badReasons are statuses (and container reasons) that will not fix themselves.
var badReasons = map[string]bool{
	"CrashLoopBackOff":           true,
	"Error":                      true,
	"OOMKilled":                  true,
	"ImagePullBackOff":           true,
	"ErrImagePull":               true,
	"InvalidImageName":           true,
	"CreateContainerConfigError": true,
	"CreateContainerError":       true,
	"RunContainerError":          true,
	"StartError":                 true,
	"ContainerStatusUnknown":     true,
	"Evicted":                    true,
	"Failed":                     true,
}

// statusSeverity classifies a kubectl-style display status.
func statusSeverity(status string) severity {
	isInit := false
	if rest, ok := strings.CutPrefix(status, "Init:"); ok {
		status, isInit = rest, true
	}
	switch {
	case badReasons[status], strings.HasPrefix(status, "ExitCode:"), strings.HasPrefix(status, "Signal:"):
		return sevBad
	case status == "Running" && !isInit:
		return sevOK
	default: // Pending, ContainerCreating, PodInitializing, Terminating, Init:N/M, unknown
		return sevWarn
	}
}

// podSeverity is statusSeverity, downgraded to warn for a running pod that is not ready.
func podSeverity(p model.Pod) severity {
	s := statusSeverity(p.Status)
	if s == sevOK && !p.Healthy() {
		return sevWarn
	}
	return s
}

// age formats a duration the way kubectl's AGE column does, coarsely.
func age(d time.Duration) string {
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// since is age(now - t), or "-" for an unset time.
func since(t, now time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return age(now.Sub(t))
}

// shortHash trims a revision hash for display.
func shortHash(h string) string {
	if h == "" {
		return "-"
	}
	if len(h) > 10 {
		return h[:10]
	}
	return h
}

// imageTag drops the registry and repository path: "reg.io/team/app:v1.2" ->
// "app:v1.2". Digests are shortened: "app@sha256:3f2a...".
func imageTag(image string) string {
	name := image[strings.LastIndex(image, "/")+1:]
	if i := strings.Index(name, "@"); i >= 0 {
		digest := strings.TrimPrefix(name[i+1:], "sha256:")
		if len(digest) > 12 {
			digest = digest[:12]
		}
		return name[:i] + "@" + digest
	}
	if !strings.Contains(name, ":") {
		return name + ":latest"
	}
	return name
}

// imageTags joins imageTag of every image.
func imageTags(images []string) string {
	tags := make([]string, len(images))
	for i, img := range images {
		tags[i] = imageTag(img)
	}
	return strings.Join(tags, ", ")
}

// lastTermination is the most recent previous termination of any container in
// the pod: the reason it last restarted.
func lastTermination(p model.Pod) *model.Termination {
	var last *model.Termination
	for _, c := range p.Containers {
		if c.Last != nil && (last == nil || c.Last.FinishedAt.After(last.FinishedAt)) {
			last = c.Last
		}
	}
	return last
}

// terminationText is "OOMKilled, exit 137".
func terminationText(t *model.Termination) string {
	reason := t.Reason
	if reason == "" {
		reason = "terminated"
	}
	return fmt.Sprintf("%s, exit %d", reason, t.ExitCode)
}

// restartText is "0", or "3 · OOMKilled 2m ago".
func restartText(p model.Pod, now time.Time) string {
	t := lastTermination(p)
	if p.Restarts == 0 || t == nil {
		return fmt.Sprint(p.Restarts)
	}
	s := fmt.Sprintf("%d · %s", p.Restarts, t.Reason)
	if !t.FinishedAt.IsZero() {
		s += " " + age(now.Sub(t.FinishedAt)) + " ago"
	}
	return s
}

// problem explains why the pod is not ready, from its first container that is
// waiting, failed or running unready. Empty when no container says anything.
func problem(p model.Pod) string {
	for _, c := range p.Containers {
		switch {
		case c.Ready:
		case c.Init && (c.Started || c.Current != nil && c.Current.ExitCode == 0):
			// sidecar running, or init container finished fine
		case c.State == "Running":
			return c.Name + ": running, not ready"
		case c.Reason != "" || c.Message != "":
			s := c.Name + ": " + strings.Join(nonEmpty(c.Reason, sanitize(c.Message)), ": ")
			if c.Message == "" && c.Last != nil {
				s += " (last: " + terminationText(c.Last) + ")"
			}
			return s
		}
	}
	return ""
}

func nonEmpty(ss ...string) []string {
	var out []string
	for _, s := range ss {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// stepText is "step 3/8 · setWeight: 40", or "" without canary steps.
func stepText(r *model.RolloutStatus) string {
	if r == nil || r.TotalSteps == 0 {
		return ""
	}
	s := fmt.Sprintf("step %d/%d", min(r.CurrentStep+1, r.TotalSteps), r.TotalSteps)
	if r.StepDesc != "" {
		s += " · " + r.StepDesc
	}
	return s
}

// isCanary reports whether traffic weight is meaningful.
func isCanary(r *model.RolloutStatus) bool {
	return r != nil && (r.Strategy == "Canary" || r.TotalSteps > 0)
}

// targetText is "Rollout shop/checkout", or "selector app=x in shop".
func targetText(t model.Target) string {
	if t.Kind == "" {
		return fmt.Sprintf("selector %s in %s", t.Selector, t.Namespace)
	}
	return fmt.Sprintf("%s %s/%s", t.Kind, t.Namespace, t.Name)
}

// visibleRevision hides old ReplicaSets that are scaled to zero.
func visibleRevision(r model.Revision) bool {
	return r.Desired > 0 || r.Current > 0 || (r.Role != model.RoleOld && r.Role != "")
}

// eventTracker dedupes events by object+reason+message and counts warnings.
type eventTracker struct {
	seen     map[string]int32
	warnings int
}

// observe returns the events that are new or whose Count went up.
func (t *eventTracker) observe(events []model.Event) []model.Event {
	if t.seen == nil {
		t.seen = map[string]int32{}
	}
	var fresh []model.Event
	for _, e := range events {
		key := e.Object + "\x00" + e.Reason + "\x00" + e.Message
		count, ok := t.seen[key]
		if ok && e.Count <= count {
			continue
		}
		if !ok && e.Type == "Warning" {
			t.warnings++
		}
		t.seen[key] = e.Count
		fresh = append(fresh, e)
	}
	return fresh
}

// eventText is "Pod/x BackOff: Back-off restarting failed container (x5)".
func eventText(e model.Event) string {
	s := e.Object + " " + e.Reason
	if e.Message != "" {
		s += ": " + sanitize(e.Message)
	}
	if e.Count > 1 {
		s += fmt.Sprintf(" (x%d)", e.Count)
	}
	return s
}

// summary is the one-line final state, shared by the plain and TUI renderers.
func summary(s *model.Snapshot, warnings int) string {
	if s == nil {
		return "final: no data received"
	}
	phase := "-"
	var ready, desired int
	if r := s.Rollout; r != nil {
		phase = r.Phase
		ready, desired = int(r.Ready), int(r.Desired)
	} else {
		for _, p := range s.Pods {
			if p.Deleting {
				continue
			}
			desired++
			if p.Healthy() {
				ready++
			}
		}
	}
	var restarts int32
	for _, p := range s.Pods {
		restarts += p.Restarts
	}
	return fmt.Sprintf("final %s: %s · %d/%d ready · %d pods · %d restarts · %d warnings",
		targetText(s.Target), phase, ready, desired, len(s.Pods), restarts, warnings)
}

// sanitize makes untrusted text (container logs, event and container
// messages) safe to print on a terminal: it drops escape sequences (CSI, OSC
// and other ESC forms) and C0/C1 control characters, expands tabs to four
// spaces and turns newlines into spaces.
func sanitize(s string) string {
	clean := true
	for i := 0; i < len(s) && clean; i++ {
		clean = s[i] >= 0x20 && s[i] < 0x7f
	}
	if clean {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == 0x1b:
			i = skipEscape(s, i)
		case c == '\t':
			b.WriteString("    ")
			i++
		case c == '\n':
			b.WriteByte(' ')
			i++
		case c < 0x20 || c == 0x7f:
			i++
		case c < utf8.RuneSelf:
			b.WriteByte(c)
			i++
		default:
			r, n := utf8.DecodeRuneInString(s[i:])
			if r < 0x80 || r > 0x9f { // C1 controls (e.g. U+009B CSI) are dropped
				b.WriteRune(r) // invalid bytes become U+FFFD
			}
			i += n
		}
	}
	return b.String()
}

// skipEscape returns the index just past the escape sequence starting at s[i] (an ESC).
func skipEscape(s string, i int) int {
	i++
	if i >= len(s) {
		return i
	}
	switch s[i] {
	case '[': // CSI: parameter and intermediate bytes, then one final byte
		i++
		for i < len(s) && s[i] >= 0x20 && s[i] <= 0x3f {
			i++
		}
		if i < len(s) && s[i] >= 0x40 && s[i] <= 0x7e {
			i++
		}
		return i
	case ']', 'P', 'X', '^', '_': // OSC, DCS, SOS, PM, APC: a string ended by BEL or ST (ESC \)
		for i++; i < len(s); i++ {
			if s[i] == 0x07 {
				return i + 1
			}
			if s[i] == 0x1b {
				if i+1 < len(s) && s[i+1] == '\\' {
					return i + 2
				}
				return i // an unterminated string: let the next ESC start over
			}
		}
		return i
	default: // two-byte (or intermediate + final) sequences like ESC c, ESC ( B
		for i < len(s) && s[i] >= 0x20 && s[i] <= 0x2f {
			i++
		}
		if i < len(s) && s[i] >= 0x30 && s[i] <= 0x7e {
			i++
		}
		return i
	}
}

// analysisText summarizes an AnalysisRun: phase, then each metric's latest
// value with its successful (✓) and failed (✗) measurement counts.
func analysisText(a model.Analysis) string {
	parts := []string{"analysis " + a.Phase}
	for _, m := range a.Metrics {
		t := m.Name
		if m.Value != "" {
			t += " " + sanitize(m.Value)
		}
		t += fmt.Sprintf(" ✓%d", m.Successful)
		if m.Failed > 0 {
			t += fmt.Sprintf(" ✗%d", m.Failed)
		}
		parts = append(parts, t)
	}
	s := strings.Join(parts, " · ")
	if a.Message != "" {
		s += " — " + sanitize(a.Message)
	}
	return s
}

// analysisSeverity colors an AnalysisRun by its phase.
func analysisSeverity(phase string) severity {
	switch phase {
	case "Successful":
		return sevOK
	case "Failed", "Error", "Inconclusive":
		return sevBad
	}
	return sevWarn
}
