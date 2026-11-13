package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/prawirdani/opticore/internal/store"
)

type jobsPane struct {
	all    []store.Job
	view   []store.Job
	counts map[int64]map[string]int
	cur    cursor
	filter filter
	loaded bool
	at     time.Time
}

func (p *jobsPane) set(jobs []store.Job, counts map[int64]map[string]int, now time.Time) {
	p.all = jobs
	p.counts = counts
	p.at = now
	p.loaded = true
	p.refilter()
}

func (p *jobsPane) refilter() {
	q := strings.ToLower(strings.TrimSpace(p.filter.text))
	p.view = p.view[:0]
	for _, j := range p.all {
		if containsFold(q, j.Name, j.Status, fmt.Sprint(j.ID)) {
			p.view = append(p.view, j)
		}
	}
	p.cur.clamp(len(p.view))
}

func (p *jobsPane) selected() (store.Job, bool) {
	if p.cur.pos < 0 || p.cur.pos >= len(p.view) {
		return store.Job{}, false
	}
	return p.view[p.cur.pos], true
}

func jobColumns() []column {
	return []column{
		{title: "", width: 2, core: true},
		{title: "id", width: 5, core: true},
		{title: "name", width: 12, flex: 2, core: true},
		{title: "status", width: 8, core: true},
		{title: "progress", width: 14, flex: 3},
		{title: "created", width: 8, flex: 1},
	}
}

func (m model) jobsBody(h int) []string {
	p := &m.jobs
	cols, widths := fitColumns(jobColumns(), m.w)
	out := []string{tableHeader(cols, widths)}

	switch {
	case !p.loaded:
		return append(out, stMuted.Render(" loading…"))
	case len(p.all) == 0:
		return append(out, "",
			stMuted.Render(" No jobs yet."),
			stMuted.Render(" Bulk jobs are created over the API: POST /v1/jobs."))
	case len(p.view) == 0:
		return append(out, "",
			stMuted.Render(fmt.Sprintf(" No job matches %q. Esc clears the filter.", p.filter.text)))
	}

	rows := h - len(out)
	if rows < 1 {
		rows = 1
	}
	now := m.now()
	start, end := p.cur.window(len(p.view), rows)
	for i := start; i < end; i++ {
		j := p.view[i]
		marker := " "
		if i == p.cur.pos {
			marker = "›"
		}
		cells := []string{
			marker,
			fmt.Sprint(j.ID),
			j.Name,
			j.Status,
			progressText(p.counts[j.ID]),
			age(now.Sub(j.CreatedAt)),
		}
		line := tableRow(cells, widths)
		if i == p.cur.pos {
			line = stSel.Render(cell(line, m.w))
		}
		out = append(out, line)
	}
	return out
}

type jobPane struct {
	job      store.Job
	counts   map[string]int
	failures []store.JobFailure
	cur      cursor
	loaded   bool
	at       time.Time
}

func jobColumnsDetail() []column {
	return []column{
		{title: "", width: 2, core: true},
		{title: "device", width: 24, flex: 3, core: true},
		{title: "task", width: 6, core: true},
		{title: "fault", width: 18, flex: 2},
	}
}

func (m model) jobBody(h int) []string {
	p := &m.jobd
	if p.job.ID == 0 {
		return []string{stMuted.Render(" no job loaded")}
	}
	facts := kv("job", fmt.Sprint(p.job.ID)) + "   " +
		kv("name", dash(p.job.Name)) + "   " +
		kv("status", p.job.Status) + "   " +
		kv("max parallel", fmt.Sprint(p.job.MaxParallel))
	out := []string{
		clip(facts, m.w),
		clip(kv("created", stamp(p.job.CreatedAt))+"   "+kv("progress", progressText(p.counts)), m.w),
		"",
		stHead.Render("FAILURES"),
	}

	cols, widths := fitColumns(jobColumnsDetail(), m.w)
	out = append(out, tableHeader(cols, widths))
	switch {
	case !p.loaded:
		return append(out, stMuted.Render(" loading…"))
	case len(p.failures) == 0:
		return append(out, stMuted.Render(" none: no task of this job failed"))
	}

	rows := h - len(out)
	if rows < 1 {
		rows = 1
	}
	start, end := p.cur.window(len(p.failures), rows)
	for i := start; i < end; i++ {
		f := p.failures[i]
		marker := " "
		if i == p.cur.pos {
			marker = "›"
		}
		cells := []string{marker, f.DeviceID, fmt.Sprint(f.TaskID), failureFault(f)}
		line := tableRow(cells, widths)
		if i == p.cur.pos {
			line = stSel.Render(cell(line, m.w))
		}
		out = append(out, line)
	}
	return out
}

func failureFault(f store.JobFailure) string {
	if f.FaultCode != nil {
		return fmt.Sprintf("%d %s", *f.FaultCode, f.Fault)
	}
	return dash(f.Fault)
}

// progressText renders done/total and, when there are any, the failed count.
func progressText(c map[string]int) string {
	total, done, failed := 0, 0, 0
	for status, n := range c {
		total += n
		switch status {
		case "done":
			done += n
		case "failed", "unknown":
			failed += n
		}
	}
	if total == 0 {
		return "-"
	}
	out := fmt.Sprintf("%d/%d", done, total)
	if failed > 0 {
		out += fmt.Sprintf(" · %d failed", failed)
	}
	return out
}
