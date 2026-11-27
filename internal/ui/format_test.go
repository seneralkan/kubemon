package ui

import (
	"testing"
	"time"

	"github.com/seneralkan/kubemon/internal/model"
)

func TestStatusSeverity(t *testing.T) {
	tests := map[string]severity{
		"Running":                    sevOK,
		"CrashLoopBackOff":           sevBad,
		"Error":                      sevBad,
		"OOMKilled":                  sevBad,
		"ImagePullBackOff":           sevBad,
		"ErrImagePull":               sevBad,
		"CreateContainerConfigError": sevBad,
		"Init:Error":                 sevBad,
		"Init:CrashLoopBackOff":      sevBad,
		"Init:ExitCode:1":            sevBad,
		"Signal:9":                   sevBad,
		"Pending":                    sevWarn,
		"ContainerCreating":          sevWarn,
		"PodInitializing":            sevWarn,
		"Init:0/2":                   sevWarn,
		"Terminating":                sevWarn,
		"Completed":                  sevWarn,
	}
	for status, want := range tests {
		if got := statusSeverity(status); got != want {
			t.Errorf("statusSeverity(%q) = %d, want %d", status, got, want)
		}
	}
}

func TestPodSeverity(t *testing.T) {
	p := runningPod("a", "h")
	if got := podSeverity(p); got != sevOK {
		t.Errorf("healthy pod = %d, want ok", got)
	}
	p.Ready = 0
	if got := podSeverity(p); got != sevWarn {
		t.Errorf("running, not ready pod = %d, want warn", got)
	}
}

func TestImageTag(t *testing.T) {
	tests := map[string]string{
		"reg.io/team/app:v1.2":                 "app:v1.2",
		"localhost:5000/app":                   "app:latest",
		"nginx":                                "nginx:latest",
		"reg.io/app@sha256:0123456789abcdef00": "app@0123456789ab",
	}
	for in, want := range tests {
		if got := imageTag(in); got != want {
			t.Errorf("imageTag(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAge(t *testing.T) {
	tests := map[time.Duration]string{
		-time.Second:     "0s",
		45 * time.Second: "45s",
		3 * time.Minute:  "3m",
		5 * time.Hour:    "5h",
		72 * time.Hour:   "3d",
	}
	for d, want := range tests {
		if got := age(d); got != want {
			t.Errorf("age(%s) = %q, want %q", d, got, want)
		}
	}
}

func TestProblem(t *testing.T) {
	p := crashingPod("a", "h", 3, t0)
	if got, want := problem(p), "app: CrashLoopBackOff (last: OOMKilled, exit 137)"; got != want {
		t.Errorf("problem = %q, want %q", got, want)
	}
	p.Containers = []model.Container{
		{Name: "init", Init: true, State: "Terminated", Current: &model.Termination{Reason: "Completed"}},
		{Name: "app", State: "Waiting", Reason: "CreateContainerConfigError", Message: `secret "db" not found`},
	}
	if got, want := problem(p), `app: CreateContainerConfigError: secret "db" not found`; got != want {
		t.Errorf("problem = %q, want %q", got, want)
	}
}

func TestEventTrackerDedupes(t *testing.T) {
	var tr eventTracker
	e := model.Event{Type: "Warning", Reason: "BackOff", Message: "m", Object: "Pod/a", Count: 1}
	if got := tr.observe([]model.Event{e}); len(got) != 1 {
		t.Fatalf("first observe = %v", got)
	}
	if got := tr.observe([]model.Event{e}); len(got) != 0 {
		t.Errorf("repeat should be deduped, got %v", got)
	}
	e.Count = 2
	if got := tr.observe([]model.Event{e}); len(got) != 1 {
		t.Errorf("count increase should re-report, got %v", got)
	}
	if tr.warnings != 1 {
		t.Errorf("warnings = %d, want 1", tr.warnings)
	}
}

func TestSanitize(t *testing.T) {
	tests := []struct{ in, want string }{
		{"plain text", "plain text"},
		{"a\tb", "a    b"},
		{"progress 10%\rprogress 20%", "progress 10%progress 20%"},
		{"line one\nline two", "line one line two"},
		{"\x1b[31mred\x1b[0m", "red"},
		{"\x1b[2J\x1b[Hcleared", "cleared"},
		{"\x1b[?25lhidden cursor", "hidden cursor"},
		{"\x1b]0;evil title\x07after", "after"},
		{"\x1b]8;;http://x\x1b\\link\x1b]8;;\x1b\\", "link"},
		{"\x1bPdcs payload\x1b\\ok", "ok"},
		{"\x1bcreset", "reset"},
		{"\x1b(Bcharset", "charset"},
		{"bell\x07 nul\x00 del\x7f", "bell nul del"},
		{"c1 \u009b31mcsi", "c1 31mcsi"},
		{"trailing esc\x1b", "trailing esc"},
		{"unterminated \x1b]0;title", "unterminated "},
		{"héllo 世界 ✓", "héllo 世界 ✓"},
	}
	for _, tt := range tests {
		if got := sanitize(tt.in); got != tt.want {
			t.Errorf("sanitize(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
