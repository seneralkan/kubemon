package ui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/seneralkan/kubemon/internal/logs"
	"github.com/seneralkan/kubemon/internal/model"
)

func newTestTUI(width, height int) *tuiModel {
	m := newTUI(true, t0)
	m.now = at(2*60 + 13)
	m.Update(tea.WindowSizeMsg{Width: width, Height: height})
	return m
}

func key(m *tuiModel, k string) tea.Cmd {
	var msg tea.KeyMsg
	switch k {
	case "tab":
		msg = tea.KeyMsg{Type: tea.KeyTab}
	case "down":
		msg = tea.KeyMsg{Type: tea.KeyDown}
	case "ctrl+c":
		msg = tea.KeyMsg{Type: tea.KeyCtrlC}
	default:
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
	}
	_, cmd := m.Update(msg)
	return cmd
}

func assertContains(t *testing.T, view string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(view, w) {
			t.Errorf("view missing %q:\n%s", w, view)
		}
	}
}

func TestTUIRendersRelease(t *testing.T) {
	m := newTestTUI(160, 60)
	s := canarySnapshot(0)
	s.Pods[0] = crashingPod("checkout-abc12-x", "abc12", 3, at(-120))
	s.Events = append(s.Events, model.Event{Time: at(-1), Type: "Warning", Reason: "BackOff", Message: "Back-off restarting", Object: "Pod/checkout-abc12-x", Count: 4})
	pending := model.Pod{Name: "checkout-abc12-y", Hash: "abc12", Phase: "Pending", Status: "Pending", Total: 1, Created: at(0)}
	s.Pods = append(s.Pods, pending)
	s.Events = append(s.Events, model.Event{Type: "Warning", Reason: "FailedScheduling", Message: "0/3 nodes are available", Object: "Pod/checkout-abc12-y", Count: 1})
	m.Update(snapMsg(s))
	m.Update(logMsg(logs.Line{Pod: "checkout-abc12-x", Container: "app", Text: "panic: oom", Previous: true}))
	m.Update(logMsg(logs.Line{Pod: "checkout-abc12-x", Container: "app", Text: "listening on :8080"}))

	v := m.View()
	assertContains(t, v,
		"Rollout shop/checkout", "Canary", "Progressing", "elapsed 2m13s",
		"step 3/8 · setWeight: 40 · weight 40%",
		"REVISIONS", "canary", "abc12", "#12", "checkout:v2", "0/1",
		"── canary abc12 #12", "── stable def34 #11",
		"CrashLoopBackOff", "3 · OOMKilled 4m ago",
		"└ app: CrashLoopBackOff (last: OOMKilled, exit 137)",
		"└ FailedScheduling: 0/3 nodes are available", // no container info: falls back to the pod's warning event
		"BackOff: Back-off restarting (x4)", "Scheduled: assigned to node-1",
		"[checkout-abc12-x/app] (previous) panic: oom", "[checkout-abc12-x/app] listening on :8080",
	)
	if strings.Contains(v, "old99") {
		t.Errorf("scaled-down old revision should be hidden:\n%s", v)
	}
	if strings.Index(v, "checkout-abc12-x") > strings.Index(v, "checkout-def34-a") {
		t.Errorf("newest revision's pods should come first:\n%s", v)
	}

	key(m, "e")
	v = m.View()
	assertContains(t, v, "EVENTS (warnings only)", "BackOff")
	if strings.Contains(v, "Scheduled: assigned") {
		t.Errorf("warnings-only should hide normal events:\n%s", v)
	}
}

func TestTUIFitsHeightAndScrolls(t *testing.T) {
	m := newTestTUI(100, 20)
	s := canarySnapshot(0)
	for i := range 30 {
		s.Pods = append(s.Pods, runningPod(fmt.Sprintf("checkout-def34-p%02d", i), "def34"))
	}
	m.Update(snapMsg(s))
	for i := range 40 {
		m.Update(logMsg(logs.Line{Pod: "p", Container: "app", Text: fmt.Sprintf("line %d", i)}))
	}

	v := m.View()
	if n := strings.Count(v, "\n") + 1; n > 20 {
		t.Errorf("view has %d lines, want <= 20:\n%s", n, v)
	}
	assertContains(t, v, "▸ PODS 33 · 1–", "line 39") // pods from the top, logs follow the tail
	if strings.Contains(v, "line 0\n") {
		t.Errorf("old log lines should be scrolled out:\n%s", v)
	}

	key(m, "down")
	assertContains(t, m.View(), "▸ PODS 33 · 2–")

	key(m, "tab")
	key(m, "tab")
	v = m.View()
	assertContains(t, v, "▸ LOGS", "  PODS 33")
	key(m, "pgup")
	if strings.Contains(m.View(), "line 39") {
		t.Errorf("pgup on logs should scroll away from the tail:\n%s", m.View())
	}
	for _, line := range strings.Split(m.View(), "\n") {
		if w := len([]rune(line)); w > 100 {
			t.Errorf("line wider than terminal (%d): %q", w, line)
		}
	}
}

func TestTUILogRingBuffer(t *testing.T) {
	m := newTestTUI(100, 40)
	for i := range logCap + 100 {
		m.Update(logMsg(logs.Line{Text: fmt.Sprint(i)}))
	}
	if len(m.logs) != logCap || m.logs[0].Text != "100" {
		t.Errorf("ring buffer len=%d first=%q", len(m.logs), m.logs[0].Text)
	}
}

func TestTUIQuits(t *testing.T) {
	for _, k := range []string{"q", "ctrl+c"} {
		if cmd := key(newTestTUI(80, 20), k); cmd == nil || cmd() != tea.Quit() {
			t.Errorf("%s should quit", k)
		}
	}
	m := newTestTUI(80, 20)
	m.Update(snapMsg(canarySnapshot(0)))
	if _, cmd := m.Update(doneMsg{}); cmd == nil || cmd() != tea.Quit() {
		t.Error("doneMsg should quit")
	}
	if got := summary(m.snap, m.events.warnings); !strings.HasPrefix(got, "final Rollout shop/checkout: Progressing") {
		t.Errorf("summary = %q", got)
	}
}

func TestTUITickUpdatesAges(t *testing.T) {
	m := newTestTUI(120, 40)
	m.Update(snapMsg(canarySnapshot(0)))
	_, cmd := m.Update(tickMsg(at(5 * 60)))
	if cmd == nil {
		t.Error("tick should schedule the next tick")
	}
	assertContains(t, m.View(), "elapsed 5m0s", "checkout-abc12-x  ContainerCreating  0/1  0  5m")
}

func TestTUILogBatchRendersAll(t *testing.T) {
	m := newTestTUI(120, 40)
	m.Update(snapMsg(canarySnapshot(0)))
	m.Update(logBatchMsg{
		{Pod: "p", Container: "app", Text: "first"},
		{Pod: "p", Container: "app", Text: "second"},
		{Pod: "p", Container: "app", Text: "third", Previous: true},
	})
	assertContains(t, m.View(), "[p/app] first", "[p/app] second", "[p/app] (previous) third")

	m.Update(logMsg(logs.Line{Pod: "p", Container: "app", Text: "logs unavailable: forbidden", Err: true}))
	assertContains(t, m.View(), "! [p/app] logs unavailable: forbidden")
}

func TestTUIVisibleWindowFollowsTail(t *testing.T) {
	m := newTestTUI(100, 30)
	m.Update(snapMsg(canarySnapshot(0)))
	batch := make(logBatchMsg, logCap+20)
	for i := range batch {
		batch[i] = logs.Line{Pod: "p", Container: "app", Text: fmt.Sprintf("line %03d", i)}
	}
	m.Update(batch)
	if len(m.logs) != logCap || m.logs[0].Text != "line 020" {
		t.Fatalf("ring buffer len=%d first=%q", len(m.logs), m.logs[0].Text)
	}

	v := m.View()
	assertContains(t, v, fmt.Sprintf("line %03d", logCap+19), fmt.Sprintf("of %d", logCap))
	if strings.Contains(v, "line 020") {
		t.Errorf("only the tail should be rendered:\n%s", v)
	}
	lp := m.panels()[panelLogs]
	if got := lp.render(lp.n-3, lp.n); len(got) != 3 || !strings.Contains(got[2], fmt.Sprintf("line %03d", logCap+19)) {
		t.Errorf("render of the last 3 lines = %q", got)
	}

	m.Update(logMsg(logs.Line{Pod: "p", Container: "app", Text: "newest"}))
	assertContains(t, m.View(), "[p/app] newest") // still stuck to the bottom
}

func TestTUISanitizesLogsAndEvents(t *testing.T) {
	m := newTestTUI(160, 40)
	s := canarySnapshot(0)
	s.Events = append(s.Events, model.Event{Type: "Warning", Reason: "BackOff", Message: "bad\x1b[2J\x1b]0;pwn\x07msg", Object: "Pod/checkout-abc12-x", Count: 1})
	m.Update(snapMsg(s))
	m.Update(logMsg(logs.Line{Pod: "p", Container: "app", Text: "\x1b[Hhello\r\tworld"}))
	v := m.View()
	assertContains(t, v, "BackOff: badmsg", "[p/app] hello    world")
	if strings.Contains(v, "\x1b[2J") || strings.Contains(v, "\x1b]0;") || strings.Contains(v, "\r") {
		t.Errorf("view contains raw control sequences: %q", v)
	}
}
