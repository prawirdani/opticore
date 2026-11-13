package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/prawirdani/opticore/internal/store"
)

// paramRow is one rendered line of the lazy parameter tree. An object row
// carries the prefix that Enter loads; a leaf row carries its value.
type paramRow struct {
	prefix string
	name   string
	object bool
	value  string
	depth  int
}

type paramChild struct {
	name   string
	object bool
	value  string
}

type paramPage struct {
	children []paramChild
}

// buildPage turns the leaves under prefix into one tree level: direct child
// objects first, then leaves. Deeper paths collapse into their first segment,
// which is what makes expanding one level at a time cheap.
func buildPage(params map[string]store.Param, prefix string) paramPage {
	objects := make([]string, 0, 8)
	leaves := make([]paramChild, 0, len(params))
	seen := map[string]bool{}
	for path, p := range params {
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		rest := path[len(prefix):]
		if rest == "" {
			continue
		}
		if i := strings.IndexByte(rest, '.'); i >= 0 {
			name := rest[:i]
			if !seen[name] {
				seen[name] = true
				objects = append(objects, name)
			}
			continue
		}
		leaves = append(leaves, paramChild{name: rest, value: paramValue(p)})
	}
	sort.Strings(objects)
	sort.Slice(leaves, func(i, j int) bool { return leaves[i].name < leaves[j].name })
	page := paramPage{children: make([]paramChild, 0, len(objects)+len(leaves))}
	for _, name := range objects {
		page.children = append(page.children, paramChild{name: name, object: true})
	}
	page.children = append(page.children, leaves...)
	return page
}

// paramValue returns the cached value as stored.
func paramValue(p store.Param) string {
	return p.Value
}

type detailPane struct {
	device   store.Device
	pages    map[string]paramPage
	expanded map[string]bool
	rows     []paramRow
	cur      cursor
	tasks    []store.Task
	events   []store.Event
	at       time.Time
}

func newDetailPane(d store.Device) detailPane {
	return detailPane{
		device:   d,
		pages:    map[string]paramPage{},
		expanded: map[string]bool{},
	}
}

func (d *detailPane) ensure() {
	if d.pages == nil {
		d.pages = map[string]paramPage{}
	}
	if d.expanded == nil {
		d.expanded = map[string]bool{}
	}
}

func (d *detailPane) resetTree() {
	d.ensure()
	d.pages = map[string]paramPage{}
	d.expanded = map[string]bool{}
	d.rows = nil
	d.cur.clamp(0)
}

func (d *detailPane) rebuild() {
	d.ensure()
	sel := ""
	if r, ok := d.selectedRow(); ok {
		sel = r.prefix
	}
	d.rows = d.rows[:0]
	d.walk("", 0)
	d.cur.clamp(len(d.rows))
	if sel != "" {
		d.selectPrefix(sel)
	}
}

func (d *detailPane) walk(prefix string, depth int) {
	page, ok := d.pages[prefix]
	if !ok {
		return
	}
	for _, c := range page.children {
		row := paramRow{name: c.name, object: c.object, value: c.value, depth: depth}
		if c.object {
			row.prefix = prefix + c.name + "."
		} else {
			row.prefix = prefix + c.name
		}
		d.rows = append(d.rows, row)
		if c.object && d.expanded[row.prefix] {
			d.walk(row.prefix, depth+1)
		}
	}
}

func (d *detailPane) selectedRow() (paramRow, bool) {
	if d.cur.pos < 0 || d.cur.pos >= len(d.rows) {
		return paramRow{}, false
	}
	return d.rows[d.cur.pos], true
}

// selectChild puts the cursor on the first child of the row it just opened,
// so pressing Enter again walks one level deeper instead of toggling shut.
func (d *detailPane) selectChild() {
	row, ok := d.selectedRow()
	if !ok || !row.object || !d.expanded[row.prefix] {
		return
	}
	if d.cur.pos+1 < len(d.rows) && d.rows[d.cur.pos+1].depth > row.depth {
		d.cur.pos++
	}
}

func (d *detailPane) selectPrefix(prefix string) {
	for i, r := range d.rows {
		if r.prefix == prefix {
			d.cur.pos = i
			return
		}
	}
}

// collapse closes the selected object, else its nearest expanded ancestor.
// False means there was nothing left to close.
func (d *detailPane) collapse() bool {
	row, ok := d.selectedRow()
	if !ok {
		return false
	}
	if row.object && d.expanded[row.prefix] {
		delete(d.expanded, row.prefix)
		d.rebuild()
		return true
	}
	for p := parentPrefix(row.prefix); p != ""; p = parentPrefix(p) {
		if d.expanded[p] {
			delete(d.expanded, p)
			d.rebuild()
			d.selectPrefix(p)
			return true
		}
	}
	return false
}

// parentPrefix is the prefix containing p, "" at the root.
func parentPrefix(p string) string {
	p = strings.TrimSuffix(p, ".")
	if i := strings.LastIndexByte(p, '.'); i >= 0 {
		return p[:i+1]
	}
	return ""
}

func (m model) detailBody(h int) []string {
	d := &m.det
	if d.device.ID == "" {
		return []string{stMuted.Render(" no device loaded")}
	}
	out := append([]string{}, m.detailFacts()...)
	out = append(out, "", stHead.Render("ACTIONS"))
	out = append(out, m.actionLines()...)
	out = append(out, "")

	tail := append(m.detailTasks(), m.detailEvents()...)
	// The tree keeps at least a few rows; the task and event blocks are
	// dropped whole on a short terminal instead of squeezing the tree.
	if len(out)+1+3+len(tail) > h {
		tail = nil
	}
	rowsH := h - len(out) - 1 - len(tail)
	if rowsH < 1 {
		rowsH = 1
	}

	out = append(out, stHead.Render("PARAMETERS"))
	switch {
	case len(d.pages) == 0:
		out = append(out, stMuted.Render(" loading…"))
	case len(d.rows) == 0:
		out = append(out, stMuted.Render(" no cached parameters; the next Inform fills them"))
	default:
		start, end := d.cur.window(len(d.rows), rowsH)
		for i := start; i < end; i++ {
			out = append(out, m.paramLine(d.rows[i], i == d.cur.pos))
		}
	}
	return append(out, tail...)
}

// factRows is the device header, one fact per row. Keeping the pairs apart
// from the rendering lets the label column be sized from the labels.
func (m model) factRows() [][2]string {
	d := m.det.device
	now := m.now()
	interval := d.InformInterval
	if interval <= 0 {
		interval = defaultInformInterval
	}
	cr := "no credentials"
	if d.CRSetAt != nil {
		cr = "credentials ready"
	}
	reboot := "no"
	if d.PendingReboot {
		reboot = "pending"
	}
	rows := [][2]string{
		{"id", dash(d.ID)},
		{"serial", dash(d.Serial)},
		{"product", dash(d.ProductClass)},
		{"software", dash(d.SoftwareVersion)},
		{"oui", dash(d.OUI)},
		{"seen", age(now.Sub(d.LastInform)) + " (" + freshness(d.LastInform, d.InformInterval, now) + ")"},
		{"interval", fmt.Sprintf("%ds", interval)},
		{"first", stamp(d.FirstInform)},
		{"addr", dash(d.LastAddr)},
		{"cr", cr},
		{"reboot", reboot},
	}
	if len(d.LastFault) > 0 && string(d.LastFault) != "null" {
		rows = append(rows, [2]string{"last fault", string(d.LastFault)})
	}
	return rows
}

// detailFacts renders the header as an aligned two-column table, so a fact
// never collides with its neighbour and every value starts at one x.
func (m model) detailFacts() []string {
	return labelTable(m.factRows(), m.w)
}

// actionLines is the ACTIONS block: the key, the verb, and what it does. The
// keys are the ones key() accepts on this screen.
func (m model) actionLines() []string {
	specs := []struct{ key, name, what string }{
		{"w", "wake", "send a connection request now"},
		{"b", "reboot", "queue a reboot task"},
		{"x", "factory reset", "queue a reset; wipes the device configuration"},
	}
	nameWidth := 0
	for _, s := range specs {
		if n := lipgloss.Width(s.name); n > nameWidth {
			nameWidth = n
		}
	}
	nameWidth += 2 // two cells between the verb and its description
	out := make([]string, 0, len(specs))
	for _, s := range specs {
		line := "  " + stAccent.Render(s.key) + "  " + cell(s.name, nameWidth) + stMuted.Render(s.what)
		out = append(out, clip(line, m.w))
	}
	return out
}

func kv(k, v string) string {
	return stMuted.Render(k+" ") + v
}

func (m model) paramLine(r paramRow, sel bool) string {
	gutter := "  "
	if sel {
		gutter = "› "
	}
	line := gutter + strings.Repeat("  ", r.depth)
	if r.object {
		marker := "▸"
		if m.det.expanded[r.prefix] {
			marker = "▾"
		}
		line += marker + " " + r.name
	} else {
		line += "· " + r.name + "  " + stMuted.Render(r.value)
	}
	line = clip(line, m.w)
	if sel {
		return stSel.Render(cell(line, m.w))
	}
	return line
}

func (m model) detailTasks() []string {
	now := m.now()
	out := []string{"", stHead.Render("TASKS")}
	if len(m.det.tasks) == 0 {
		return append(out, stMuted.Render("  none for this device"))
	}
	for i, t := range m.det.tasks {
		if i >= 3 {
			break
		}
		line := fmt.Sprintf("  #%d %s %s %s ago", t.ID, t.Name, t.Status, age(now.Sub(t.UpdatedAt)))
		if f := taskFault(t); f != "-" {
			line += "  " + f
		}
		out = append(out, clip(line, m.w))
	}
	return out
}

func (m model) detailEvents() []string {
	now := m.now()
	out := []string{"", stHead.Render("EVENTS")}
	if len(m.det.events) == 0 {
		return append(out, stMuted.Render("  none for this device"))
	}
	for i, e := range m.det.events {
		if i >= 3 {
			break
		}
		line := fmt.Sprintf("  %s ago  %s  %s", age(now.Sub(e.At)), e.Type, string(e.Detail))
		out = append(out, clip(line, m.w))
	}
	return out
}

func taskFault(t store.Task) string {
	if t.FaultCode != nil {
		return fmt.Sprintf("%d %s", *t.FaultCode, t.Fault)
	}
	if t.Fault != "" {
		return t.Fault
	}
	return "-"
}
