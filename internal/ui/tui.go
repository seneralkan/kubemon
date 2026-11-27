package ui

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/seneralkan/kubemon/internal/logs"
	"github.com/seneralkan/kubemon/internal/model"
)

// logCap is how many log lines the TUI keeps.
const logCap = 500

// logLinger is how long the feeder waits to batch log lines into one message:
// bubbletea renders the view after every message.
const logLinger = 50 * time.Millisecond

// Panels, in tab order.
const (
	panelPods = iota
	panelEvents
	panelLogs
)

// Messages fed into the TUI.
type (
	snapMsg     model.Snapshot
	logMsg      logs.Line
	logBatchMsg []logs.Line
	tickMsg     time.Time
	doneMsg     struct{} // input ended: quit
)

var (
	styleBold   = lipgloss.NewStyle().Bold(true)
	styleDim    = lipgloss.NewStyle().Faint(true)
	styleGreen  = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styleYellow = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	styleRed    = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	podPalette  = []lipgloss.Color{"6", "5", "4", "2", "3", "14", "13", "12"}
)

func sevStyle(s severity) lipgloss.Style {
	switch s {
	case sevOK:
		return styleGreen
	case sevWarn:
		return styleYellow
	default:
		return styleRed
	}
}

func runTUI(ctx context.Context, o Options, in Input) error {
	p := tea.NewProgram(newTUI(o.ShowLogs, time.Now()), tea.WithAltScreen(), tea.WithOutput(o.Out))

	// The feeder stops when ctx ends (after flushing the pending snapshot) or
	// when the program exits; p.Send is a no-op once the program is gone.
	feedCtx, stop := context.WithCancel(ctx)
	fed := make(chan struct{})
	go func() {
		defer close(fed)
		pump(feedCtx, in,
			func(s model.Snapshot) { p.Send(snapMsg(s)) },
			func(b []logs.Line) { p.Send(logBatchMsg(b)) }, logLinger)
		p.Send(doneMsg{})
	}()
	final, err := p.Run()
	stop()
	<-fed

	if m, ok := final.(*tuiModel); ok {
		fmt.Fprintln(o.Out, summary(m.snap, m.events.warnings))
	}
	if errors.Is(err, tea.ErrInterrupted) {
		return nil
	}
	return err
}

type tuiModel struct {
	showLogs bool
	start    time.Time
	now      time.Time
	snap     *model.Snapshot
	events   eventTracker
	logs     []logEntry // ring buffer of the last logCap lines
	width    int
	height   int
	focus    int
	scroll   [3]int // lines away from the panel's anchor: top for pods, newest (bottom) for events and logs
	warnOnly bool
}

// logEntry is a log line with its rendering, styled once on arrival.
type logEntry struct {
	logs.Line
	view string
}

func newTUI(showLogs bool, start time.Time) *tuiModel {
	return &tuiModel{showLogs: showLogs, start: start, now: start}
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *tuiModel) Init() tea.Cmd { return tick() }

func (m *tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case snapMsg:
		s := model.Snapshot(msg)
		m.snap = &s
		m.events.observe(s.Events)
	case logMsg:
		m.addLogs(logs.Line(msg))
	case logBatchMsg:
		m.addLogs(msg...)
	case tickMsg:
		m.now = time.Time(msg)
		return m, tick()
	case doneMsg:
		return m, tea.Quit
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.KeyMsg:
		return m, m.key(msg.String())
	}
	return m, nil
}

func (m *tuiModel) addLogs(ls ...logs.Line) {
	for _, l := range ls[max(len(ls)-logCap, 0):] {
		m.logs = append(m.logs, logEntry{l, logView(l)})
	}
	if len(m.logs) > logCap {
		m.logs = m.logs[len(m.logs)-logCap:]
	}
}

func (m *tuiModel) key(k string) tea.Cmd {
	switch k {
	case "q", "ctrl+c":
		return tea.Quit
	case "tab":
		m.focus = (m.focus + 1) % len(m.panels())
	case "e":
		m.warnOnly = !m.warnOnly
		m.scroll[panelEvents] = 0
	case "up", "k":
		m.scrollBy(-1, false)
	case "down", "j":
		m.scrollBy(1, false)
	case "pgup":
		m.scrollBy(-1, true)
	case "pgdown":
		m.scrollBy(1, true)
	}
	return nil
}

// scrollBy moves the focused panel by dir lines, or by dir pages.
func (m *tuiModel) scrollBy(dir int, page bool) {
	ps := m.panels()
	h := m.heights(len(m.header()), ps)[m.focus]
	p := ps[m.focus]
	if page {
		dir *= max(1, h-1)
	}
	if p.fromBottom {
		dir = -dir
	}
	m.scroll[m.focus] = min(max(m.scroll[m.focus]+dir, 0), max(p.n-h, 0))
}

// panel is a scrollable list of n lines; only the visible ones are rendered.
type panel struct {
	title      string
	n          int
	render     func(start, end int) []string
	fromBottom bool // newest at the bottom: follow the tail by default
}

func (m *tuiModel) View() string {
	if m.snap == nil {
		return "kubemon: waiting for the first snapshot…"
	}
	out := m.header()
	ps := m.panels()
	hs := m.heights(len(out), ps)
	for i, p := range ps {
		n, h := p.n, hs[i]
		off := min(m.scroll[i], max(n-h, 0))
		start := off
		if p.fromBottom {
			start = max(n-h-off, 0)
		}
		end := min(start+h, n)

		title := p.title
		if n > h {
			title += fmt.Sprintf(" · %d–%d of %d", start+1, end, n)
		}
		if i == m.focus {
			title = styleBold.Render("▸ " + title)
		} else {
			title = styleDim.Render("  " + title)
		}
		out = append(out, title)
		out = append(out, p.render(start, end)...)
	}
	out = append(out, styleDim.Render("tab focus · ↑/↓ pgup/pgdn scroll · e warnings only · q quit"))

	if m.height > 0 && len(out) > m.height {
		out = out[:m.height]
	}
	if m.width > 0 {
		fit := lipgloss.NewStyle().MaxWidth(m.width)
		for i := range out {
			out[i] = fit.Render(out[i])
		}
	}
	return strings.Join(out, "\n")
}

// heights splits the rows left under the header between the panels: each gets
// an equal share, and rows a panel does not need go to the others in order.
func (m *tuiModel) heights(headerRows int, ps []panel) []int {
	hs := make([]int, len(ps))
	if m.height <= 0 {
		for i, p := range ps {
			hs[i] = p.n
		}
		return hs
	}
	avail := max(m.height-headerRows-len(ps)-1, 0) // panel titles and footer
	share := avail / len(ps)
	for i, p := range ps {
		hs[i] = min(p.n, share)
		avail -= hs[i]
	}
	for i, p := range ps {
		more := min(p.n-hs[i], avail)
		hs[i] += more
		avail -= more
	}
	return hs
}

func (m *tuiModel) header() []string {
	s := m.snap
	elapsed := m.now.Sub(m.start).Truncate(time.Second)
	top := []string{styleBold.Render("kubemon"), targetText(s.Target)}
	if r := s.Rollout; r != nil {
		top = append(top, r.Strategy, phaseBadge(r.Phase))
	} else {
		ready, total := 0, 0
		for _, p := range s.Pods {
			if !p.Deleting {
				total++
				if p.Healthy() {
					ready++
				}
			}
		}
		top = append(top, fmt.Sprintf("%d/%d pods ready", ready, total))
	}
	top = append(top, styleDim.Render("elapsed "+elapsed.String()))
	lines := []string{strings.Join(nonEmpty(top...), "  ")}

	if r := s.Rollout; r != nil {
		var prog []string
		if st := stepText(r); st != "" {
			prog = append(prog, st)
		}
		if isCanary(r) {
			prog = append(prog, fmt.Sprintf("weight %d%%", r.Weight))
		}
		if r.Paused {
			prog = append(prog, styleYellow.Render("paused"))
		}
		if len(prog) > 0 {
			lines = append(lines, strings.Join(prog, " · "))
		}
		if r.Message != "" {
			style := styleYellow
			if r.Phase == model.PhaseHealthy {
				style = styleDim
			}
			lines = append(lines, style.Render(r.Message))
		}
		for _, a := range r.Analysis {
			lines = append(lines, sevStyle(analysisSeverity(a.Phase)).Render(analysisText(a)))
		}
	}

	var rows [][]string
	var revs []model.Revision
	for _, r := range s.Revisions {
		if visibleRevision(r) {
			revs = append(revs, r)
			rows = append(rows, []string{r.Role, shortHash(r.Hash), revNumber(r), imageTags(r.Images), fmt.Sprintf("%d/%d", r.Ready, r.Desired)})
		}
	}
	if len(rows) > 0 {
		lines = append(lines, "", styleDim.Render("  REVISIONS"))
		align(rows)
		for i, r := range revs {
			ready := styleYellow
			if r.Ready == r.Desired {
				ready = styleGreen
			}
			c := rows[i]
			lines = append(lines, "  "+styleBold.Render(c[0])+"  "+c[1]+"  "+c[2]+"  "+c[3]+"  "+ready.Render(c[4]))
		}
	}
	return append(lines, "")
}

func phaseBadge(phase string) string {
	bg := lipgloss.Color("7")
	switch phase {
	case model.PhaseHealthy:
		bg = "2"
	case model.PhaseProgressing, model.PhasePaused:
		bg = "3"
	case model.PhaseDegraded, model.PhaseAborted:
		bg = "1"
	}
	return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(bg).Padding(0, 1).Render(phase)
}

func revNumber(r model.Revision) string {
	if r.Number == 0 {
		return ""
	}
	return fmt.Sprintf("#%d", r.Number)
}

func (m *tuiModel) panels() []panel {
	pods, evs := m.podRows(), m.visibleEvents()
	ps := []panel{
		{title: fmt.Sprintf("PODS %d", len(m.snap.Pods)), n: len(pods), render: func(a, b int) []string { return renderPodRows(pods[a:b]) }},
		{title: "EVENTS", n: len(evs), render: func(a, b int) []string { return renderEvents(evs[a:b]) }, fromBottom: true},
	}
	if m.warnOnly {
		ps[panelEvents].title += " (warnings only)"
	}
	if m.showLogs {
		ps = append(ps, panel{title: "LOGS", n: len(m.logs), render: m.renderLogs, fromBottom: true})
	}
	return ps
}

// podRow is one unstyled line of the pods panel: a revision group heading
// (group), a pod (cells), or why the pod above is not ready (why).
type podRow struct {
	group string
	cells []string
	pod   model.Pod
	why   string
}

// podRows groups pods under their revision, newest revision first.
func (m *tuiModel) podRows() []podRow {
	s := m.snap
	type group struct {
		title string
		pods  []model.Pod
	}
	byHash := map[string][]model.Pod{}
	for _, p := range s.Pods {
		byHash[p.Hash] = append(byHash[p.Hash], p)
	}
	var groups []group
	for _, r := range s.Revisions {
		if pods := byHash[r.Hash]; len(pods) > 0 {
			groups = append(groups, group{strings.Join(nonEmpty(r.Role, shortHash(r.Hash), revNumber(r)), " "), pods})
			delete(byHash, r.Hash)
		}
	}
	var other []model.Pod
	for _, p := range s.Pods {
		if _, ok := byHash[p.Hash]; ok {
			other = append(other, p)
		}
	}
	if len(other) > 0 {
		groups = append(groups, group{"other", other})
	}

	var rows []podRow
	var cells [][]string
	for _, g := range groups {
		rows = append(rows, podRow{group: g.title})
		for _, p := range g.pods {
			c := []string{p.Name, p.Status, fmt.Sprintf("%d/%d", p.Ready, p.Total), restartText(p, m.now), since(p.Created, m.now), p.Node}
			cells = append(cells, c)
			rows = append(rows, podRow{cells: c, pod: p})
			if !p.Healthy() && !p.Deleting {
				if why := m.why(p); why != "" {
					rows = append(rows, podRow{why: why})
				}
			}
		}
	}
	align(cells) // in place: rows share the cell slices
	return rows
}

func renderPodRows(rows []podRow) []string {
	lines := make([]string, len(rows))
	for i, r := range rows {
		switch c := r.cells; {
		case c != nil:
			restarts := c[3]
			if r.pod.Restarts > 0 {
				restarts = styleYellow.Render(restarts)
			}
			lines[i] = "  " + c[0] + "  " + sevStyle(podSeverity(r.pod)).Render(c[1]) + "  " + c[2] + "  " + restarts + "  " + c[4] + "  " + styleDim.Render(c[5])
		case r.why != "":
			lines[i] = styleDim.Render("    └ " + r.why)
		default:
			lines[i] = styleDim.Render("── " + r.group)
		}
	}
	return lines
}

// why explains a not-ready pod: its first problematic container, or else the
// latest warning event about it (e.g. FailedScheduling for a Pending pod).
func (m *tuiModel) why(p model.Pod) string {
	if s := problem(p); s != "" {
		return s
	}
	evs := m.snap.Events
	for i := len(evs) - 1; i >= 0; i-- {
		if e := evs[i]; e.Type == "Warning" && e.Object == "Pod/"+p.Name {
			return strings.Join(nonEmpty(e.Reason, sanitize(e.Message)), ": ")
		}
	}
	return ""
}

func (m *tuiModel) visibleEvents() []model.Event {
	if !m.warnOnly {
		return m.snap.Events
	}
	var evs []model.Event
	for _, e := range m.snap.Events {
		if e.Type == "Warning" {
			evs = append(evs, e)
		}
	}
	return evs
}

func renderEvents(evs []model.Event) []string {
	lines := make([]string, len(evs))
	for i, e := range evs {
		lines[i] = e.Time.Format("15:04:05") + " " + eventText(e)
		if e.Type == "Warning" {
			lines[i] = styleRed.Render(lines[i])
		}
	}
	return lines
}

func (m *tuiModel) renderLogs(start, end int) []string {
	lines := make([]string, end-start)
	for i, l := range m.logs[start:end] {
		lines[i] = l.view
	}
	return lines
}

// logView styles a log line for the logs panel.
func logView(l logs.Line) string {
	text := sanitize(l.Text)
	prefix := "[" + l.Pod + "/" + l.Container + "]"
	if l.Err { // Text is "logs unavailable: <why>"
		return styleRed.Render("! " + prefix + " " + text)
	}
	if l.Previous {
		return styleDim.Render(prefix + " (previous) " + text)
	}
	return lipgloss.NewStyle().Foreground(podColor(l.Pod)).Render(prefix) + " " + text
}

func podColor(pod string) lipgloss.Color {
	h := fnv.New32a()
	h.Write([]byte(pod))
	return podPalette[h.Sum32()%uint32(len(podPalette))]
}

// align pads every cell to its column's width, in place.
func align(rows [][]string) {
	var w []int
	for _, r := range rows {
		for i, c := range r {
			if i == len(w) {
				w = append(w, 0)
			}
			w[i] = max(w[i], lipgloss.Width(c))
		}
	}
	for _, r := range rows {
		for i, c := range r {
			r[i] = c + strings.Repeat(" ", w[i]-lipgloss.Width(c))
		}
	}
}
