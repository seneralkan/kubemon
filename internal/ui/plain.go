package ui

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/seneralkan/kubemon/internal/logs"
	"github.com/seneralkan/kubemon/internal/model"
)

// plain prints a compact header for the first snapshot and then one
// timestamped line per change, so CI logs read as a release timeline.
type plain struct {
	w      io.Writer
	now    func() time.Time
	last   *model.Snapshot
	events eventTracker
}

func newPlain(w io.Writer, now func() time.Time) *plain {
	return &plain{w: w, now: now}
}

func (p *plain) printf(t time.Time, format string, args ...any) {
	if t.IsZero() {
		t = p.now()
	}
	fmt.Fprintf(p.w, "%s %s\n", t.Format("15:04:05"), fmt.Sprintf(format, args...))
}

func (p *plain) snapshot(s model.Snapshot) {
	if p.last == nil {
		p.header(s)
	} else {
		p.rolloutChanges(s.Time, p.last.Rollout, s.Rollout)
		p.revisionChanges(s.Time, p.last.Revisions, s.Revisions)
		p.podChanges(s.Time, p.last.Pods, s.Pods)
	}
	for _, e := range p.events.observe(s.Events) {
		t := e.Time
		if t.IsZero() {
			t = s.Time
		}
		prefix := "event"
		if e.Type == "Warning" {
			prefix = "WARN"
		}
		p.printf(t, "%s %s", prefix, eventText(e))
	}
	p.last = &s
}

func (p *plain) log(l logs.Line) {
	if l.Err { // Text is "logs unavailable: <why>"
		p.printf(l.Time, "WARN [%s/%s] %s", l.Pod, l.Container, sanitize(l.Text))
		return
	}
	prev := ""
	if l.Previous {
		prev = "(previous) "
	}
	p.printf(l.Time, "[%s/%s] %s%s", l.Pod, l.Container, prev, sanitize(l.Text))
}

func (p *plain) final() {
	p.printf(p.now(), "%s", summary(p.last, p.events.warnings))
}

func (p *plain) header(s model.Snapshot) {
	parts := []string{targetText(s.Target)}
	if r := s.Rollout; r != nil {
		parts = append(parts, r.Strategy, r.Phase, stepText(r))
		if isCanary(r) {
			parts = append(parts, fmt.Sprintf("weight %d%%", r.Weight))
		}
		if r.Paused {
			parts = append(parts, "paused")
		}
	}
	p.printf(s.Time, "%s", strings.Join(nonEmpty(parts...), " · "))
	if s.Rollout != nil && s.Rollout.Message != "" {
		p.printf(s.Time, "message: %s", s.Rollout.Message)
	}
	if s.Rollout != nil {
		for _, a := range s.Rollout.Analysis {
			p.printf(s.Time, "%s", analysisText(a))
		}
	}
	for _, r := range s.Revisions {
		if visibleRevision(r) {
			p.printf(s.Time, "revision %s", revisionText(r))
		}
	}
	for _, pod := range s.Pods {
		p.printf(s.Time, "pod %s", podText(pod))
	}
}

func (p *plain) rolloutChanges(t time.Time, a, b *model.RolloutStatus) {
	if b == nil {
		return
	}
	if a == nil {
		a = &model.RolloutStatus{}
	}
	var ch []string
	if a.Phase != b.Phase {
		ch = append(ch, fmt.Sprintf("phase %s → %s", a.Phase, b.Phase))
	}
	if st := stepText(b); st != stepText(a) {
		ch = append(ch, st)
	}
	if isCanary(b) && a.Weight != b.Weight {
		ch = append(ch, fmt.Sprintf("weight %d%% → %d%%", a.Weight, b.Weight))
	}
	switch {
	case b.Paused && !a.Paused:
		ch = append(ch, "paused")
	case a.Paused && !b.Paused:
		ch = append(ch, "resumed")
	}
	if a.Message != b.Message && b.Message != "" {
		ch = append(ch, "message: "+b.Message)
	}
	if len(ch) > 0 {
		p.printf(t, "rollout %s", strings.Join(nonEmpty(ch...), " · "))
	}
	before := map[string]string{}
	for _, an := range a.Analysis {
		before[an.Name] = analysisText(an)
	}
	for _, an := range b.Analysis {
		if txt := analysisText(an); txt != before[an.Name] {
			p.printf(t, "%s", txt)
		}
	}
}

func (p *plain) revisionChanges(t time.Time, prev, cur []model.Revision) {
	old := map[string]model.Revision{}
	for _, r := range prev {
		old[r.Hash] = r
	}
	for _, r := range cur {
		o, ok := old[r.Hash]
		if !ok || !visibleRevision(o) {
			if visibleRevision(r) {
				p.printf(t, "revision %s", revisionText(r))
			}
			continue
		}
		var ch []string
		if o.Role != r.Role {
			ch = append(ch, fmt.Sprintf("role %s → %s", o.Role, r.Role))
		}
		if o.Ready != r.Ready || o.Desired != r.Desired {
			ch = append(ch, fmt.Sprintf("ready %d/%d → %d/%d", o.Ready, o.Desired, r.Ready, r.Desired))
		}
		if len(ch) > 0 {
			p.printf(t, "revision %s %s", shortHash(r.Hash), strings.Join(ch, " · "))
		}
	}
}

func (p *plain) podChanges(t time.Time, prev, cur []model.Pod) {
	old := map[string]model.Pod{}
	for _, q := range prev {
		old[q.Name] = q
	}
	for _, q := range cur {
		o, ok := old[q.Name]
		delete(old, q.Name)
		if !ok {
			p.printf(t, "pod %s added", podText(q))
			continue
		}
		var ch []string
		if o.Status != q.Status {
			ch = append(ch, fmt.Sprintf("%s → %s", o.Status, q.Status))
		}
		if o.Ready != q.Ready || o.Total != q.Total {
			ch = append(ch, fmt.Sprintf("ready %d/%d → %d/%d", o.Ready, o.Total, q.Ready, q.Total))
		}
		if q.Restarts > o.Restarts {
			r := "restarted"
			if last := lastTermination(q); last != nil {
				r += " (" + terminationText(last) + ")"
			}
			ch = append(ch, fmt.Sprintf("%s, %d total", r, q.Restarts))
		}
		if len(ch) == 0 {
			continue
		}
		line := "pod " + q.Name + " " + strings.Join(ch, " · ")
		if why := problem(q); o.Status != q.Status && podSeverity(q) != sevOK && why != "" {
			line += " — " + why
		}
		p.printf(t, "%s", line)
	}
	for _, q := range prev { // keep snapshot order for removals
		if _, gone := old[q.Name]; gone {
			p.printf(t, "pod %s deleted", q.Name)
		}
	}
}

// revisionText is "canary 7d9f8c6b5 #12 app:v1.2.3 ready 1/2".
func revisionText(r model.Revision) string {
	parts := []string{r.Role, shortHash(r.Hash)}
	if r.Number > 0 {
		parts = append(parts, fmt.Sprintf("#%d", r.Number))
	}
	parts = append(parts, imageTags(r.Images), fmt.Sprintf("ready %d/%d", r.Ready, r.Desired))
	return strings.Join(nonEmpty(parts...), " ")
}

// podText is "checkout-7d9f8c6b5-x2x9q [7d9f8c6b5] Running 1/1 ready, 0 restarts, node-1",
// followed by why it is not ready when known.
func podText(q model.Pod) string {
	s := fmt.Sprintf("%s [%s] %s %d/%d ready, %d restarts", q.Name, shortHash(q.Hash), q.Status, q.Ready, q.Total, q.Restarts)
	if q.Node != "" {
		s += ", " + q.Node
	}
	if why := problem(q); podSeverity(q) != sevOK && why != "" {
		s += " — " + why
	}
	return s
}
