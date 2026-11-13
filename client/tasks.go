package client

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// Value is one set argument. Type defaults to xsd:string when empty.
type Value struct {
	Path  string
	Value string
	Type  string
}

// Get builds a get task. With no paths it reads every parameter under the
// device root.
func Get(paths ...string) Task {
	if len(paths) == 0 {
		return Task{Name: "get", Args: json.RawMessage(`{}`)}
	}
	return Task{Name: "get", Args: jsonArgs(struct {
		Paths []string `json:"paths"`
	}{paths})}
}

// Set builds a set task. Each path must be a leaf.
func Set(values ...Value) Task {
	rows := make([][]string, len(values))
	for i, v := range values {
		typ := v.Type
		if typ == "" {
			typ = "xsd:string"
		}
		rows[i] = []string{v.Path, v.Value, typ}
	}
	return Task{Name: "set", Args: jsonArgs(struct {
		Values [][]string `json:"values"`
	}{rows})}
}

// AddObject builds an add_object task. path must end in ".".
func AddObject(path string) Task {
	return Task{Name: "add_object", Args: jsonArgs(struct {
		Path string `json:"path"`
	}{path})}
}

// DeleteObject builds a delete_object task. path must end in ".".
func DeleteObject(path string) Task {
	return Task{Name: "delete_object", Args: jsonArgs(struct {
		Path string `json:"path"`
	}{path})}
}

// Reboot builds a reboot task.
func Reboot() Task { return Task{Name: "reboot", Args: json.RawMessage(`{}`)} }

// Reset builds a factory reset task.
func Reset() Task { return Task{Name: "reset", Args: json.RawMessage(`{}`)} }

// PingArgs is the args of a ping task. Only Host is required; the server fills
// the defaults for zero values.
type PingArgs struct {
	Host      string `json:"host"`
	Count     int    `json:"count,omitempty"`
	TimeoutMS int    `json:"timeout_ms,omitempty"`
	Size      int    `json:"size,omitempty"`
	Interface string `json:"interface,omitempty"`
}

// PingOption tunes a ping task.
type PingOption func(*PingArgs)

// PingCount sets the number of echo requests.
func PingCount(n int) PingOption { return func(a *PingArgs) { a.Count = n } }

// PingTimeout sets the per-request timeout.
func PingTimeout(d time.Duration) PingOption {
	return func(a *PingArgs) { a.TimeoutMS = int(d / time.Millisecond) }
}

// PingSize sets the payload size in bytes.
func PingSize(n int) PingOption { return func(a *PingArgs) { a.Size = n } }

// PingInterface sets the device interface name.
func PingInterface(name string) PingOption { return func(a *PingArgs) { a.Interface = name } }

// Ping builds a ping task.
func Ping(host string, opts ...PingOption) Task {
	a := PingArgs{Host: host}
	for _, o := range opts {
		o(&a)
	}
	return Task{Name: "ping", Args: jsonArgs(a)}
}

// CreateTasks enqueues one task or an ordered batch for a device. An empty
// idemKey omits the Idempotency-Key header.
func (c *Client) CreateTasks(ctx context.Context, deviceID string, tasks []Task, idemKey string) ([]Task, error) {
	var out struct {
		Data []Task `json:"data"`
	}
	if err := c.do(ctx, http.MethodPost, []string{"devices", deviceID, "tasks"}, nil,
		idempotency(idemKey), tasks, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

// GetTask returns one task by id.
func (c *Client) GetTask(ctx context.Context, taskID int64) (*Task, error) {
	var out struct {
		Data Task `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, []string{"tasks", strconv.FormatInt(taskID, 10)},
		nil, nil, nil, &out); err != nil {
		return nil, err
	}
	return &out.Data, nil
}

// CancelTask cancels a pending task, or the rest of its batch.
func (c *Client) CancelTask(ctx context.Context, deviceID string, taskID int64) error {
	return c.do(ctx, http.MethodDelete,
		[]string{"devices", deviceID, "tasks", strconv.FormatInt(taskID, 10)}, nil, nil, nil, nil)
}

// Wake sends a connection request to a device now.
func (c *Client) Wake(ctx context.Context, deviceID string) error {
	return c.do(ctx, http.MethodPost, []string{"devices", deviceID, "wake"}, nil, nil, nil, nil)
}

// Summon is GenieACS's Summon in one call: it queues a whole-tree get, which
// also sends the connection request, so the device reports every parameter in
// the session that follows. It returns the queued task.
func (c *Client) Summon(ctx context.Context, deviceID string) ([]Task, error) {
	return c.CreateTasks(ctx, deviceID, []Task{Get()}, "")
}

// RevertTask queues a revert set built from a done set task's recorded prev.
func (c *Client) RevertTask(ctx context.Context, deviceID string, taskID int64, idemKey string) ([]Task, error) {
	var out struct {
		Data []Task `json:"data"`
	}
	if err := c.do(ctx, http.MethodPost,
		[]string{"devices", deviceID, "tasks", strconv.FormatInt(taskID, 10), "revert"},
		nil, idempotency(idemKey), nil, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

func idempotency(key string) map[string]string {
	if key == "" {
		return nil
	}
	return map[string]string{"Idempotency-Key": key}
}

func jsonArgs(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		// Only structs of strings and ints reach here, which cannot fail.
		return nil
	}
	return b
}
