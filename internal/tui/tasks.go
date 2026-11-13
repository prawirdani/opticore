package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/prawirdani/opticore/internal/store"
)

// taskStatuses is the operator-facing cycle for `s`; the store also has
// "cancelled", which still shows under "all".
var taskStatuses = []string{"", "pending", "running", "failed", "unknown", "expired", "done"}

type tasksPane struct {
	deviceID    string
	deviceLabel string
	all         []store.Task
	view        []store.Task
	status      string
	cur         cursor
	filter      filter
	loaded      bool
	at          time.Time
}

func (p *tasksPane) set(rows []store.Task, now time.Time) {
	p.all = rows
	p.at = now
	p.loaded = true
	p.refilter()
}

func (p *tasksPane) refilter() {
	q := strings.ToLower(strings.TrimSpace(p.filter.text))
	p.view = p.view[:0]
	for _, t := range p.all {
		if p.status != "" && t.Status != p.status {
			continue
		}
		if containsFold(q, t.Name, t.Status, taskFault(t), fmt.Sprint(t.ID)) {
			p.view = append(p.view, t)
		}
	}
	p.cur.clamp(len(p.view))
}

func (p *tasksPane) statusLabel() string {
	if p.status == "" {
		return "all"
	}
	return p.status
}

func (p *tasksPane) cycleStatus() {
	for i, s := range taskStatuses {
		if s == p.status {
			p.status = taskStatuses[(i+1)%len(taskStatuses)]
			return
		}
	}
	p.status = ""
}

func taskColumns() []column {
	return []column{
		{title: "", width: 2, core: true},
		{title: "id", width: 6, core: true},
		{title: "task", width: 10, flex: 1, core: true},
		{title: "status", width: 9, core: true},
		{title: "att", width: 3},
		{title: "fault", width: 18, flex: 4},
		{title: "updated", width: 8, flex: 1},
	}
}

func (m model) tasksBody(h int) []string {
	p := &m.task
	cols, widths := fitColumns(taskColumns(), m.w)
	out := []string{tableHeader(cols, widths)}

	switch {
	case p.deviceID == "":
		return append(out, "",
			stMuted.Render(" No device selected."),
			stMuted.Render(" Pick one on [1] devices, then press 2."))
	case !p.loaded:
		return append(out, stMuted.Render(" loading…"))
	case len(p.all) == 0:
		return append(out, "",
			stMuted.Render(fmt.Sprintf(" No tasks for %s yet.", p.deviceLabel)))
	case len(p.view) == 0:
		return append(out, "",
			stMuted.Render(fmt.Sprintf(" No %s tasks for %s. s cycles the status filter.", p.statusLabel(), p.deviceLabel)))
	}

	rows := h - len(out)
	if rows < 1 {
		rows = 1
	}
	now := m.now()
	start, end := p.cur.window(len(p.view), rows)
	for i := start; i < end; i++ {
		t := p.view[i]
		marker := " "
		if i == p.cur.pos {
			marker = "›"
		}
		cells := []string{
			marker,
			fmt.Sprint(t.ID),
			t.Name,
			t.Status,
			fmt.Sprint(t.Attempts),
			taskFault(t),
			age(now.Sub(t.UpdatedAt)),
		}
		line := tableRow(cells, widths)
		if i == p.cur.pos {
			line = stSel.Render(cell(line, m.w))
		}
		out = append(out, line)
	}
	return out
}
