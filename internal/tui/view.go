package tui

import (
	"fmt"
	"strings"
)

func (m model) View() string {
	if m.w <= 0 || m.h <= 0 {
		return ""
	}
	status := m.statusLines()
	head := m.headerLines()
	if m.h <= len(head)+len(status)+1 {
		return strings.Join(append(head, status...), "\n")
	}
	// A confirmation, or a daemon that is down, takes as many rows as it needs,
	// so the frame shrinks the body instead of clipping the sentence.
	bodyH := m.h - len(head) - len(status) - 1
	lines := make([]string, 0, m.h)
	lines = append(lines, head...)
	body := m.body(bodyH)
	if len(body) > bodyH {
		body = body[:bodyH]
	}
	lines = append(lines, body...)
	for len(lines) < m.h-1-len(status) {
		lines = append(lines, "")
	}
	lines = append(lines, status...)
	lines = append(lines, m.helpLine())
	// One last guard: nothing may wrap into soup, whatever the terminal size.
	for i, ln := range lines {
		lines[i] = clip(ln, m.w)
	}
	return strings.Join(lines, "\n")
}

func (m model) body(h int) []string {
	switch m.screen {
	case scrDevice:
		return m.detailBody(h)
	case scrTasks:
		return m.tasksBody(h)
	case scrJobs:
		return m.jobsBody(h)
	case scrJob:
		return m.jobBody(h)
	}
	return m.devicesBody(h)
}

func (m model) tabLine() string {
	var b strings.Builder
	b.WriteString(stTitle.Render("OPTICORE"))
	b.WriteString(stMuted.Render(" acs console"))
	b.WriteString("   ")
	for i, t := range []struct {
		key   string
		s     screen
		label string
	}{
		{"1", scrDevices, "devices"},
		{"2", scrTasks, "tasks"},
		{"3", scrJobs, "jobs"},
	} {
		if i > 0 {
			b.WriteString("  ")
		}
		active := m.screen == t.s ||
			(t.s == scrDevices && m.screen == scrDevice) ||
			(t.s == scrJobs && m.screen == scrJob)
		label := "[" + t.key + "] " + t.label
		if active {
			b.WriteString(stAccent.Render(label))
		} else {
			b.WriteString(stMuted.Render(label))
		}
	}
	if m.screen == scrDevice && m.det.device.ID != "" {
		b.WriteString(stMuted.Render(" › " + m.det.device.Serial))
	}
	if m.screen == scrJob && m.jobd.job.ID != 0 {
		b.WriteString(stMuted.Render(fmt.Sprintf(" › #%d", m.jobd.job.ID)))
	}
	if mark := m.serveMark(); mark != "" {
		b.WriteString("   ")
		b.WriteString(stMuted.Render(mark))
	}
	return clip(b.String(), m.w)
}

// headerLines is the tab bar, a daemon line when the state needs one, and the
// context line. A healthy daemon gets only the quiet mark in the tab bar.
func (m model) headerLines() []string {
	out := []string{m.tabLine()}
	switch {
	case m.serve == serveDown:
		out = append(out, clip(stBad.Render(serveDownAlert), m.w))
	case m.serveErr != nil:
		out = append(out, clip(stBad.Render(serveUnknownMark), m.w))
	}
	return append(out, m.contextLine())
}

// serveMark is the quiet, low-emphasis tab-bar mark for a serving daemon.
// Degraded states get a header line of their own instead of a clipped tail.
func (m model) serveMark() string {
	if m.serve == serveUp {
		return "● serving"
	}
	return ""
}

func (m model) contextLine() string {
	var s string
	switch m.screen {
	case scrDevice:
		s = fmt.Sprintf("device %s  ·  refreshed %s", dash(m.det.device.Serial), clock(m.det.at))
	case scrTasks:
		if m.task.deviceID == "" {
			s = "tasks  ·  no device selected"
		} else {
			s = fmt.Sprintf("tasks for %s  ·  status %s  ·  refreshed %s",
				m.task.deviceLabel, m.task.statusLabel(), clock(m.task.at))
		}
	case scrJobs:
		s = fmt.Sprintf("%d jobs  ·  refreshed %s", len(m.jobs.all), clock(m.jobs.at))
	case scrJob:
		s = fmt.Sprintf("job %d  ·  refreshed %s", m.jobd.job.ID, clock(m.jobd.at))
	default:
		s = fmt.Sprintf("%d devices", len(m.dev.all))
		if len(m.dev.view) != len(m.dev.all) {
			s += fmt.Sprintf("  ·  %d shown", len(m.dev.view))
		}
		s += "  ·  refreshed " + clock(m.dev.at)
	}
	if f := m.activeFilter(); f != nil && (f.text != "" || f.editing) {
		s += "  ·  filter " + f.text
		if f.editing {
			s += "_"
		}
	}
	return clip(stMuted.Render(s), m.w)
}

// statusLines is the bottom status area: a pending confirmation owns it until
// y or n, otherwise it is the last message or the loading line. The confirm
// prompt wraps, because the consequence of a factory reset does not fit one
// line at 80 columns and must not be cut off.
func (m model) statusLines() []string {
	if m.confirm != nil {
		out := []string{clip(stAccent.Render(m.confirm.prompt), m.w)}
		if m.serve == serveDown {
			for _, line := range wrapWords(serveWarning, m.w) {
				out = append(out, clip(stBad.Render(line), m.w))
			}
		}
		for _, line := range wrapWords(m.confirm.detail+"  [y] yes  [n] no", m.w) {
			out = append(out, clip(line, m.w))
		}
		return out
	}
	switch {
	case m.loading != "":
		return []string{clip(stMuted.Render("· "+m.loading), m.w)}
	case m.statusBad:
		return []string{clip(stBad.Render(m.status), m.w)}
	}
	return []string{clip(stMuted.Render(m.status), m.w)}
}

func (m model) helpLine() string {
	var s string
	switch m.screen {
	case scrDevice:
		s = "↑↓ move  ·  enter expand  ·  esc/⌫ back  ·  w wake  ·  b reboot  ·  x reset  ·  r refresh now  ·  q quit"
	case scrTasks:
		s = "↑↓ move  ·  s status  ·  / filter  ·  esc/⌫ back  ·  r refresh now  ·  1/2/3 screens  ·  q quit"
	case scrJobs:
		s = "↑↓ move  ·  enter open  ·  / filter  ·  r refresh now  ·  1/2/3 screens  ·  q quit"
	case scrJob:
		s = "↑↓ move  ·  esc/⌫ back  ·  r refresh now  ·  q quit"
	default:
		s = "↑↓ move  ·  enter open  ·  / filter  ·  w wake  ·  r refresh now  ·  1/2/3 screens  ·  q quit"
	}
	return clip(stMuted.Render(s), m.w)
}
