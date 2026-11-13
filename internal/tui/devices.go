package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/prawirdani/opticore/internal/store"
)

type devicesPane struct {
	all     []store.Device
	view    []store.Device
	cur     cursor
	filter  filter
	loaded  bool
	at      time.Time
	truncat bool // the list hit deviceLimit, so older devices are not loaded
}

func (p *devicesPane) set(rows []store.Device, now time.Time) {
	p.all = rows
	p.at = now
	p.loaded = true
	p.truncat = len(rows) >= deviceLimit
	p.refilter(now)
}

func (p *devicesPane) refilter(now time.Time) {
	q := strings.ToLower(strings.TrimSpace(p.filter.text))
	p.view = p.view[:0]
	for _, d := range p.all {
		if containsFold(q, d.ID, d.Serial, d.ProductClass, d.SoftwareVersion,
			freshness(d.LastInform, d.InformInterval, now)) {
			p.view = append(p.view, d)
		}
	}
	p.cur.clamp(len(p.view))
}

func (p *devicesPane) selected() (store.Device, bool) {
	if p.cur.pos < 0 || p.cur.pos >= len(p.view) {
		return store.Device{}, false
	}
	return p.view[p.cur.pos], true
}

func deviceColumns() []column {
	return []column{
		{title: "", width: 2, core: true},
		{title: "serial", width: 12, flex: 3, core: true},
		{title: "product", width: 12, flex: 2},
		{title: "software", width: 12, flex: 2},
		{title: "seen", width: 6, core: true},
		{title: "status", width: 7, core: true},
		{title: "address", width: 12, flex: 2},
		{title: "cr", width: 5},
	}
}

func (m model) devicesBody(h int) []string {
	p := &m.dev
	cols, widths := fitColumns(deviceColumns(), m.w)
	out := []string{tableHeader(cols, widths)}

	switch {
	case !p.loaded:
		return append(out, stMuted.Render(" loading…"))
	case len(p.all) == 0:
		return append(out, "",
			stMuted.Render(" No devices yet. The first Inform creates one."),
			stMuted.Render(" Point an ONT at this ACS, then press r."))
	case len(p.view) == 0:
		return append(out, "",
			stMuted.Render(fmt.Sprintf(" No device matches %q. Esc clears the filter.", p.filter.text)))
	}

	rows := h - len(out)
	if rows < 1 {
		rows = 1
	}
	now := m.now()
	start, end := p.cur.window(len(p.view), rows)
	for i := start; i < end; i++ {
		d := p.view[i]
		marker := " "
		if i == p.cur.pos {
			marker = "›"
		}
		cells := []string{
			marker,
			d.Serial,
			d.ProductClass,
			d.SoftwareVersion,
			age(now.Sub(d.LastInform)),
			freshness(d.LastInform, d.InformInterval, now),
			d.LastAddr,
			crLabel(d),
		}
		line := tableRow(cells, widths)
		if i == p.cur.pos {
			line = stSel.Render(cell(line, m.w))
		}
		out = append(out, line)
	}
	if p.truncat {
		out = append(out, stMuted.Render(fmt.Sprintf(" newest %d shown; narrow the filter to find older devices", len(p.all))))
	}
	return out
}

// crLabel says whether the device has connection-request credentials without
// ever touching a secret.
func crLabel(d store.Device) string {
	if d.CRSetAt != nil {
		return "ready"
	}
	return "none"
}
