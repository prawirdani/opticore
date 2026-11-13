package tui

import (
	"fmt"
	"strings"
)

func (m model) View() string {
	if m.w <= 0 || m.h <= 0 {
		return ""
	}
	if m.h < 5 {
		return clip(m.statusLine(), m.w)
	}
	bodyH := m.h - 4
	lines := make([]string, 0, m.h)
	lines = append(lines, m.tabLine(), m.contextLine())
	body := m.body(bodyH)
	if len(body) > bodyH {
		body = body[:bodyH]
	}
	lines = append(lines, body...)
	for len(lines) < m.h-2 {
		lines = append(lines, "")
	}
	lines = append(lines, m.statusLine(), m.helpLine())
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
	return clip(b.String(), m.w)
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

func (m model) statusLine() string {
	switch {
	case m.confirm != nil:
		return clip(stAccent.Render(fmt.Sprintf("Wake %s now?  [y] yes  [n] no", m.confirm.label)), m.w)
	case m.loading != "":
		return clip(stMuted.Render("· "+m.loading), m.w)
	case m.statusBad:
		return clip(stBad.Render(m.status), m.w)
	}
	return clip(stMuted.Render(m.status), m.w)
}

func (m model) helpLine() string {
	var s string
	switch m.screen {
	case scrDevice:
		s = "↑↓ move  ·  enter expand  ·  esc collapse/back  ·  w wake  ·  r refresh  ·  q quit"
	case scrTasks:
		s = "↑↓ move  ·  s status  ·  / filter  ·  esc back  ·  r refresh  ·  1/2/3 screens  ·  q quit"
	case scrJobs:
		s = "↑↓ move  ·  enter open  ·  / filter  ·  r refresh  ·  1/2/3 screens  ·  q quit"
	case scrJob:
		s = "↑↓ move  ·  esc back  ·  r refresh  ·  q quit"
	default:
		s = "↑↓ move  ·  enter open  ·  / filter  ·  w wake  ·  r refresh  ·  1/2/3 screens  ·  q quit"
	}
	return clip(stMuted.Render(s), m.w)
}
