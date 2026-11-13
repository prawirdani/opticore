package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/prawirdani/opticore/internal/store"
)

var testNow = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

// fake satisfies every reader. Each call counts nothing: the assertions look
// at what the model decided to keep, not at call order.
type fake struct {
	devices  []store.Device
	devErr   error
	params   map[string]map[string]store.Param
	paramErr error
	tasks    []store.Task
	taskErr  error
	events   []store.Event
	jobs     []store.Job
	counts   map[int64]map[string]int
	failures []store.JobFailure
	jobErr   error
	wakeErr  error
	woken    []string

	queued     []store.Task
	enqueueErr error
	nextID     int64

	calls map[string]int

	serveDown  bool
	servingErr error
}

func (f *fake) note(name string) {
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[name]++
}

func (f *fake) count(name string) int { return f.calls[name] }

func (f *fake) serving(ctx context.Context) (bool, error) {
	f.note("Serving")
	if f.servingErr != nil {
		return false, f.servingErr
	}
	return !f.serveDown, nil
}

func (f *fake) deps() Deps {
	return Deps{Devices: f, Tasks: f, Jobs: f, Events: f, Params: f, Wake: f.wake, Enqueue: f.enqueue, Serving: f.serving}
}

func (f *fake) ListDevices(ctx context.Context, flt store.ListFilter) ([]store.Device, error) {
	f.note("ListDevices")
	return f.devices, f.devErr
}

func (f *fake) GetDevice(ctx context.Context, idOrSerial string) (store.Device, error) {
	f.note("GetDevice")
	for _, d := range f.devices {
		if d.ID == idOrSerial || d.Serial == idOrSerial {
			return d, nil
		}
	}
	return store.Device{}, errors.New("no such device")
}

func (f *fake) ListTasksFiltered(ctx context.Context, idOrSerial, status string, limit int) ([]store.Task, error) {
	f.note("ListTasksFiltered")
	if f.taskErr != nil {
		return nil, f.taskErr
	}
	out := []store.Task{}
	for _, t := range f.tasks {
		if t.DeviceID != idOrSerial {
			continue
		}
		if status != "" && t.Status != status {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

func (f *fake) ListEvents(ctx context.Context, idOrSerial string, limit int, before time.Time) ([]store.Event, error) {
	f.note("ListEvents")
	out := []store.Event{}
	for _, e := range f.events {
		if e.DeviceID == idOrSerial {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fake) Params(ctx context.Context, idOrSerial, prefix string) (map[string]store.Param, error) {
	f.note("Params")
	if f.paramErr != nil {
		return nil, f.paramErr
	}
	out := map[string]store.Param{}
	for path, p := range f.params[idOrSerial] {
		if strings.HasPrefix(path, prefix) {
			out[path] = p
		}
	}
	return out, nil
}

func (f *fake) ListJobs(ctx context.Context, limit int) ([]store.Job, error) {
	f.note("ListJobs")
	return f.jobs, f.jobErr
}

func (f *fake) GetJob(ctx context.Context, id int64) (store.Job, error) {
	for _, j := range f.jobs {
		if j.ID == id {
			return j, nil
		}
	}
	return store.Job{}, errors.New("no such job")
}

func (f *fake) JobCounts(ctx context.Context, id int64) (map[string]int, error) {
	if f.jobErr != nil {
		return nil, f.jobErr
	}
	return f.counts[id], nil
}

func (f *fake) JobFailures(ctx context.Context, id int64) ([]store.JobFailure, error) {
	return f.failures, nil
}

func (f *fake) wake(ctx context.Context, idOrSerial string) error {
	f.woken = append(f.woken, idOrSerial)
	return f.wakeErr
}

func (f *fake) enqueue(ctx context.Context, idOrSerial string, tasks []store.NewTask) ([]store.Task, error) {
	if f.enqueueErr != nil {
		return nil, f.enqueueErr
	}
	out := make([]store.Task, 0, len(tasks))
	for _, t := range tasks {
		f.nextID++
		if f.nextID < 31 {
			f.nextID = 31
		}
		task := store.Task{ID: f.nextID, DeviceID: idOrSerial, Name: t.Name, Status: "pending", UpdatedAt: testNow}
		f.queued = append(f.queued, task)
		f.tasks = append(f.tasks, task) // the detail's TASKS block must see it
		out = append(out, task)
	}
	return out, nil
}

func newTestModel(f *fake) model {
	m := newModel(context.Background(), f.deps())
	m.w, m.h = 120, 40
	m.now = func() time.Time { return testNow }
	// Tests drive the tick by message; nothing is scheduled on a real clock.
	m.tick = func(time.Duration) tea.Cmd { return nil }
	return m
}

// settle feeds msg, then runs every command the model returns until it stops
// asking for work. That is the same loop bubbletea runs, minus the terminal.
func settle(t *testing.T, m model, msg tea.Msg) model {
	t.Helper()
	for i := 0; i < 100; i++ {
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, cmd := range batch {
				if cmd != nil {
					m = settle(t, m, cmd())
				}
			}
			return m
		}
		mm, cmd := m.Update(msg)
		next, ok := mm.(model)
		if !ok {
			t.Fatalf("Update returned %T, want model", mm)
		}
		m = next
		if cmd == nil {
			return m
		}
		msg = cmd()
	}
	t.Fatal("model never settled: command chain is too long")
	return m
}

func boot(t *testing.T, f *fake) model {
	t.Helper()
	m := newTestModel(f)
	return settle(t, m, m.Init()())
}

func press(t *testing.T, m model, k tea.KeyMsg) model {
	t.Helper()
	return settle(t, m, k)
}

func typed(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

var (
	keyDown  = tea.KeyMsg{Type: tea.KeyDown}
	keyEnter = tea.KeyMsg{Type: tea.KeyEnter}
	keyEsc   = tea.KeyMsg{Type: tea.KeyEsc}
	keyBack  = tea.KeyMsg{Type: tea.KeyBackspace}
)

func device(i int) store.Device {
	return store.Device{
		ID:              fmt.Sprintf("00259E-HG8145V5-ONT%04d", i),
		Serial:          fmt.Sprintf("ONT%04d", i),
		OUI:             "00259E",
		ProductClass:    "HG8145V5",
		SoftwareVersion: "V5R020C10S271",
		LastInform:      testNow.Add(-time.Minute),
		InformInterval:  300,
		LastAddr:        "10.0.0.12:40312",
	}
}

func nDevices(n int) []store.Device {
	out := make([]store.Device, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, device(i))
	}
	return out
}

func TestCursorBoundsAndShrinkAfterRefresh(t *testing.T) {
	f := &fake{devices: nDevices(5)}
	m := boot(t, f)
	if m.dev.cur.pos != 0 || len(m.dev.view) != 5 {
		t.Fatalf("after boot: pos=%d rows=%d", m.dev.cur.pos, len(m.dev.view))
	}
	for i := 0; i < 9; i++ {
		m = press(t, m, keyDown)
	}
	if m.dev.cur.pos != 4 {
		t.Fatalf("cursor past the end: pos=%d, want 4", m.dev.cur.pos)
	}

	// The list shrinks under the cursor: refresh must clamp, not index past.
	f.devices = nDevices(2)
	m = press(t, m, typed("r"))
	if m.dev.cur.pos != 1 {
		t.Fatalf("cursor after shrinking refresh: pos=%d, want 1", m.dev.cur.pos)
	}
	if _, ok := m.dev.selected(); !ok {
		t.Fatal("selection is out of bounds after clamp")
	}
}

func TestFilterNarrowsAndZeroMatch(t *testing.T) {
	f := &fake{devices: nDevices(3)}
	m := boot(t, f)

	m = press(t, m, typed("/"))
	if !m.dev.filter.editing {
		t.Fatal("/ did not start the filter")
	}
	m = press(t, m, typed("ont0002"))
	if len(m.dev.view) != 1 || m.dev.view[0].Serial != "ONT0002" {
		t.Fatalf("filter view: %+v", m.dev.view)
	}

	m = press(t, m, typed(""))
	m = press(t, m, typed("zzz"))
	if len(m.dev.view) != 0 || m.dev.cur.pos != 0 {
		t.Fatalf("zero-match view len=%d pos=%d", len(m.dev.view), m.dev.cur.pos)
	}
	if !strings.Contains(m.View(), "No device matches") {
		t.Fatalf("zero-match screen has no empty state:\n%s", m.View())
	}

	// While typing a filter, backspace edits the text instead of going back.
	m = press(t, m, keyBack)
	if !strings.HasSuffix(m.dev.filter.text, "zz") || strings.HasSuffix(m.dev.filter.text, "zzz") || m.screen != scrDevices {
		t.Fatalf("backspace in the filter: text=%q screen=%v", m.dev.filter.text, m.screen)
	}

	m = press(t, m, keyEsc)
	if m.dev.filter.text != "" || len(m.dev.view) != 3 {
		t.Fatalf("esc did not restore: text=%q rows=%d", m.dev.filter.text, len(m.dev.view))
	}
}

func TestFilterMatchesFreshness(t *testing.T) {
	f := &fake{devices: nDevices(2)}
	f.devices[0].LastInform = testNow.Add(-30 * time.Minute) // 6x300s: stale
	m := boot(t, f)
	m = press(t, m, typed("/"))
	m = press(t, m, typed("stale"))
	if len(m.dev.view) != 1 || m.dev.view[0].Serial != "ONT0001" {
		t.Fatalf("freshness filter view: %+v", m.dev.view)
	}
}

func TestFreshnessBoundaries(t *testing.T) {
	const interval = 300
	cases := []struct {
		age  time.Duration
		want string
	}{
		{0, "fresh"},
		{2 * interval * time.Second, "fresh"},
		{2*interval*time.Second + time.Second, "stale"},
		{6 * interval * time.Second, "stale"},
		{6*interval*time.Second + time.Second, "offline"},
		{-time.Minute, "fresh"},
	}
	for _, c := range cases {
		last := testNow.Add(-c.age)
		if got := freshness(last, interval, testNow); got != c.want {
			t.Errorf("age %s: got %q, want %q", c.age, got, c.want)
		}
	}
	// An interval the device never reported falls back to 300s, like the API.
	if got := freshness(testNow.Add(-600*time.Second), 0, testNow); got != "fresh" {
		t.Errorf("default interval: got %q, want fresh", got)
	}
	if got := freshness(testNow.Add(-601*time.Second), 0, testNow); got != "stale" {
		t.Errorf("default interval: got %q, want stale", got)
	}
}

func TestWakeErrorBecomesStatusLine(t *testing.T) {
	f := &fake{devices: nDevices(1), wakeErr: errors.New("connection request returned 401")}
	m := boot(t, f)

	m = press(t, m, typed("w"))
	if m.confirm == nil {
		t.Fatal("w did not ask for confirmation")
	}
	if !strings.Contains(m.View(), "Wake ONT0001 — HG8145V5?") {
		t.Fatalf("confirmation prompt missing:\n%s", m.View())
	}

	m = press(t, m, typed("n"))
	if m.confirm != nil || len(f.woken) != 0 || m.status != "cancelled" {
		t.Fatalf("n did not cancel: confirm=%v woke=%v status=%q", m.confirm, f.woken, m.status)
	}

	m = press(t, m, typed("w"))
	m = press(t, m, typed("y"))
	if len(f.woken) != 1 || f.woken[0] != device(1).ID {
		t.Fatalf("wake call: %v", f.woken)
	}
	if !m.statusBad || !strings.Contains(m.status, "connection request returned 401") {
		t.Fatalf("wake error status: bad=%v %q", m.statusBad, m.status)
	}
	if m.screen != scrDevices {
		t.Fatalf("wake error moved the screen to %v", m.screen)
	}
	if !strings.Contains(m.View(), "401") {
		t.Fatalf("wake error not rendered:\n%s", m.View())
	}
}

func TestReaderErrorIsStatusLineNotExit(t *testing.T) {
	f := &fake{devErr: errors.New("dial tcp: connection refused")}
	m := boot(t, f)
	if !m.statusBad || !strings.Contains(m.status, "connection refused") {
		t.Fatalf("status: bad=%v %q", m.statusBad, m.status)
	}
	if m.View() == "" {
		t.Fatal("screen went blank on a query error")
	}
}

func TestTabsKeepCursorAndAdoptDevice(t *testing.T) {
	f := &fake{devices: nDevices(4), tasks: []store.Task{
		{ID: 1, DeviceID: device(3).ID, Name: "reboot", Status: "pending", UpdatedAt: testNow},
	}}
	m := boot(t, f)
	m = press(t, m, keyDown)
	m = press(t, m, keyDown)

	m = press(t, m, typed("2"))
	if m.screen != scrTasks || m.task.deviceID != device(3).ID {
		t.Fatalf("tasks tab: screen=%v device=%q", m.screen, m.task.deviceID)
	}
	if len(m.task.view) != 1 {
		t.Fatalf("tasks for the adopted device: %+v", m.task.view)
	}

	m = press(t, m, typed("1"))
	if m.screen != scrDevices || m.dev.cur.pos != 2 {
		t.Fatalf("cursor lost: screen=%v pos=%d", m.screen, m.dev.cur.pos)
	}
}

func TestTaskStatusFilterCycle(t *testing.T) {
	id := device(1).ID
	f := &fake{
		devices: nDevices(1),
		tasks: []store.Task{
			{ID: 3, DeviceID: id, Name: "set", Status: "pending", UpdatedAt: testNow},
			{ID: 2, DeviceID: id, Name: "reboot", Status: "done", UpdatedAt: testNow},
			{ID: 1, DeviceID: id, Name: "get", Status: "failed", FaultCode: intp(9002), Fault: "Internal error", UpdatedAt: testNow},
		},
	}
	m := boot(t, f)
	m = press(t, m, typed("2"))
	if len(m.task.view) != 3 {
		t.Fatalf("all tasks: %d", len(m.task.view))
	}
	m = press(t, m, typed("s"))
	if m.task.status != "pending" || len(m.task.view) != 1 {
		t.Fatalf("pending filter: status=%q rows=%d", m.task.status, len(m.task.view))
	}
	m = press(t, m, typed("s"))
	if m.task.status != "running" || len(m.task.view) != 0 {
		t.Fatalf("running filter: status=%q rows=%d", m.task.status, len(m.task.view))
	}
	if !strings.Contains(m.View(), "No running tasks") {
		t.Fatalf("zero-match task state missing:\n%s", m.View())
	}
	if !strings.Contains(m.View(), "9002") && !strings.Contains(m.View(), "running") {
		t.Fatal("view should still name the filter")
	}
}

func TestDetailExpandsOneLevelAndShowsStoredValues(t *testing.T) {
	d := device(1)
	f := &fake{
		devices: []store.Device{d},
		params: map[string]map[string]store.Param{d.ID: {
			"InternetGatewayDevice.DeviceInfo.Manufacturer":                    {Value: "Huawei"},
			"InternetGatewayDevice.DeviceInfo.SoftwareVersion":                 {Value: "V5R020C10S271"},
			"InternetGatewayDevice.ManagementServer.URL":                       {Value: "http://acs:7547/"},
			"InternetGatewayDevice.ManagementServer.ConnectionRequestPassword": {Value: "hunter2"},
		}},
		events: []store.Event{{ID: 1, DeviceID: d.ID, At: testNow, Type: "boot", Detail: []byte(`{"codes":["1 BOOT"]}`)}},
	}
	m := boot(t, f)
	m = press(t, m, keyEnter)
	if m.screen != scrDevice {
		t.Fatalf("enter did not open the device: %v", m.screen)
	}
	if len(m.det.rows) != 1 || !m.det.rows[0].object {
		t.Fatalf("root level: %+v", m.det.rows)
	}

	m = press(t, m, keyEnter) // InternetGatewayDevice.
	if !m.det.expanded["InternetGatewayDevice."] || len(m.det.rows) != 3 {
		t.Fatalf("first level: expanded=%v rows=%+v", m.det.expanded, m.det.rows)
	}

	m = press(t, m, keyEnter) // DeviceInfo.
	if !m.det.expanded["InternetGatewayDevice.DeviceInfo."] || len(m.det.rows) != 5 {
		t.Fatalf("second level: rows=%+v", m.det.rows)
	}
	view := m.View()
	if !strings.Contains(view, "Manufacturer") || !strings.Contains(view, "Huawei") {
		t.Fatalf("leaf not shown:\n%s", view)
	}

	for i := 0; i < 3; i++ { // DeviceInfo -> ManagementServer.
		m = press(t, m, keyDown)
	}
	m = press(t, m, keyEnter)
	view = m.View()
	if !strings.Contains(view, "hunter2") {
		t.Fatalf("stored value not shown:\n%s", view)
	}
	if strings.Contains(view, "••••") {
		t.Fatalf("stored value masked:\n%s", view)
	}

	m = press(t, m, keyEsc) // collapse ManagementServer.
	if m.det.expanded["InternetGatewayDevice.ManagementServer."] {
		t.Fatal("esc did not collapse the leaf's parent")
	}
	m = press(t, m, keyBack) // backspace is esc: collapse the root object.
	if m.det.expanded["InternetGatewayDevice."] || len(m.det.rows) != 1 {
		t.Fatalf("backspace did not collapse to the root level: %v", m.det.expanded)
	}
	m = press(t, m, keyBack) // nothing left: back to the list.
	if m.screen != scrDevices {
		t.Fatalf("backspace did not leave the detail: %v", m.screen)
	}
}

func TestParamValueShowsStoredValues(t *testing.T) {
	if got := paramValue(store.Param{Value: "hunter2"}); got != "hunter2" {
		t.Fatalf("param rendered as %q, want the stored value", got)
	}
	if got := paramValue(store.Param{Value: "enc:abc"}); got != "enc:abc" {
		t.Fatalf("sealed-looking param rendered as %q", got)
	}
	if got := paramValue(store.Param{Value: "plain"}); got != "plain" {
		t.Fatalf("plain param rendered as %q", got)
	}
}

func TestEmptyDatabaseStates(t *testing.T) {
	m := boot(t, &fake{})
	if !strings.Contains(m.View(), "No devices yet") {
		t.Fatalf("devices empty state missing:\n%s", m.View())
	}
	m = press(t, m, typed("3"))
	if !strings.Contains(m.View(), "No jobs yet") {
		t.Fatalf("jobs empty state missing:\n%s", m.View())
	}
	m = press(t, m, typed("2"))
	if !strings.Contains(m.View(), "No device selected") {
		t.Fatalf("tasks empty state missing:\n%s", m.View())
	}
}

func TestJobDetailListsFailures(t *testing.T) {
	f := &fake{
		jobs:     []store.Job{{ID: 7, Name: "reboot-olt3", Status: "done", MaxParallel: 10, CreatedAt: testNow.Add(-time.Hour)}},
		counts:   map[int64]map[string]int{7: {"done": 3, "failed": 1}},
		failures: []store.JobFailure{{DeviceID: device(2).ID, TaskID: 42, FaultCode: intp(9005), Fault: "Invalid parameter"}},
	}
	m := boot(t, f)
	m = press(t, m, typed("3"))
	if !strings.Contains(m.View(), "3/4") {
		t.Fatalf("job progress missing:\n%s", m.View())
	}
	m = press(t, m, keyEnter)
	if m.screen != scrJob {
		t.Fatalf("enter did not open the job: %v", m.screen)
	}
	view := m.View()
	if !strings.Contains(view, "9005") || !strings.Contains(view, "Invalid parameter") {
		t.Fatalf("failure not rendered:\n%s", view)
	}
	m = press(t, m, keyEsc)
	if m.screen != scrJobs {
		t.Fatalf("esc did not return to jobs: %v", m.screen)
	}
}

func TestNarrowWidthDoesNotWrapOrPanic(t *testing.T) {
	m := newTestModel(&fake{devices: nDevices(3)})
	m.w = 34
	m.h = 12
	m = settle(t, m, m.Init()())
	for _, line := range strings.Split(m.View(), "\n") {
		if lipgloss.Width(line) > m.w {
			t.Fatalf("line wider than the terminal (%d): %q", m.w, line)
		}
	}
}

func TestQuitKeys(t *testing.T) {
	m := boot(t, &fake{devices: nDevices(1)})
	for _, k := range []tea.KeyMsg{typed("q"), {Type: tea.KeyCtrlC}} {
		_, cmd := m.Update(k)
		if cmd == nil {
			t.Fatalf("%v did not quit", k)
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Fatalf("%v returned %T, want tea.QuitMsg", k, cmd())
		}
	}
}

func intp(n int) *int { return &n }

func TestDetailHeaderIsAlignedTable(t *testing.T) {
	dev := device(1)
	dev.FirstInform = testNow.Add(-40 * time.Hour)
	dev.CRSetAt = &testNow
	dev.PendingReboot = true
	dev.LastFault = json.RawMessage(`{"code":9002,"message":"` + strings.Repeat("fault detail ", 30) + `"}`)
	f := &fake{devices: []store.Device{dev}}

	for _, width := range []int{80, 130} {
		t.Run(fmt.Sprintf("width %d", width), func(t *testing.T) {
			m := newTestModel(f)
			m.w = width
			m = settle(t, m, m.Init()())
			m = press(t, m, keyEnter)
			if m.screen != scrDevice {
				t.Fatalf("enter did not open the device: %v", m.screen)
			}

			rows := m.factRows()
			lines := m.detailFacts()
			if len(lines) != len(rows) {
				t.Fatalf("%d lines for %d facts", len(lines), len(rows))
			}

			labelWidth := 0
			for _, r := range rows {
				if n := lipgloss.Width(r[0]); n > labelWidth {
					labelWidth = n
				}
			}
			labelWidth++ // the gutter cell

			for i, line := range lines {
				if got := lipgloss.Width(line); got > width {
					t.Fatalf("fact line %d is %d wide, terminal is %d: %q", i, got, width, line)
				}
				plain := stripANSI(line)
				label := rows[i][0] + strings.Repeat(" ", labelWidth-lipgloss.Width(rows[i][0]))
				if !strings.HasPrefix(plain, label) {
					t.Errorf("fact %q does not start in the label column: %q", rows[i][0], plain)
					continue
				}
				want := stripANSI(clip(rows[i][1], width-labelWidth))
				if got := plain[len(label):]; got != want {
					t.Errorf("fact %q: value column %q, want %q", rows[i][0], got, want)
				}
			}

			last := stripANSI(lines[len(lines)-1])
			if !strings.HasSuffix(last, "…") {
				t.Fatalf("a value too wide for the column was not ellipsised: %q", last)
			}

			for i, line := range strings.Split(m.View(), "\n") {
				if got := lipgloss.Width(line); got > width {
					t.Fatalf("rendered line %d is %d wide, terminal is %d: %q", i, got, width, line)
				}
			}
		})
	}
}

// stripANSI removes SGR sequences so a test can talk about columns in bytes.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			j := i + 1
			if j < len(s) && s[j] == '[' {
				j++
				for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
					j++
				}
				if j < len(s) {
					j++
				}
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func TestDetailRebootQueuesTask(t *testing.T) {
	f := &fake{devices: nDevices(1)}
	m := boot(t, f)
	if strings.Contains(m.View(), "ACTIONS") {
		t.Fatal("ACTIONS must not appear on the list screen")
	}

	m = press(t, m, keyEnter)
	if m.screen != scrDevice {
		t.Fatalf("enter did not open the device: %v", m.screen)
	}
	if !strings.Contains(m.View(), "ACTIONS") || !strings.Contains(m.View(), "factory reset") {
		t.Fatalf("detail screen has no ACTIONS block:\n%s", m.View())
	}

	m = press(t, m, typed("b"))
	if m.confirm == nil {
		t.Fatal("b did not ask for confirmation")
	}
	if m.confirm.prompt != "Reboot ONT0001 — HG8145V5?" {
		t.Fatalf("reboot prompt: %q", m.confirm.prompt)
	}
	if !strings.Contains(m.confirm.detail, "restarts on its next session") {
		t.Fatalf("reboot consequence: %q", m.confirm.detail)
	}

	m = press(t, m, typed("y"))
	if len(f.queued) != 1 {
		t.Fatalf("queued %d tasks, want 1: %+v", len(f.queued), f.queued)
	}
	if f.queued[0].Name != "reboot" || f.queued[0].DeviceID != device(1).ID {
		t.Fatalf("queued task: %+v", f.queued[0])
	}
	if m.status != "queued reboot #31 — device woken" || m.statusBad {
		t.Fatalf("status: bad=%v %q", m.statusBad, m.status)
	}
	if !strings.Contains(m.View(), "#31 reboot") {
		t.Fatalf("TASKS block was not refreshed:\n%s", m.View())
	}
	if len(f.woken) != 0 {
		t.Fatalf("the console must not wake a task action itself: %v", f.woken)
	}
}

func TestDetailFactoryResetIsDestructiveAndConfirmed(t *testing.T) {
	f := &fake{devices: nDevices(1)}
	m := boot(t, f)
	m = press(t, m, keyEnter)

	m = press(t, m, typed("x"))
	if m.confirm == nil || m.confirm.prompt != "Factory reset ONT0001 — HG8145V5?" {
		t.Fatalf("reset prompt: %+v", m.confirm)
	}
	if !strings.Contains(m.confirm.detail, "Destructive") ||
		!strings.Contains(m.confirm.detail, "wipes the device configuration") ||
		!strings.Contains(m.confirm.detail, "connection-request credentials") {
		t.Fatalf("reset consequence: %q", m.confirm.detail)
	}
	if !strings.Contains(m.View(), "wipes the device configuration") {
		t.Fatalf("consequence is not rendered:\n%s", m.View())
	}

	m = press(t, m, typed("n"))
	if len(f.queued) != 0 {
		t.Fatalf("n queued %+v", f.queued)
	}

	m = press(t, m, typed("x"))
	m = press(t, m, typed("y"))
	if len(f.queued) != 1 || f.queued[0].Name != "reset" {
		t.Fatalf("queued: %+v", f.queued)
	}
	if m.status != "queued factory reset #31 — device woken" {
		t.Fatalf("status: %q", m.status)
	}
}

func TestConfirmBlocksOtherKeys(t *testing.T) {
	f := &fake{devices: nDevices(1)}
	m := boot(t, f)
	m = press(t, m, keyEnter)
	m = press(t, m, typed("b"))
	if m.confirm == nil {
		t.Fatal("no confirmation to test")
	}

	for _, k := range []tea.KeyMsg{typed("b"), typed("x"), typed("w"), keyTab, keyShiftTab} {
		m = press(t, m, k)
	}
	m = press(t, m, keyDown)
	m = press(t, m, keyEnter)
	m = press(t, m, typed("r"))
	m = press(t, m, typed("/"))
	if m.det.focus != focusParams {
		t.Fatalf("a key moved the pane focus during a confirmation: %v", m.det.focus)
	}
	if m.confirm == nil {
		t.Fatal("a non-y/n key closed the confirmation")
	}
	if len(f.queued) != 0 || len(f.woken) != 0 {
		t.Fatalf("keys inside the confirmation acted: queued=%+v woken=%v", f.queued, f.woken)
	}

	m = press(t, m, keyBack)
	if m.confirm != nil || len(f.queued) != 0 || m.status != "cancelled" {
		t.Fatalf("backspace did not cancel cleanly: confirm=%v queued=%+v status=%q", m.confirm, f.queued, m.status)
	}
}

func TestEnqueueErrorIsStatusLine(t *testing.T) {
	f := &fake{devices: nDevices(1), enqueueErr: errors.New("task rejected")}
	m := boot(t, f)
	m = press(t, m, keyEnter)
	m = press(t, m, typed("b"))
	m = press(t, m, typed("y"))

	if !m.statusBad || !strings.Contains(m.status, "task rejected") {
		t.Fatalf("status: bad=%v %q", m.statusBad, m.status)
	}
	if m.screen != scrDevice || m.View() == "" {
		t.Fatalf("enqueue error lost the screen: %v", m.screen)
	}
	if len(f.queued) != 0 {
		t.Fatalf("a failed enqueue recorded tasks: %+v", f.queued)
	}
}

func TestListWakeStillCallsWakeOnly(t *testing.T) {
	f := &fake{devices: nDevices(1)}
	m := boot(t, f)
	m = press(t, m, typed("w"))
	m = press(t, m, typed("y"))

	if len(f.woken) != 1 || f.woken[0] != device(1).ID {
		t.Fatalf("wake calls: %v", f.woken)
	}
	if len(f.queued) != 0 {
		t.Fatalf("the list's w queued tasks: %+v", f.queued)
	}
}

// send feeds any message, not just a key, through the same update loop.
func send(t *testing.T, m model, msg tea.Msg) model {
	t.Helper()
	return settle(t, m, msg)
}

func TestTickDoesNothingWhileConfirming(t *testing.T) {
	f := &fake{devices: nDevices(1)}
	m := boot(t, f)
	m = press(t, m, typed("w"))
	if m.confirm == nil {
		t.Fatal("no confirmation open")
	}
	before := f.count("ListDevices")
	m = send(t, m, tickMsg{})
	if got := f.count("ListDevices"); got != before {
		t.Fatalf("tick reloaded during a confirmation: %d -> %d", before, got)
	}
	if m.confirm == nil || m.screen != scrDevices {
		t.Fatalf("tick disturbed the confirmation: %+v", m.confirm)
	}
}

func TestTickCmdCarriesTheTime(t *testing.T) {
	msg := tickCmd(time.Millisecond)()
	if _, ok := msg.(tickMsg); !ok {
		t.Fatalf("tick command returned %T", msg)
	}

	// A tick on a tab that is due reloads; the chain is re-armed by the same
	// update, so there is never more than one timer.
	m := boot(t, &fake{devices: nDevices(1)})
	m.tick = tickCmd
	_, cmd := m.Update(tickMsg{})
	if cmd == nil {
		t.Fatal("the tick chain did not re-arm")
	}
}

func TestTickReloadsOnlyTheActiveTab(t *testing.T) {
	f := &fake{
		devices: nDevices(2),
		tasks:   []store.Task{{ID: 1, DeviceID: device(1).ID, Name: "get", Status: "done", UpdatedAt: testNow}},
	}
	m := boot(t, f)

	devices := f.count("ListDevices")
	m = send(t, m, tickMsg{})
	if got := f.count("ListDevices"); got != devices+1 {
		t.Fatalf("devices tick: ListDevices %d -> %d", devices, got)
	}
	if f.count("ListTasksFiltered") != 0 || f.count("ListJobs") != 0 {
		t.Fatalf("devices tick touched another reader: tasks=%d jobs=%d",
			f.count("ListTasksFiltered"), f.count("ListJobs"))
	}

	m = press(t, m, typed("2"))
	devices = f.count("ListDevices")
	tasks := f.count("ListTasksFiltered")
	m = send(t, m, tickMsg{})
	if f.count("ListTasksFiltered") != tasks+1 {
		t.Fatalf("tasks tick did not reload tasks: %d", f.count("ListTasksFiltered"))
	}
	if f.count("ListDevices") != devices {
		t.Fatal("tasks tick reloaded devices")
	}

	m = press(t, m, typed("3"))
	tasks = f.count("ListTasksFiltered")
	jobs := f.count("ListJobs")
	m = send(t, m, tickMsg{})
	if f.count("ListJobs") != jobs+1 {
		t.Fatalf("jobs tick did not reload jobs: %d", f.count("ListJobs"))
	}
	if f.count("ListTasksFiltered") != tasks {
		t.Fatal("jobs tick reloaded tasks")
	}
}

func TestTickKeepsFilterAndCursor(t *testing.T) {
	f := &fake{devices: nDevices(3)}
	m := boot(t, f)
	m = press(t, m, keyDown)
	m = press(t, m, keyDown)
	m = press(t, m, typed("/"))
	m = press(t, m, typed("ont"))
	if len(m.dev.view) != 3 || m.dev.cur.pos != 2 {
		t.Fatalf("pre-tick state: rows=%d pos=%d", len(m.dev.view), m.dev.cur.pos)
	}
	selected := m.dev.view[2].Serial

	f.devices = nDevices(4)
	m = send(t, m, tickMsg{})
	if m.dev.filter.text != "ont" || !m.dev.filter.editing {
		t.Fatalf("tick dropped the filter: %q editing=%v", m.dev.filter.text, m.dev.filter.editing)
	}
	if m.dev.cur.pos != 2 || m.dev.view[2].Serial != selected {
		t.Fatalf("tick moved the cursor: pos=%d selected=%q, want %q",
			m.dev.cur.pos, m.dev.view[m.dev.cur.pos].Serial, selected)
	}
	if len(m.dev.view) != 4 {
		t.Fatalf("tick did not reload: %d rows", len(m.dev.view))
	}
}

func TestTabsCycleThreeScreens(t *testing.T) {
	m := boot(t, &fake{devices: nDevices(1)})
	steps := []struct {
		k    tea.KeyMsg
		want screen
	}{
		{tea.KeyMsg{Type: tea.KeyTab}, scrTasks},
		{tea.KeyMsg{Type: tea.KeyTab}, scrJobs},
		{tea.KeyMsg{Type: tea.KeyTab}, scrDevices},
		{tea.KeyMsg{Type: tea.KeyShiftTab}, scrJobs},
	}
	for _, step := range steps {
		m = press(t, m, step.k)
		if m.screen != step.want {
			t.Fatalf("after %v: screen %v, want %v", step.k, m.screen, step.want)
		}
	}
}

func TestFourDoesNothing(t *testing.T) {
	m := boot(t, &fake{devices: nDevices(1)})
	m = press(t, m, typed("4"))
	if m.screen != scrDevices {
		t.Fatalf("4 moved the screen to %v", m.screen)
	}
	if m.status != "" || m.loading != "" {
		t.Fatalf("4 said something: status=%q loading=%q", m.status, m.loading)
	}
	if strings.Contains(m.View(), "logs") {
		t.Fatalf("the tab bar still mentions logs:\n%s", m.View())
	}
}

func TestServingHeaderStates(t *testing.T) {
	f := &fake{devices: nDevices(1)}
	m := boot(t, f)
	if got := f.count("Serving"); got != 1 {
		t.Fatalf("serving checked %d times at start, want 1", got)
	}
	if !strings.Contains(m.View(), "● serving") {
		t.Fatalf("no serving mark:\n%s", m.View())
	}
	if strings.Contains(m.View(), "daemon not running") {
		t.Fatal("down alert shown while serving")
	}

	f.serveDown = true
	m = send(t, m, tickMsg{})
	view := m.View()
	if strings.Contains(view, "● serving") {
		t.Fatalf("stale serving mark:\n%s", view)
	}
	if !strings.Contains(view, "✕ daemon not running — devices cannot reach the ACS; wakes and tasks wait") {
		t.Fatalf("down alert missing:\n%s", view)
	}
}

func TestServingFlipKeepsScreenState(t *testing.T) {
	f := &fake{devices: nDevices(3)}
	m := boot(t, f)
	m = press(t, m, keyDown)
	m = press(t, m, typed("/"))
	m = press(t, m, typed("ont"))
	if m.dev.cur.pos != 1 || len(m.dev.view) != 3 {
		t.Fatalf("pre-tick state: pos=%d rows=%d", m.dev.cur.pos, len(m.dev.view))
	}
	selected := m.dev.view[1].Serial

	f.serveDown = true
	m = send(t, m, tickMsg{})
	if m.dev.filter.text != "ont" || !m.dev.filter.editing {
		t.Fatalf("serving flip dropped the filter: %q editing=%v", m.dev.filter.text, m.dev.filter.editing)
	}
	if m.dev.cur.pos != 1 || m.dev.view[1].Serial != selected {
		t.Fatalf("serving flip moved the cursor: pos=%d want %q", m.dev.cur.pos, selected)
	}
	if m.serve != serveDown {
		t.Fatalf("state did not flip: %v", m.serve)
	}

	// A confirmation stays open across the status read, and the list behind it
	// keeps its rows.
	m = press(t, m, keyEsc) // leave filter editing
	m = press(t, m, typed("w"))
	if m.confirm == nil {
		t.Fatal("no confirmation open")
	}
	id := m.confirm.id
	m = send(t, m, tickMsg{})
	if m.confirm == nil || m.confirm.id != id {
		t.Fatalf("status read closed the confirmation: %+v", m.confirm)
	}
	if !strings.Contains(m.View(), "daemon not running") {
		t.Fatalf("alert missing during a confirmation:\n%s", m.View())
	}
}

func TestServingFlipKeepsParameterTree(t *testing.T) {
	d := device(1)
	f := &fake{
		devices: []store.Device{d},
		params: map[string]map[string]store.Param{d.ID: {
			"InternetGatewayDevice.DeviceInfo.Manufacturer": {Value: "Huawei"},
			"InternetGatewayDevice.DeviceInfo.UpTime":       {Value: "10"},
		}},
	}
	m := boot(t, f)
	m = press(t, m, keyEnter)
	m = press(t, m, keyEnter)
	if !m.det.expanded["InternetGatewayDevice."] || len(m.det.rows) != 2 {
		t.Fatalf("tree did not expand: %v rows=%d", m.det.expanded, len(m.det.rows))
	}
	rows := len(m.det.rows)

	f.serveDown = true
	m = send(t, m, tickMsg{})
	if len(m.det.rows) != rows || !m.det.expanded["InternetGatewayDevice."] {
		t.Fatalf("status read reset the parameter tree: rows=%d want %d", len(m.det.rows), rows)
	}
	if !strings.Contains(m.View(), "daemon not running") {
		t.Fatalf("detail screen lost the daemon alert:\n%s", m.View())
	}
}

func TestServingErrorShowsUnknown(t *testing.T) {
	f := &fake{devices: nDevices(1), servingErr: errors.New("advisory lock query failed")}
	m := boot(t, f)
	view := m.View()
	if !strings.Contains(view, "? daemon status unknown") {
		t.Fatalf("unknown mark missing:\n%s", view)
	}
	if strings.Contains(view, "● serving") || strings.Contains(view, "✕") {
		t.Fatal("unknown state rendered as known")
	}
	if !m.statusBad || !strings.Contains(m.status, "advisory lock query failed") {
		t.Fatalf("status: bad=%v %q", m.statusBad, m.status)
	}
	if m.View() == "" {
		t.Fatal("screen went blank on a serving error")
	}
}

func TestServingNilRendersNothing(t *testing.T) {
	f := &fake{devices: nDevices(1)}
	m := newTestModel(f)
	m.deps.Serving = nil
	if cmd := m.checkServing(); cmd != nil {
		t.Fatal("nil Serving still scheduled a read")
	}
	m = settle(t, m, m.Init()())
	view := m.View()
	for _, needle := range []string{"serving", "daemon", "status unknown"} {
		if strings.Contains(view, needle) {
			t.Fatalf("nil Deps.Serving rendered %q:\n%s", needle, view)
		}
	}
}

func TestConfirmWarnsOnlyWhenDown(t *testing.T) {
	f := &fake{devices: nDevices(1)}
	m := boot(t, f)
	m = press(t, m, keyEnter)
	m = press(t, m, typed("b"))
	if strings.Contains(m.View(), serveWarning) {
		t.Fatalf("warning shown while serving:\n%s", m.View())
	}
	m = press(t, m, keyEsc)

	f.serveDown = true
	m = send(t, m, tickMsg{})
	m = press(t, m, typed("b"))
	if !strings.Contains(m.View(), serveWarning) {
		t.Fatalf("confirmation lacks the down warning:\n%s", m.View())
	}

	// The warning is information, not a block: the task is still queued.
	m = press(t, m, typed("y"))
	if len(f.queued) != 1 || f.queued[0].Name != "reboot" {
		t.Fatalf("down state blocked the action: %+v", f.queued)
	}
}

func viewLines(m model) []string {
	var out []string
	for _, line := range strings.Split(m.View(), "\n") {
		out = append(out, stripANSI(line))
	}
	return out
}

func lineIndex(lines []string, needle string) int {
	for i, line := range lines {
		if strings.Contains(line, needle) {
			return i
		}
	}
	return -1
}

func TestDetailHeaderTwoColumns(t *testing.T) {
	dev := device(1)
	dev.LastAddr = "10.0.0.12:40312"
	dev.CRSetAt = &testNow
	f := &fake{
		devices: []store.Device{dev},
		params: map[string]map[string]store.Param{dev.ID: {
			"InternetGatewayDevice.DeviceInfo.Manufacturer": {Value: "Huawei"},
		}},
	}
	open := func(width int) model {
		m := newTestModel(f)
		m.w, m.h = width, 30
		m = settle(t, m, m.Init()())
		return press(t, m, keyEnter)
	}

	wide := viewLines(open(120))
	paramsWide := lineIndex(wide, "PARAMETERS")
	actionsLine := lineIndex(wide, "ACTIONS")
	if actionsLine < 0 || paramsWide < 0 {
		t.Fatalf("missing blocks at 120:\n%s", strings.Join(wide, "\n"))
	}
	if !strings.HasPrefix(wide[actionsLine], "id ") {
		t.Fatalf("actions are not beside the facts at 120: %q", wide[actionsLine])
	}
	if wake := lineIndex(wide, "wake"); wake < 0 || !strings.Contains(wide[wake], "serial") {
		t.Fatalf("actions are not on the facts' rows at 120: %q", wide[wake])
	}
	actionsCol := strings.Index(wide[actionsLine], "ACTIONS")
	if actionsCol <= 0 || actionsCol >= 60 {
		t.Fatalf("actions column %d is not bounded at 120", actionsCol)
	}

	// A longer id must not move the actions column or the parameters block.
	long := device(1)
	long.ID = "00AABB-HG8245H5-48575443BD7589AB"
	fl := &fake{devices: []store.Device{long}}
	ml := newTestModel(fl)
	ml.w, ml.h = 120, 30
	ml = settle(t, ml, ml.Init()())
	ml = press(t, ml, keyEnter)
	longLines := viewLines(ml)
	longActions := lineIndex(longLines, "ACTIONS")
	if longActions < 0 {
		t.Fatalf("no ACTIONS with a long id:\n%s", strings.Join(longLines, "\n"))
	}
	if got := strings.Index(longLines[longActions], "ACTIONS"); got != actionsCol {
		t.Fatalf("actions column moved with a longer id: %d -> %d", actionsCol, got)
	}
	if got := lineIndex(longLines, "PARAMETERS"); got != paramsWide {
		t.Fatalf("parameters moved with a longer id: %d -> %d", paramsWide, got)
	}

	narrow := viewLines(open(80))
	paramsNarrow := lineIndex(narrow, "PARAMETERS")
	actionsNarrow := lineIndex(narrow, "ACTIONS")
	if actionsNarrow < 0 || paramsNarrow < 0 {
		t.Fatalf("missing blocks at 80:\n%s", strings.Join(narrow, "\n"))
	}
	for _, line := range narrow {
		if strings.Contains(line, "ACTIONS") && strings.Contains(line, "ONT0001") {
			t.Fatalf("actions still beside the facts at 80: %q", line)
		}
	}
	if actionsNarrow <= lineIndex(narrow, "last fault") {
		t.Fatalf("actions are not below the facts at 80: actions=%d", actionsNarrow)
	}
	if wake := lineIndex(narrow, "wake"); wake <= actionsNarrow {
		t.Fatalf("wake row is not under ACTIONS at 80: %q", narrow[wake])
	}
	if paramsNarrow <= paramsWide {
		t.Fatalf("parameters start at %d at 80 and %d at 120: stacked must be lower", paramsNarrow, paramsWide)
	}
}

func TestDetailHeaderWidthsStable(t *testing.T) {
	dev := device(1)
	dev.LastAddr = "10.0.0.12"
	f := &fake{devices: []store.Device{dev}}
	m := newTestModel(f)
	m.w, m.h = 120, 30
	m = settle(t, m, m.Init()())
	m = press(t, m, keyEnter)

	before := viewLines(m)
	beforeParams := lineIndex(before, "PARAMETERS")
	beforeActions := lineIndex(before, "ACTIONS")
	beforeCol := strings.Index(before[beforeActions], "ACTIONS")

	// A longer address and a stale device move values, not columns.
	f.devices[0].LastAddr = "203.0.113.250:65535"
	f.devices[0].LastInform = testNow.Add(-30 * time.Minute)
	m = press(t, m, typed("r"))

	after := viewLines(m)
	if got := lineIndex(after, "PARAMETERS"); got != beforeParams {
		t.Fatalf("parameters moved: %d -> %d", beforeParams, got)
	}
	afterActions := lineIndex(after, "ACTIONS")
	if afterActions != beforeActions {
		t.Fatalf("actions row moved: %d -> %d", beforeActions, afterActions)
	}
	if got := strings.Index(after[afterActions], "ACTIONS"); got != beforeCol {
		t.Fatalf("actions column moved: %d -> %d", beforeCol, got)
	}
	if !strings.Contains(after[lineIndex(after, "addr")], "203.0.113.250:65535") {
		t.Fatal("address did not update")
	}
}

func paramFilterFixture(t *testing.T) (*fake, model) {
	t.Helper()
	dev := device(1)
	f := &fake{
		devices: []store.Device{dev},
		params: map[string]map[string]store.Param{dev.ID: {
			"InternetGatewayDevice.DeviceInfo.Manufacturer":                      {Value: "Huawei"},
			"InternetGatewayDevice.DeviceInfo.SoftwareVersion":                   {Value: "V5R020C10S271"},
			"InternetGatewayDevice.LANDevice.1.WLANConfiguration.1.SSID":         {Value: "Home"},
			"InternetGatewayDevice.LANDevice.1.WLANConfiguration.1.PreSharedKey": {Value: "hunter2"},
			"InternetGatewayDevice.ManagementServer.URL":                         {Value: "http://acs:7547/"},
		}},
	}
	m := newTestModel(f)
	m.w, m.h = 120, 40
	m = settle(t, m, m.Init()())
	m = press(t, m, keyEnter)
	return f, m
}

func TestDetailParamFilter(t *testing.T) {
	_, m := paramFilterFixture(t)

	m = press(t, m, typed("/"))
	if !m.det.filter.editing {
		t.Fatal("/ did not start the parameter filter")
	}
	m = press(t, m, typed("wlan"))

	view := m.View()
	if !strings.Contains(view, "PARAMETERS  2 of 5  /wlan") {
		t.Fatalf("match count missing:\n%s", view)
	}
	if !strings.Contains(view, "SSID") || !strings.Contains(view, "PreSharedKey") {
		t.Fatalf("matching leaves missing:\n%s", view)
	}
	if strings.Contains(view, "Manufacturer") {
		t.Fatalf("non-matching leaf shown:\n%s", view)
	}
	for _, parent := range [...]string{"InternetGatewayDevice", "LANDevice", "WLANConfiguration"} {
		if !strings.Contains(stripANSI(view), parent) {
			t.Fatalf("parent %q not kept:\n%s", parent, view)
		}
	}
	m = press(t, m, keyEnter) // keep the filter
	if m.det.filter.text != "wlan" || m.det.filter.editing {
		t.Fatalf("enter did not keep the filter: %q editing=%v", m.det.filter.text, m.det.filter.editing)
	}
	rows := len(m.det.rows)
	m = press(t, m, keyEnter) // no expansion while searching
	if len(m.det.rows) != rows {
		t.Fatal("enter changed the tree while filtering")
	}
	for i := 0; i < 8; i++ {
		m = press(t, m, keyDown)
		if m.det.cur.pos >= len(m.det.rows) {
			t.Fatalf("cursor %d outside %d rows", m.det.cur.pos, len(m.det.rows))
		}
	}

	m = press(t, m, keyEsc)
	if m.det.filter.text != "" || m.det.filtering() {
		t.Fatalf("esc did not restore the tree: %q", m.det.filter.text)
	}
	if len(m.det.rows) != 1 {
		t.Fatalf("full tree after clearing: %d rows, want 1", len(m.det.rows))
	}
	if strings.Contains(m.View(), " of 5 ") {
		t.Fatal("match count survived the clear")
	}
}

func TestDetailParamFilterNoMatch(t *testing.T) {
	_, m := paramFilterFixture(t)
	m = press(t, m, typed("/"))
	m = press(t, m, typed("zzz"))
	view := m.View()
	if !strings.Contains(view, `no parameter matches "zzz"`) {
		t.Fatalf("empty match state missing:\n%s", view)
	}
	if m.det.cur.pos != 0 {
		t.Fatalf("cursor outside the empty list: %d", m.det.cur.pos)
	}
	m = press(t, m, keyEsc)
	if len(m.det.rows) != 1 {
		t.Fatalf("esc did not restore: %d rows", len(m.det.rows))
	}
}

func TestDetailParamFilterEmptyCache(t *testing.T) {
	d := device(1)
	f := &fake{devices: []store.Device{d}}
	m := newTestModel(f)
	m.w, m.h = 120, 30
	m = settle(t, m, m.Init()())
	m = press(t, m, keyEnter)
	m = press(t, m, typed("/"))
	if !m.det.filter.editing {
		t.Fatal("filter did not open without cached params")
	}
	m = press(t, m, typed("wlan"))
	if !strings.Contains(m.View(), "no cached parameters to search") {
		t.Fatalf("empty cache state missing:\n%s", m.View())
	}
}

func TestDetailParamFilterKeepsSelectionWhenVisible(t *testing.T) {
	f, m := paramFilterFixture(t)
	dev := f.devices[0]
	for _, prefix := range []string{
		"InternetGatewayDevice.",
		"InternetGatewayDevice.LANDevice.",
		"InternetGatewayDevice.LANDevice.1.",
		"InternetGatewayDevice.LANDevice.1.WLANConfiguration.",
		"InternetGatewayDevice.LANDevice.1.WLANConfiguration.1.",
	} {
		m.det.expanded[prefix] = true
		m.det.pages[prefix] = buildPage(f.params[dev.ID], prefix)
	}
	m.det.rebuild()

	const ssid = "InternetGatewayDevice.LANDevice.1.WLANConfiguration.1.SSID"
	m.det.selectPrefix(ssid)
	if row, ok := m.det.selectedRow(); !ok || row.prefix != ssid {
		t.Fatalf("fixture cursor: %+v", row)
	}

	m = press(t, m, typed("/"))
	m = press(t, m, typed("ssid"))
	m = press(t, m, keyEnter) // keep
	if row, ok := m.det.selectedRow(); !ok || row.prefix != ssid {
		t.Fatalf("filter lost the selection: %+v", row)
	}

	m = press(t, m, keyEsc) // clear the filter
	if m.det.filter.text != "" {
		t.Fatalf("filter not cleared: %q", m.det.filter.text)
	}
	if row, ok := m.det.selectedRow(); !ok || row.prefix != ssid {
		t.Fatalf("clearing the filter lost the visible selection: %+v", row)
	}
}

var (
	keyTab      = tea.KeyMsg{Type: tea.KeyTab}
	keyShiftTab = tea.KeyMsg{Type: tea.KeyShiftTab}
)

// detailFixture opens the detail screen with n tasks and n events.
func detailFixture(t *testing.T, tasks, events int) (*fake, model) {
	t.Helper()
	dev := device(1)
	f := &fake{devices: []store.Device{dev}}
	for i := 1; i <= tasks; i++ {
		f.tasks = append(f.tasks, store.Task{ID: int64(i), DeviceID: dev.ID, Name: "set",
			Status: "failed", Attempts: 2, FaultCode: intp(9003), Fault: "Invalid arguments",
			UpdatedAt: testNow.Add(-time.Duration(i) * time.Minute)})
	}
	for i := 0; i < events; i++ {
		f.events = append(f.events, store.Event{ID: int64(i + 1), DeviceID: dev.ID,
			At: testNow.Add(-time.Duration(i) * time.Minute), Type: "periodic",
			Detail: []byte(`{"codes":["2 PERIODIC"]}`)})
	}
	m := newTestModel(f)
	m.w, m.h = 120, 40
	m = settle(t, m, m.Init()())
	return f, press(t, m, keyEnter)
}

func TestDetailFocusCyclesAndWraps(t *testing.T) {
	_, m := detailFixture(t, 2, 2)
	if m.det.focus != focusParams {
		t.Fatalf("initial focus: %v", m.det.focus)
	}
	steps := []struct {
		key  tea.KeyMsg
		want detailFocus
	}{
		{keyTab, focusTasks},
		{keyTab, focusEvents},
		{keyTab, focusParams},
		{keyShiftTab, focusEvents},
		{keyShiftTab, focusTasks},
		{keyShiftTab, focusParams},
	}
	for _, step := range steps {
		m = press(t, m, step.key)
		if m.det.focus != step.want {
			t.Fatalf("%v: focus %v, want %v", step.key, m.det.focus, step.want)
		}
	}
	if !strings.Contains(stripANSI(m.View()), "▸ PARAMETERS") {
		t.Fatalf("focused pane is not marked:\n%s", m.View())
	}
}

func TestDetailPaneCursorsIndependent(t *testing.T) {
	_, m := detailFixture(t, 20, 10)
	m = press(t, m, keyTab) // tasks
	for i := 0; i < 3; i++ {
		m = press(t, m, keyDown)
	}
	m = press(t, m, keyTab) // events
	m = press(t, m, keyDown)
	m = press(t, m, keyShiftTab) // back to tasks
	if m.det.taskCur.pos != 3 {
		t.Fatalf("task cursor reset: %d", m.det.taskCur.pos)
	}
	if m.det.eventCur.pos != 1 {
		t.Fatalf("event cursor: %d", m.det.eventCur.pos)
	}
	m = press(t, m, keyTab) // events again
	if m.det.eventCur.pos != 1 {
		t.Fatalf("event cursor lost on return: %d", m.det.eventCur.pos)
	}
}

func TestDetailTaskListScrollsToLastRow(t *testing.T) {
	_, m := detailFixture(t, 20, 10)
	m = press(t, m, keyTab)
	m = press(t, m, typed("G"))
	if m.det.taskCur.pos != 19 {
		t.Fatalf("cursor after G: %d", m.det.taskCur.pos)
	}
	view := stripANSI(m.View())
	if !strings.Contains(view, "TASKS  20 of 20") {
		t.Fatalf("task header does not show the position:\n%s", view)
	}
	if !strings.Contains(view, "#20 set failed") {
		t.Fatalf("last task row not rendered:\n%s", view)
	}
}

func countTaskRows(lines []string) int {
	n := 0
	for _, line := range lines {
		if strings.HasPrefix(line, "  #") || strings.HasPrefix(line, "› #") {
			n++
		}
	}
	return n
}

func TestDetailPaneRowsGrowWithHeight(t *testing.T) {
	f, _ := detailFixture(t, 20, 10)
	render := func(h int) int {
		m := newTestModel(f)
		m.w, m.h = 120, h
		m = settle(t, m, m.Init()())
		m = press(t, m, keyEnter)
		return countTaskRows(viewLines(m))
	}
	short, tall := render(30), render(46)
	if short < 2 {
		t.Fatalf("only %d task rows at 30 lines", short)
	}
	if tall <= short {
		t.Fatalf("rows did not grow with the terminal: %d at 46, %d at 30", tall, short)
	}

	// Never more rows than were loaded.
	f2, _ := detailFixture(t, 2, 1)
	m := newTestModel(f2)
	m.w, m.h = 120, 46
	m = settle(t, m, m.Init()())
	m = press(t, m, keyEnter)
	if got := countTaskRows(viewLines(m)); got > 2 {
		t.Fatalf("rendered %d task rows for 2 tasks", got)
	}
}

func TestDetailHelpLineIsTrimmed(t *testing.T) {
	f, _ := detailFixture(t, 2, 2)
	m := newTestModel(f)
	m.w, m.h = 80, 30
	m = settle(t, m, m.Init()())
	m = press(t, m, keyEnter)

	lines := viewLines(m)
	help := lines[len(lines)-1]
	if lipgloss.Width(help) > 80 {
		t.Fatalf("help line is %d wide at 80 columns: %q", lipgloss.Width(help), help)
	}
	for _, gone := range []string{"w wake", "b reboot", "x reset"} {
		if strings.Contains(help, gone) {
			t.Fatalf("help line still lists %q: %q", gone, help)
		}
	}
	for _, want := range []string{"tab", "esc", "/", "r refresh", "q quit"} {
		if !strings.Contains(help, want) {
			t.Fatalf("help line lacks %q: %q", want, help)
		}
	}
}

func TestDetailSlashFromEventsFocusesParams(t *testing.T) {
	_, m := detailFixture(t, 2, 2)
	m = press(t, m, keyTab)
	m = press(t, m, keyTab) // events
	if m.det.focus != focusEvents {
		t.Fatalf("focus: %v", m.det.focus)
	}
	m = press(t, m, typed("/"))
	if m.det.focus != focusParams || !m.det.filter.editing {
		t.Fatalf("slash did not focus the tree: focus=%v editing=%v", m.det.focus, m.det.filter.editing)
	}
	m = press(t, m, typed("manu"))
	if !strings.Contains(stripANSI(m.View()), "PARAMETERS  0 of") {
		t.Fatalf("filter did not apply:\n%s", m.View())
	}
}

func TestDetailEscFocusOrder(t *testing.T) {
	_, m := detailFixture(t, 2, 2)
	m = press(t, m, keyTab) // tasks
	m = press(t, m, keyEsc)
	if m.det.focus != focusParams || m.screen != scrDevice {
		t.Fatalf("esc from tasks: focus=%v screen=%v", m.det.focus, m.screen)
	}
	m = press(t, m, keyEsc)
	if m.screen != scrDevices {
		t.Fatalf("esc from parameters did not leave the detail: %v", m.screen)
	}
}
