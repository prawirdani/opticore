package client

import (
	"encoding/json"
	"fmt"
	"time"
)

// Param is one parameter value. Path is set only by ListParams. The remaining
// fields are shared with the Inform hook's parameter payload.
type Param struct {
	Path      string     `json:"path,omitempty"`
	Value     string     `json:"value"`
	Type      string     `json:"type,omitempty"`
	Writable  *bool      `json:"writable,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// Device is both an API device row and the identity carried in a hook Step.
//
// The hook sends only ID, Serial, OUI, ProductClass and SoftwareVersion. The
// API adds the rest; LastInform is a pointer because the hook omits it and a
// zero time.Time would otherwise marshal.
type Device struct {
	ID              string `json:"id"`
	Serial          string `json:"serial"`
	OUI             string `json:"oui"`
	ProductClass    string `json:"product_class"`
	SoftwareVersion string `json:"software_version"`

	LastInform    *time.Time       `json:"last_inform,omitempty"`
	LastAddr      string           `json:"last_addr,omitempty"`
	CRURL         string           `json:"cr_url,omitempty"`
	CRReady       bool             `json:"cr_ready,omitempty"`
	PendingReboot bool             `json:"pending_reboot,omitempty"`
	LastFault     json.RawMessage  `json:"last_fault,omitempty"`
	Freshness     string           `json:"freshness,omitempty"`
	ParamsCount   int              `json:"params_count,omitempty"`
	Params        map[string]Param `json:"params,omitempty"`
	Tasks         map[string]int   `json:"tasks,omitempty"`
}

// Task is one CWMP task, used both as a request (the typed builders return
// this) and as a stored task row. Fields the server only returns are
// omitempty, so a builder or hook reply marshals the same wire shape as
// before.
type Task struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`

	ID           int64           `json:"id,omitempty"`
	Prev         json.RawMessage `json:"prev,omitempty"`
	Result       json.RawMessage `json:"result,omitempty"`
	Status       string          `json:"status,omitempty"`
	Attempts     int             `json:"attempts,omitempty"`
	FaultCode    *int            `json:"fault_code,omitempty"`
	FaultMessage string          `json:"fault,omitempty"`
	CreatedAt    *time.Time      `json:"created_at,omitempty"`
	RunAt        *time.Time      `json:"run_at,omitempty"`
	ExpiresAt    *time.Time      `json:"expires_at,omitempty"`
	UpdatedAt    *time.Time      `json:"updated_at,omitempty"`
	BatchID      *string         `json:"batch_id,omitempty"`
	BatchSeq     int             `json:"batch_seq,omitempty"`
}

// Fault is a CWMP fault reported by a task. It implements error, so callers
// can pass Task.Fault() to errors.As.
type Fault struct {
	Code   int
	String string
}

func (f *Fault) Error() string {
	if f.String == "" {
		return fmt.Sprintf("CWMP fault %d", f.Code)
	}
	return fmt.Sprintf("CWMP fault %d: %s", f.Code, f.String)
}

// Fault returns the task's CWMP fault, or nil when it recorded none.
func (t *Task) Fault() *Fault {
	if t == nil || (t.FaultCode == nil && t.FaultMessage == "") {
		return nil
	}
	f := &Fault{String: t.FaultMessage}
	if t.FaultCode != nil {
		f.Code = *t.FaultCode
	}
	return f
}

// EventRecord is one row of GET /v1/devices/{id}/events.
type EventRecord struct {
	ID       int64           `json:"id"`
	DeviceID string          `json:"device_id"`
	At       time.Time       `json:"at"`
	Type     string          `json:"type"`
	Detail   json.RawMessage `json:"detail"`
}

// JobSelector chooses the devices a job expands to.
type JobSelector struct {
	Serials         []string `json:"serials,omitempty"`
	IDs             []string `json:"ids,omitempty"`
	ProductClass    string   `json:"product_class,omitempty"`
	SoftwareVersion string   `json:"software_version,omitempty"`
}

// JobSpec is one POST /v1/jobs request. Task is copied to every selected
// device.
type JobSpec struct {
	Name        string      `json:"name"`
	Task        Task        `json:"task"`
	Select      JobSelector `json:"select"`
	MaxParallel int         `json:"max_parallel,omitempty"`
	RunAt       *time.Time  `json:"run_at,omitempty"`
}

// JobFailure is one failed task of a job.
type JobFailure struct {
	DeviceID  string `json:"device_id"`
	TaskID    int64  `json:"task_id"`
	FaultCode *int   `json:"fault_code"`
	Fault     string `json:"fault"`
}

// Job is the job snapshot returned by the API and streamed as a Watch event.
type Job struct {
	ID          int64          `json:"id"`
	Name        string         `json:"name"`
	Status      string         `json:"status"`
	MaxParallel int            `json:"max_parallel"`
	CreatedAt   time.Time      `json:"created_at"`
	Counts      map[string]int `json:"counts"`
	Failures    []JobFailure   `json:"failures,omitempty"`
}
