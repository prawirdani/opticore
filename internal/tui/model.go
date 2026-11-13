package tui

import (
	"context"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/prawirdani/opticore/internal/store"
)

type screen int

const (
	scrDevices screen = iota
	scrDevice
	scrTasks
	scrJobs
	scrJob
)

// limits keep one refresh bounded; the console lists, it does not page.
const (
	deviceLimit = 200
	taskLimit   = 200
	eventLimit  = 10
	jobLimit    = 50
	detailTaskN = 20
)

// confirmState is a pending "are you sure" for the one write the console has.
type confirmState struct {
	id    string
	label string
}

type model struct {
	ctx  context.Context
	deps Deps
	now  func() time.Time

	w, h   int
	screen screen

	dev  devicesPane
	det  detailPane
	task tasksPane
	jobs jobsPane
	jobd jobPane

	status    string
	statusBad bool
	loading   string
	confirm   *confirmState
}

func newModel(ctx context.Context, d Deps) model {
	m := model{ctx: ctx, deps: d, now: time.Now, screen: scrDevices}
	m.loading = "loading devices…"
	return m
}

func (m model) Init() tea.Cmd {
	return loadDevices(m.ctx, m.deps.Devices)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		return m, nil
	case tea.KeyMsg:
		return m.key(msg)
	case devicesMsg:
		m.loading = ""
		if msg.err != nil {
			m.fail("devices", msg.err)
			return m, nil
		}
		m.dev.set(msg.rows, m.now())
		return m, nil
	case tasksMsg:
		m.loading = ""
		if msg.err != nil {
			m.fail("tasks", msg.err)
			return m, nil
		}
		if msg.device != m.task.deviceID {
			return m, nil
		}
		m.task.set(msg.rows, m.now())
		return m, nil
	case detailMsg:
		m.loading = ""
		if msg.device.ID != "" && m.det.device.ID != "" && msg.device.ID != m.det.device.ID {
			return m, nil
		}
		if msg.err != nil {
			m.fail("device", msg.err)
		}
		if msg.device.ID != "" {
			m.det.device = msg.device
		}
		m.det.tasks = msg.tasks
		m.det.events = msg.events
		if msg.params != nil {
			m.det.pages[""] = buildPage(msg.params, "")
			m.det.rebuild()
		}
		m.det.at = m.now()
		return m, nil
	case paramsMsg:
		m.loading = ""
		if msg.device != m.det.device.ID {
			return m, nil
		}
		if msg.err != nil {
			m.fail("parameters", msg.err)
			delete(m.det.expanded, msg.prefix)
			m.det.rebuild()
			return m, nil
		}
		m.det.pages[msg.prefix] = buildPage(msg.params, msg.prefix)
		m.det.rebuild()
		if row, ok := m.det.selectedRow(); ok && row.prefix == msg.prefix {
			m.det.selectChild()
		}
		return m, nil
	case jobsMsg:
		m.loading = ""
		if msg.err != nil {
			m.fail("jobs", msg.err)
			return m, nil
		}
		m.jobs.set(msg.jobs, msg.counts, m.now())
		return m, nil
	case jobMsg:
		m.loading = ""
		if msg.id != m.jobd.job.ID {
			return m, nil
		}
		m.jobd.job = msg.job
		m.jobd.counts = msg.counts
		m.jobd.failures = msg.failures
		m.jobd.cur.clamp(len(msg.failures))
		m.jobd.loaded = true
		m.jobd.at = m.now()
		if msg.err != nil {
			m.fail(fmt.Sprintf("job %d", msg.id), msg.err)
		}
		return m, nil
	case wakeMsg:
		if msg.err != nil {
			m.status, m.statusBad = "ERR: wake failed: "+msg.err.Error(), true
		} else {
			m.status, m.statusBad = "wake sent to "+msg.label, false
		}
		return m, nil
	}
	return m, nil
}

func (m *model) fail(what string, err error) {
	m.status, m.statusBad = "ERR: "+what+": "+err.Error(), true
}

func (m *model) say(s string) {
	m.status, m.statusBad = s, false
}

func (m model) key(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if k.Type == tea.KeyCtrlC {
		return m, tea.Quit
	}
	if m.confirm != nil {
		return m.confirmKey(k)
	}
	if m.editing() {
		return m.filterKey(k)
	}
	switch k.String() {
	case "q":
		return m, tea.Quit
	case "1":
		return m, m.switchTo(scrDevices)
	case "2":
		return m, m.switchTo(scrTasks)
	case "3":
		return m, m.switchTo(scrJobs)
	case "tab":
		return m, m.switchTo(m.nextTab(1))
	case "shift+tab":
		return m, m.switchTo(m.nextTab(-1))
	case "r":
		m.status, m.statusBad = "", false
		return m, m.refresh()
	case "/":
		if f := m.activeFilter(); f != nil {
			f.begin()
		}
		return m, nil
	case "s":
		if m.screen == scrTasks {
			m.task.cycleStatus()
			m.task.refilter()
		}
		return m, nil
	case "w":
		m.beginWake()
		return m, nil
	case "up", "k":
		m.moveCursor(-1)
		return m, nil
	case "down", "j":
		m.moveCursor(1)
		return m, nil
	case "home", "g":
		m.moveCursor(-1 << 20)
		return m, nil
	case "end", "G":
		m.moveCursor(1 << 20)
		return m, nil
	case "enter":
		return m, m.enter()
	case "esc":
		return m, m.back()
	}
	return m, nil
}

func (m model) confirmKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	c := m.confirm
	switch k.String() {
	case "y", "Y", "enter":
		m.confirm = nil
		wake := m.deps.Wake
		if wake == nil {
			m.say("wake is not configured")
			return m, nil
		}
		ctx := m.ctx
		return m, func() tea.Msg {
			return wakeMsg{id: c.id, label: c.label, err: wake(ctx, c.id)}
		}
	case "n", "N", "esc", "q":
		m.confirm = nil
		m.say("wake cancelled")
	}
	return m, nil
}

func (m *model) editing() bool {
	if f := m.activeFilter(); f != nil {
		return f.editing
	}
	return false
}

func (m *model) activeFilter() *filter {
	switch m.screen {
	case scrDevices:
		return &m.dev.filter
	case scrTasks:
		return &m.task.filter
	case scrJobs:
		return &m.jobs.filter
	}
	return nil
}

// refilterRows reapplies the active filter after its text changes.
func (m *model) refilterRows() {
	switch m.screen {
	case scrDevices:
		m.dev.refilter(m.now())
	case scrTasks:
		m.task.refilter()
	case scrJobs:
		m.jobs.refilter()
	}
}

func (m model) filterKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	f := m.activeFilter()
	if f == nil {
		return m, nil
	}
	switch k.Type {
	case tea.KeyEsc:
		f.cancel()
	case tea.KeyEnter:
		f.commit()
		return m, nil
	case tea.KeyBackspace:
		if r := []rune(f.text); len(r) > 0 {
			f.text = string(r[:len(r)-1])
		}
	case tea.KeyCtrlU:
		f.text = ""
	case tea.KeyRunes:
		f.text += string(k.Runes)
	default:
		return m, nil
	}
	m.refilterRows()
	return m, nil
}

func (m *model) moveCursor(delta int) {
	switch m.screen {
	case scrDevices:
		m.dev.cur.move(delta, len(m.dev.view))
	case scrTasks:
		m.task.cur.move(delta, len(m.task.view))
	case scrJobs:
		m.jobs.cur.move(delta, len(m.jobs.view))
	case scrDevice:
		m.det.cur.move(delta, len(m.det.rows))
	case scrJob:
		m.jobd.cur.move(delta, len(m.jobd.failures))
	}
}

func (m *model) enter() tea.Cmd {
	switch m.screen {
	case scrDevices:
		return m.openDevice()
	case scrJobs:
		return m.openJob()
	case scrDevice:
		return m.detailToggle()
	}
	return nil
}

var tabs = []screen{scrDevices, scrTasks, scrJobs}

func (m model) nextTab(dir int) screen {
	cur := 0
	switch m.screen {
	case scrTasks:
		cur = 1
	case scrJobs, scrJob:
		cur = 2
	}
	cur = (cur + dir + len(tabs)) % len(tabs)
	return tabs[cur]
}

// switchTo moves between the three tabs. The Tasks tab belongs to a device,
// so it adopts whichever device the operator has selected and loads only when
// that device changed.
func (m *model) switchTo(s screen) tea.Cmd {
	if s == scrTasks {
		id, label, ok := m.selectedDevice()
		m.screen = scrTasks
		if !ok || m.task.deviceID == id {
			return nil
		}
		m.task = tasksPane{deviceID: id, deviceLabel: label}
		m.loading = "loading tasks…"
		return loadTasks(m.ctx, m.deps.Tasks, id)
	}
	m.screen = s
	if s == scrJobs && !m.jobs.loaded {
		m.loading = "loading jobs…"
		return loadJobs(m.ctx, m.deps.Jobs)
	}
	return nil
}

// selectedDevice is the device the operator is looking at: the detail's
// device, or the highlighted row of the devices list.
func (m *model) selectedDevice() (id, label string, ok bool) {
	if m.screen == scrDevice && m.det.device.ID != "" {
		return m.det.device.ID, m.det.device.Serial, true
	}
	if d, ok := m.dev.selected(); ok {
		return d.ID, d.Serial, true
	}
	return "", "", false
}

func (m *model) refresh() tea.Cmd {
	switch m.screen {
	case scrDevices:
		m.loading = "loading devices…"
		return loadDevices(m.ctx, m.deps.Devices)
	case scrTasks:
		if m.task.deviceID == "" {
			m.task.loaded = true
			return nil
		}
		m.loading = "loading tasks for " + m.task.deviceLabel + "…"
		return loadTasks(m.ctx, m.deps.Tasks, m.task.deviceID)
	case scrJobs:
		m.loading = "loading jobs…"
		return loadJobs(m.ctx, m.deps.Jobs)
	case scrDevice:
		if m.det.device.ID == "" {
			return nil
		}
		m.loading = "loading device…"
		m.det.resetTree()
		return loadDetail(m.ctx, m.deps, m.det.device.ID)
	case scrJob:
		if m.jobd.job.ID == 0 {
			return nil
		}
		m.loading = fmt.Sprintf("loading job %d…", m.jobd.job.ID)
		return loadJob(m.ctx, m.deps.Jobs, m.jobd.job.ID)
	}
	return nil
}

func (m *model) openDevice() tea.Cmd {
	d, ok := m.dev.selected()
	if !ok {
		return nil
	}
	m.det = newDetailPane(d)
	m.screen = scrDevice
	m.status, m.statusBad = "", false
	m.loading = "loading device…"
	return loadDetail(m.ctx, m.deps, d.ID)
}

func (m *model) openJob() tea.Cmd {
	j, ok := m.jobs.selected()
	if !ok {
		return nil
	}
	m.jobd = jobPane{job: j}
	m.screen = scrJob
	m.loading = fmt.Sprintf("loading job %d…", j.ID)
	return loadJob(m.ctx, m.deps.Jobs, j.ID)
}

func (m *model) detailToggle() tea.Cmd {
	row, ok := m.det.selectedRow()
	if !ok || !row.object {
		return nil
	}
	if m.det.expanded[row.prefix] {
		delete(m.det.expanded, row.prefix)
		m.det.rebuild()
		return nil
	}
	m.det.expanded[row.prefix] = true
	if _, ok := m.det.pages[row.prefix]; ok {
		m.det.rebuild()
		m.det.selectChild()
		return nil
	}
	m.loading = "loading parameters…"
	return loadParams(m.ctx, m.deps.Params, m.det.device.ID, row.prefix)
}

func (m *model) beginWake() {
	var id, label string
	switch m.screen {
	case scrDevice:
		id, label = m.det.device.ID, m.det.device.Serial
	case scrDevices:
		if d, ok := m.dev.selected(); ok {
			id, label = d.ID, d.Serial
		}
	}
	if id == "" {
		m.say("no device selected")
		return
	}
	if m.deps.Wake == nil {
		m.say("wake is not configured")
		return
	}
	m.confirm = &confirmState{id: id, label: label}
}

// back implements Esc: collapse a level, then leave the drill-in screen, then
// clear the filter, then return to the devices tab.
func (m *model) back() tea.Cmd {
	switch m.screen {
	case scrDevice:
		if m.det.collapse() {
			return nil
		}
		m.screen = scrDevices
		return nil
	case scrJob:
		m.screen = scrJobs
		return nil
	}
	if f := m.activeFilter(); f != nil && f.text != "" {
		f.text = ""
		m.refilterRows()
		return nil
	}
	if m.screen != scrDevices {
		m.screen = scrDevices
	}
	return nil
}

type devicesMsg struct {
	rows []store.Device
	err  error
}

type tasksMsg struct {
	device string
	rows   []store.Task
	err    error
}

type detailMsg struct {
	device store.Device
	params map[string]store.Param
	tasks  []store.Task
	events []store.Event
	err    error
}

type paramsMsg struct {
	device string
	prefix string
	params map[string]store.Param
	err    error
}

type jobsMsg struct {
	jobs   []store.Job
	counts map[int64]map[string]int
	err    error
}

type jobMsg struct {
	id       int64
	job      store.Job
	counts   map[string]int
	failures []store.JobFailure
	err      error
}

type wakeMsg struct {
	id    string
	label string
	err   error
}

func loadDevices(ctx context.Context, r DeviceReader) tea.Cmd {
	return func() tea.Msg {
		rows, err := r.ListDevices(ctx, store.ListFilter{Limit: deviceLimit})
		return devicesMsg{rows: rows, err: err}
	}
}

func loadTasks(ctx context.Context, r TaskReader, id string) tea.Cmd {
	return func() tea.Msg {
		rows, err := r.ListTasksFiltered(ctx, id, "", taskLimit)
		return tasksMsg{device: id, rows: rows, err: err}
	}
}

func loadDetail(ctx context.Context, d Deps, id string) tea.Cmd {
	return func() tea.Msg {
		dev, err := d.Devices.GetDevice(ctx, id)
		if err != nil {
			return detailMsg{err: err}
		}
		msg := detailMsg{device: dev}
		params, err := d.Params.Params(ctx, dev.ID, "")
		if err != nil {
			msg.err = err
		} else {
			msg.params = params
		}
		tasks, err := d.Tasks.ListTasksFiltered(ctx, dev.ID, "", detailTaskN)
		if err != nil && msg.err == nil {
			msg.err = err
		} else {
			msg.tasks = tasks
		}
		events, err := d.Events.ListEvents(ctx, dev.ID, eventLimit, time.Time{})
		if err != nil && msg.err == nil {
			msg.err = err
		} else {
			msg.events = events
		}
		return msg
	}
}

func loadParams(ctx context.Context, r ParamReader, id, prefix string) tea.Cmd {
	return func() tea.Msg {
		params, err := r.Params(ctx, id, prefix)
		return paramsMsg{device: id, prefix: prefix, params: params, err: err}
	}
}

func loadJobs(ctx context.Context, r JobReader) tea.Cmd {
	return func() tea.Msg {
		jobs, err := r.ListJobs(ctx, jobLimit)
		if err != nil {
			return jobsMsg{err: err}
		}
		counts := make(map[int64]map[string]int, len(jobs))
		for _, j := range jobs {
			// shortcut: one JobCounts call per listed job; the store has no
			// bulk count, revisit if the list is ever paged deeper than this.
			c, err := r.JobCounts(ctx, j.ID)
			if err != nil {
				return jobsMsg{err: err}
			}
			counts[j.ID] = c
		}
		return jobsMsg{jobs: jobs, counts: counts}
	}
}

func loadJob(ctx context.Context, r JobReader, id int64) tea.Cmd {
	return func() tea.Msg {
		j, err := r.GetJob(ctx, id)
		if err != nil {
			return jobMsg{id: id, err: err}
		}
		counts, err := r.JobCounts(ctx, id)
		if err != nil {
			return jobMsg{id: id, job: j, err: err}
		}
		failures, err := r.JobFailures(ctx, id)
		if err != nil {
			return jobMsg{id: id, job: j, counts: counts, err: err}
		}
		return jobMsg{id: id, job: j, counts: counts, failures: failures}
	}
}
