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

	// all and paths are the whole cached parameter set from the first read.
	// The tree fetches more pages lazily, but the search filters these, so it
	// never needs another read.
	all     map[string]store.Param
	paths   []string
	filter  filter
	matches int
}

func newDetailPane(d store.Device) detailPane {
	return detailPane{
		device:   d,
		pages:    map[string]paramPage{},
		expanded: map[string]bool{},
		all:      map[string]store.Param{},
	}
}

// takeCache stores the full cached parameter set from the detail read.
func (d *detailPane) takeCache(params map[string]store.Param) {
	d.all = params
	d.paths = make([]string, 0, len(params))
	for path := range params {
		d.paths = append(d.paths, path)
	}
	sort.Strings(d.paths)
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

// filtering reports whether a parameter search is narrowing the tree.
func (d *detailPane) filtering() bool {
	return strings.TrimSpace(d.filter.text) != ""
}

func (d *detailPane) rebuild() {
	d.ensure()
	sel := ""
	if r, ok := d.selectedRow(); ok {
		sel = r.prefix
	}
	d.rows = d.rows[:0]
	d.matches = 0
	if d.filtering() {
		d.buildMatches()
	} else {
		d.walk("", 0)
	}
	d.cur.clamp(len(d.rows))
	if sel != "" {
		d.selectPrefix(sel)
	}
}

// buildMatches draws every cached path that matches the query, one parent row
// per ancestor, so the hierarchy still reads. Paths are sorted, so sibling
// matches stay together.
func (d *detailPane) buildMatches() {
	q := strings.ToLower(strings.TrimSpace(d.filter.text))
	seen := map[string]bool{}
	for _, path := range d.paths {
		if !strings.Contains(strings.ToLower(path), q) {
			continue
		}
		d.matches++
		for _, obj := range ancestorPrefixes(path) {
			if seen[obj] {
				continue
			}
			seen[obj] = true
			d.rows = append(d.rows, paramRow{
				prefix: obj,
				name:   leafName(obj),
				object: true,
				depth:  pathDepth(obj),
			})
		}
		d.rows = append(d.rows, paramRow{
			prefix: path,
			name:   leafName(path),
			value:  paramValue(d.all[path]),
			depth:  pathDepth(path),
		})
	}
}

// ancestorPrefixes lists the object prefixes containing a leaf, outermost
// first: "A.B.C" -> ["A.", "A.B."].
func ancestorPrefixes(path string) []string {
	var out []string
	for i := 0; i < len(path); i++ {
		if path[i] == '.' {
			out = append(out, path[:i+1])
		}
	}
	return out
}

// leafName is the last segment of a path or prefix.
func leafName(path string) string {
	path = strings.TrimSuffix(path, ".")
	if i := strings.LastIndexByte(path, '.'); i >= 0 {
		return path[i+1:]
	}
	return path
}

// pathDepth is the tree depth of a path: root objects are 0.
func pathDepth(path string) int {
	return strings.Count(strings.TrimSuffix(path, "."), ".")
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
	out := m.detailHeader()
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

	title := "PARAMETERS"
	if d.filtering() {
		title = fmt.Sprintf("PARAMETERS  %d of %d  /%s", d.matches, len(d.paths), strings.TrimSpace(d.filter.text))
	}
	out = append(out, stHead.Render(title))
	switch {
	case len(d.pages) == 0:
		out = append(out, stMuted.Render(" loading…"))
	case d.filtering() && len(d.paths) == 0:
		out = append(out, stMuted.Render(" no cached parameters to search; the next Inform fills them"))
	case d.filtering() && d.matches == 0:
		out = append(out, stMuted.Render(fmt.Sprintf(" no parameter matches %q; esc clears the filter", strings.TrimSpace(d.filter.text))))
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
	fault := "-"
	if len(d.LastFault) > 0 && string(d.LastFault) != "null" {
		fault = string(d.LastFault)
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
		{"last fault", fault},
	}
	return rows
}

// detailFacts renders the fact table on its own, full width.
func (m model) detailFacts() []string {
	return labelTable(m.factRows(), m.w)
}

// Two-column header. The split promises a fact value at least minFactValue
// wide and an action description at least minActionDesc wide; those floors,
// the labels and the action copy are constants, so the columns never move when
// a value grows or a device goes from fresh to stale.
const (
	columnGap     = 2
	minFactValue  = 30 // an OUI-ProductClass-Serial id, in practice
	maxFactValue  = 34 // the full id "00259E-HG8145V5-48575443BD7589AB" is 32; longer values clip
	minActionDesc = 18 // "queue a reboot task"
)

type actionSpec struct{ key, name, what string }

var actionSpecs = []actionSpec{
	{"w", "wake", "send a connection request now"},
	{"b", "reboot", "queue a reboot task"},
	{"x", "factory reset", "queue a reset; wipes the device configuration"},
}

func actionNameWidth() int {
	n := 0
	for _, s := range actionSpecs {
		if w := lipgloss.Width(s.name); w > n {
			n = w
		}
	}
	return n + 2 // two cells between the verb and its description
}

func actionPrefixWidth() int {
	return 2 + 1 + 2 + actionNameWidth() // gutter, key, gap, verb, gap
}

func actionDescWidth() int {
	n := 0
	for _, s := range actionSpecs {
		if w := lipgloss.Width(s.what); w > n {
			n = w
		}
	}
	return n
}

// detailHeader lays the facts and the actions side by side when the terminal
// is wide enough for both floors, and stacks the actions under the facts when
// it is not.
func (m model) detailHeader() []string {
	rows := m.factRows()
	labelW := 0
	for _, r := range rows {
		if n := lipgloss.Width(r[0]); n > labelW {
			labelW = n
		}
	}
	labelW++
	factsMin := labelW + 1 + minFactValue
	actionsMin := actionPrefixWidth() + minActionDesc

	if m.w < factsMin+columnGap+actionsMin {
		out := labelTable(rows, m.w)
		out = append(out, "", stHead.Render("ACTIONS"))
		return append(out, m.actionLines(m.w)...)
	}

	// Fixed widths: the facts get label + gutter + a capped value column, never
	// the whole terminal, and the actions sit right after them. Anything to
	// the right of the actions stays empty.
	factsW := labelW + 1 + maxFactValue
	if max := m.w - columnGap - actionsMin; factsW > max {
		factsW = max // narrow two-column layouts shrink facts, not the actions
	}
	actionsW := actionPrefixWidth() + actionDescWidth()
	if max := m.w - columnGap - factsW; actionsW > max {
		actionsW = max
	}
	right := append([]string{stHead.Render("ACTIONS")}, m.actionLines(actionsW)...)
	return joinColumns(labelTable(rows, factsW), right, factsW, actionsW)
}

// joinColumns pads the left block to its own width so the right column starts
// at one x on every row.
func joinColumns(left, right []string, leftW, rightW int) []string {
	n := len(left)
	if len(right) > n {
		n = len(right)
	}
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		l := ""
		if i < len(left) {
			l = cell(left[i], leftW)
		}
		r := ""
		if i < len(right) {
			r = clip(right[i], rightW)
		}
		out = append(out, l+strings.Repeat(" ", columnGap)+r)
	}
	return out
}

// actionLines is the ACTIONS block: the key, the verb, and what it does. The
// keys are the ones key() accepts on this screen.
func (m model) actionLines(w int) []string {
	nameWidth := actionNameWidth()
	out := make([]string, 0, len(actionSpecs))
	for _, s := range actionSpecs {
		line := "  " + stAccent.Render(s.key) + "  " + cell(s.name, nameWidth) + stMuted.Render(s.what)
		out = append(out, clip(line, w))
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
		switch {
		case m.det.filtering():
			marker = "·" // every match is already shown; nothing to expand
		case m.det.expanded[r.prefix]:
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
