package tui

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// logLimit bounds one read: the daemon writes faster than a human reads.
const logLimit = 300

// logLine is one daemon log line. A line that is not JSON keeps its raw text
// and is shown as written, because a partial trailing write must not vanish.
type logLine struct {
	clock  string
	level  string
	msg    string
	fields []string // "k=v" in the order they appear on the line
	raw    string
	hay    string
}

// parseLogLine decodes one slog JSON object, keeping key order. slog writes
// time, level and msg first, then whatever the call site added.
func parseLogLine(raw string) logLine {
	line := logLine{}
	dec := json.NewDecoder(strings.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return rawLine(raw)
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return rawLine(raw)
		}
		key, _ := keyTok.(string)
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			// A trailing partial write: show it exactly as it arrived.
			return rawLine(raw)
		}
		switch key {
		case "time":
			var s string
			if json.Unmarshal(val, &s) == nil {
				if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
					line.clock = t.Format("15:04:05")
				}
			}
		case "level":
			var s string
			if json.Unmarshal(val, &s) == nil {
				line.level = strings.ToUpper(s)
			}
		case "msg":
			var s string
			if json.Unmarshal(val, &s) == nil {
				line.msg = s
			}
		default:
			if key != "" {
				line.fields = append(line.fields, key+"="+fieldValue(val))
			}
		}
	}
	if line.clock == "" {
		line.clock = "--:--:--"
	}
	if line.level == "" {
		line.level = "-"
	}
	line.hay = strings.ToLower(line.msg + " " + strings.Join(line.fields, " "))
	return line
}

func rawLine(raw string) logLine {
	return logLine{raw: raw, hay: strings.ToLower(raw)}
}

// fieldValue renders one slog field as an operator reads it: strings bare,
// string arrays as [a b], anything else as compact JSON.
func fieldValue(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var ss []string
	if json.Unmarshal(raw, &ss) == nil {
		return "[" + strings.Join(ss, " ") + "]"
	}
	var v any
	if json.Unmarshal(raw, &v) == nil {
		if b, err := json.Marshal(v); err == nil {
			return string(b)
		}
	}
	return string(raw)
}

type logsPane struct {
	all       []logLine
	view      []logLine
	cur       cursor
	filter    filter
	loaded    bool
	truncated bool
	err       error
	at        time.Time
}

// set stores the reader's lines newest first. The cursor keeps its index when
// it still exists, so a refresh does not move the row under the operator.
func (p *logsPane) set(raw []string, at time.Time) {
	before := len(p.view)
	pos := p.cur.pos
	p.all = p.all[:0]
	for i := len(raw) - 1; i >= 0; i-- {
		if strings.TrimSpace(raw[i]) == "" {
			continue
		}
		p.all = append(p.all, parseLogLine(raw[i]))
	}
	p.truncated = len(raw) >= logLimit
	p.err = nil
	p.at = at
	p.loaded = true
	p.refilter()
	// Anchor the viewport. On the newest line, new lines push in at the top
	// and the cursor stays there. Anywhere else, shift by the number of
	// matching lines that arrived above the cursor, so the selected line does
	// not slide away under the operator.
	if pos > 0 {
		if grown := len(p.view) - before; grown > 0 {
			p.cur.pos = pos + grown
		} else {
			p.cur.pos = pos
		}
		p.cur.clamp(len(p.view))
	}
}

func (p *logsPane) fail(err error, at time.Time) {
	p.err = err
	p.at = at
	p.loaded = true
}

func (p *logsPane) refilter() {
	q := strings.ToLower(strings.TrimSpace(p.filter.text))
	p.view = p.view[:0]
	for _, l := range p.all {
		if q == "" || strings.Contains(l.hay, q) {
			p.view = append(p.view, l)
		}
	}
	p.cur.clamp(len(p.view))
}

// logLineText is the rendering shape: HH:MM:SS, a fixed five-cell level
// column, the message, then every field as k=v in the order written. Clipped,
// never wrapped.
func (m model) logLineText(l logLine) string {
	if l.raw != "" {
		return clip(stMuted.Render(l.raw), m.w)
	}
	level := cell(levelStyle(l.level).Render(l.level), 5)
	line := cell(l.clock, 8) + " " + level + " " + l.msg
	for _, f := range l.fields {
		line += "  " + f
	}
	return clip(line, m.w)
}

func (m model) logsBody(h int) []string {
	p := &m.logs
	if !p.loaded {
		return []string{stMuted.Render(" loading…")}
	}
	switch {
	case len(p.all) == 0 && p.err != nil:
		return []string{stMuted.Render(" log lines could not be read; see the status line")}
	case len(p.all) == 0:
		return []string{
			stMuted.Render(" Logs is empty."),
			stMuted.Render(" The daemon has not written a log line yet."),
		}
	case len(p.view) == 0:
		return []string{stMuted.Render(fmt.Sprintf(" No log line matches %q. Esc clears the filter.", p.filter.text))}
	}

	rows := h
	if p.truncated {
		rows--
	}
	if rows < 1 {
		rows = 1
	}
	out := make([]string, 0, rows+1)
	start, end := p.cur.window(len(p.view), rows)
	for i := start; i < end; i++ {
		line := m.logLineText(p.view[i])
		if i == p.cur.pos {
			line = stSel.Render(cell(line, m.w))
		}
		out = append(out, line)
	}
	if p.truncated {
		out = append(out, stMuted.Render(fmt.Sprintf(" newest %d lines; older entries are not loaded", logLimit)))
	}
	return out
}
