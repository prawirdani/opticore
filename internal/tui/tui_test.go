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
}

func (f *fake) deps() Deps {
	return Deps{Devices: f, Tasks: f, Jobs: f, Events: f, Params: f, Wake: f.wake}
}

func (f *fake) ListDevices(ctx context.Context, flt store.ListFilter) ([]store.Device, error) {
	return f.devices, f.devErr
}

func (f *fake) GetDevice(ctx context.Context, idOrSerial string) (store.Device, error) {
	for _, d := range f.devices {
		if d.ID == idOrSerial || d.Serial == idOrSerial {
			return d, nil
		}
	}
	return store.Device{}, errors.New("no such device")
}

func (f *fake) ListTasksFiltered(ctx context.Context, idOrSerial, status string, limit int) ([]store.Task, error) {
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
	out := []store.Event{}
	for _, e := range f.events {
		if e.DeviceID == idOrSerial {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fake) Params(ctx context.Context, idOrSerial, prefix string) (map[string]store.Param, error) {
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

func newTestModel(f *fake) model {
	m := newModel(context.Background(), f.deps())
	m.w, m.h = 120, 40
	m.now = func() time.Time { return testNow }
	return m
}

// settle feeds msg, then runs every command the model returns until it stops
// asking for work. That is the same loop bubbletea runs, minus the terminal.
func settle(t *testing.T, m model, msg tea.Msg) model {
	t.Helper()
	for i := 0; i < 100; i++ {
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
	if !strings.Contains(m.View(), "Wake ONT0001 now?") {
		t.Fatalf("confirmation prompt missing:\n%s", m.View())
	}

	m = press(t, m, typed("n"))
	if m.confirm != nil || len(f.woken) != 0 || m.status != "wake cancelled" {
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

func TestDetailExpandsOneLevelAndNeverPrintsSecrets(t *testing.T) {
	d := device(1)
	f := &fake{
		devices: []store.Device{d},
		params: map[string]map[string]store.Param{d.ID: {
			"InternetGatewayDevice.DeviceInfo.Manufacturer":                    {Value: "Huawei"},
			"InternetGatewayDevice.DeviceInfo.SoftwareVersion":                 {Value: "V5R020C10S271"},
			"InternetGatewayDevice.ManagementServer.URL":                       {Value: "http://acs:7547/"},
			"InternetGatewayDevice.ManagementServer.ConnectionRequestPassword": {Secret: true, Value: "hunter2"},
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
	if !strings.Contains(view, "••••") {
		t.Fatalf("secret leaf not masked:\n%s", view)
	}
	if strings.Contains(view, "hunter2") {
		t.Fatalf("secret value leaked into the screen:\n%s", view)
	}

	m = press(t, m, keyEsc) // collapse ManagementServer.
	if m.det.expanded["InternetGatewayDevice.ManagementServer."] {
		t.Fatal("esc did not collapse the leaf's parent")
	}
	m = press(t, m, keyEsc) // collapse the root object: it is the last parent.
	if m.det.expanded["InternetGatewayDevice."] || len(m.det.rows) != 1 {
		t.Fatalf("esc did not collapse to the root level: %v", m.det.expanded)
	}
	m = press(t, m, keyEsc) // nothing left: back to the list.
	if m.screen != scrDevices {
		t.Fatalf("esc did not leave the detail: %v", m.screen)
	}
}

func TestParamValueMasksSecrets(t *testing.T) {
	if got := paramValue(store.Param{Secret: true, Value: "hunter2"}); got != "••••" {
		t.Fatalf("secret param rendered as %q", got)
	}
	if got := paramValue(store.Param{Value: "enc:abc"}); got != "••••" {
		t.Fatalf("sealed param rendered as %q", got)
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
