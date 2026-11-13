package tui

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
)

// Colour is decoration only. Every state also carries a word (fresh, failed,
// "ERR:"), so a monochrome terminal loses no meaning.
var (
	stTitle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("231"))
	stAccent = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("81"))
	stMuted  = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	stHead   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("250"))
	stSel    = lipgloss.NewStyle().Reverse(true)
	stBad    = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
)

// Log levels carry their word (INFO, WARN, ERROR) as the signal; colour only
// reinforces it.
var (
	stLevelInfo  = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	stLevelDebug = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	stLevelWarn  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
	stLevelBad   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("203"))
)

func levelStyle(level string) lipgloss.Style {
	switch level {
	case "ERROR", "FATAL", "CRITICAL":
		return stLevelBad
	case "WARN", "WARNING":
		return stLevelWarn
	case "DEBUG":
		return stLevelDebug
	}
	return stLevelInfo
}

// defaultInformInterval is the fallback used when a device never reported
// one, the same default as the API.
const defaultInformInterval = 300

// freshness restates the rule from internal/api/devices.go: fresh within two
// inform intervals, stale within six, offline after that. The TUI does not
// import internal/api, because it is a terminal client over the store, not a
// second HTTP surface; the rule lives here a second time and the two must
// change together.
func freshness(last time.Time, interval int, now time.Time) string {
	if interval <= 0 {
		interval = defaultInformInterval
	}
	age := now.Sub(last)
	switch {
	case age <= time.Duration(2*interval)*time.Second:
		return "fresh"
	case age <= time.Duration(6*interval)*time.Second:
		return "stale"
	default:
		return "offline"
	}
}

// age renders a duration the way an operator reads it: 4s, 3m, 5h, 12d.
func age(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// clock is the last-refresh time, the only timestamp that must be short.
func clock(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.Format("15:04:05")
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// clip cuts s to w visible cells and keeps ANSI escapes intact.
func clip(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	var b strings.Builder
	vis := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			j := i + 1
			if j < len(s) {
				j++ // the '['
				for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
					j++
				}
				if j < len(s) {
					j++ // the final byte
				}
			}
			b.WriteString(s[i:j])
			i = j
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		rw := lipgloss.Width(string(r))
		if vis+rw > w-1 {
			b.WriteString("…")
			break
		}
		b.WriteString(s[i : i+size])
		vis += rw
		i += size
	}
	b.WriteString("\x1b[0m")
	return b.String()
}

// cell clips a value to one column and pads it out, so columns line up
// whatever the terminal width.
func cell(s string, w int) string {
	s = clip(s, w)
	if pad := w - lipgloss.Width(s); pad > 0 {
		s += strings.Repeat(" ", pad)
	}
	return s
}

// column is one table column. flex shares the leftover width; core columns
// are never dropped, the rest go from the right as the terminal narrows.
type column struct {
	title string
	width int
	flex  int
	core  bool
}

func span(cols []column) int {
	n := 2 * (len(cols) - 1)
	for _, c := range cols {
		n += c.width
	}
	return n
}

func fitColumns(cols []column, avail int) ([]column, []int) {
	vis := append([]column(nil), cols...)
	for len(vis) > 1 && span(vis) > avail {
		drop := -1
		for i := len(vis) - 1; i >= 0; i-- {
			if !vis[i].core {
				drop = i
				break
			}
		}
		if drop < 0 {
			break
		}
		vis = append(vis[:drop], vis[drop+1:]...)
	}
	flex := 0
	for _, c := range vis {
		flex += c.flex
	}
	left := avail - span(vis)
	if left < 0 {
		left = 0
	}
	widths := make([]int, len(vis))
	for i, c := range vis {
		widths[i] = c.width
		if flex > 0 && c.flex > 0 {
			widths[i] += left * c.flex / flex
		}
	}
	return vis, widths
}

func tableRow(cells []string, widths []int) string {
	var b strings.Builder
	for i := range widths {
		if i > 0 {
			b.WriteString("  ")
		}
		s := ""
		if i < len(cells) {
			s = cells[i]
		}
		b.WriteString(cell(s, widths[i]))
	}
	return b.String()
}

func tableHeader(cols []column, widths []int) string {
	cells := make([]string, len(cols))
	for i, c := range cols {
		cells[i] = strings.ToUpper(c.title)
	}
	return stHead.Render(tableRow(cells, widths))
}

// wrapWords breaks text on spaces to fit w cells. A word wider than w is
// clipped, never broken mid-word, so a URL stays readable.
func wrapWords(s string, w int) []string {
	if w <= 0 {
		return nil
	}
	var out []string
	line := ""
	for _, word := range strings.Fields(s) {
		switch {
		case line == "":
			line = word
		case lipgloss.Width(line)+1+lipgloss.Width(word) <= w:
			line += " " + word
		default:
			out = append(out, clip(line, w))
			line = word
		}
	}
	if line != "" {
		out = append(out, clip(line, w))
	}
	return out
}

// labelTable lays pairs out as an aligned two-column table: the label column
// is sized from the longest label, the value column takes the rest, and a
// value that does not fit is clipped with an ellipsis, never wrapped.
func labelTable(rows [][2]string, w int) []string {
	if len(rows) == 0 || w <= 0 {
		return nil
	}
	lw := 0
	for _, r := range rows {
		if n := lipgloss.Width(r[0]); n > lw {
			lw = n
		}
	}
	lw++ // one gutter cell between the label and its value
	if lw > w {
		lw = w
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		line := cell(stMuted.Render(r[0]), lw)
		if rest := w - lw; rest > 0 {
			line += clip(r[1], rest)
		}
		out = append(out, line)
	}
	return out
}

// cursor is a list position. The window is derived from pos and the available
// height, so a refresh that shortens the list cannot leave a stale offset.
type cursor struct{ pos int }

func (c *cursor) clamp(n int) {
	switch {
	case n <= 0:
		c.pos = 0
	case c.pos >= n:
		c.pos = n - 1
	case c.pos < 0:
		c.pos = 0
	}
}

func (c *cursor) move(delta, n int) {
	c.pos += delta
	c.clamp(n)
}

func (c cursor) window(n, h int) (int, int) {
	if h <= 0 {
		h = 1
	}
	if n <= h {
		return 0, n
	}
	start := c.pos - h/2
	if start < 0 {
		start = 0
	}
	if start+h > n {
		start = n - h
	}
	return start, start + h
}

// filter is the incremental screen filter: "/" starts it, every rune narrows
// the loaded rows, Esc restores the text from before the edit.
type filter struct {
	text    string
	editing bool
	backup  string
}

func (f *filter) begin() {
	f.backup = f.text
	f.editing = true
}

func (f *filter) cancel() {
	f.text = f.backup
	f.editing = false
}

func (f *filter) commit() { f.editing = false }

// containsFold reports whether any of the fields contains q, case-insensitive.
func containsFold(q string, fields ...string) bool {
	if q == "" {
		return true
	}
	for _, f := range fields {
		if strings.Contains(strings.ToLower(f), q) {
			return true
		}
	}
	return false
}
