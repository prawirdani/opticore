// Package tui is the Opticore operator console: four keyboard-only screens
// over the local database. It speaks no CWMP and holds no SQL of its own.
// Every read goes through the interfaces in Deps, and the one write (a wake)
// goes through the caller's Wake func, so the daemon's validation and
// encryption stay the single path.
package tui

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/prawirdani/opticore/internal/store"
)

// Deps is what the console needs. Devices, Tasks, Jobs and Events are
// satisfied by *store.Store as written. Params needs a small adapter: the
// store has no prefix query, so keep the leaves of GetDevice's cache that
// start with the prefix. Wake is supplied by the caller, normally
// acs.Service.Wake, so a wake keeps its credential self-heal.
type Deps struct {
	Devices DeviceReader
	Tasks   TaskReader
	Jobs    JobReader
	Events  EventReader
	Params  ParamReader
	Wake    func(ctx context.Context, idOrSerial string) error
}

// DeviceReader lists devices newest-inform first and reads one by id or
// serial.
type DeviceReader interface {
	ListDevices(ctx context.Context, f store.ListFilter) ([]store.Device, error)
	GetDevice(ctx context.Context, idOrSerial string) (store.Device, error)
}

// TaskReader is a device's task log, newest first, optionally one status.
type TaskReader interface {
	ListTasksFiltered(ctx context.Context, deviceIDorSerial, status string, limit int) ([]store.Task, error)
}

// JobReader lists jobs and reads one job's progress and failures. JobCounts
// and JobFailures are per job; the store offers no bulk form.
type JobReader interface {
	ListJobs(ctx context.Context, limit int) ([]store.Job, error)
	GetJob(ctx context.Context, id int64) (store.Job, error)
	JobCounts(ctx context.Context, jobID int64) (map[string]int, error)
	JobFailures(ctx context.Context, jobID int64) ([]store.JobFailure, error)
}

// EventReader is a device's event log, newest first.
type EventReader interface {
	ListEvents(ctx context.Context, deviceIDorSerial string, limit int, before time.Time) ([]store.Event, error)
}

// ParamReader returns the cached parameter leaves under one prefix. The keys
// are full paths; the console groups them into one tree level at a time.
type ParamReader interface {
	Params(ctx context.Context, deviceIDorSerial, prefix string) (map[string]store.Param, error)
}

// Run starts the console on the alternate screen and returns when the
// operator quits. A query error becomes a status line inside the UI, never a
// return value; nil means a normal quit.
func Run(ctx context.Context, d Deps) error {
	m := newModel(ctx, d)
	_, err := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx)).Run()
	return err
}
