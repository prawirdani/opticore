// Package acs is the domain layer: what a CWMP session does with the task
// queue, and how tasks are created. It holds no provisioning logic (§7).
package acs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/prawirdani/opticore/internal/store"
	"github.com/prawirdani/opticore/internal/wake"
)

// encPrefix marks a task arg or prev value sealed with the secret key.
const encPrefix = "enc:"

// SetArgs is the args of a set task: [[path, value, type], ...].
type SetArgs struct {
	Values [][]string `json:"values"`
}

// GetArgs is the args of a get task. Paths ending in "." read a subtree.
type GetArgs struct {
	Paths []string `json:"paths"`
}

// ObjectArgs is the args of add_object and delete_object.
type ObjectArgs struct {
	Path string `json:"path"`
}

// Service creates tasks and wakes devices. It is shared by the CLI and,
// later, the HTTP API, so validation and encryption cannot diverge.
type Service struct {
	Store  *store.Store
	Key    []byte
	Waker  *wake.Waker // nil: never wake
	Logger interface {
		Warn(msg string, args ...any)
	}
}

// ErrReservedPath rejects client writes to connection-request credentials,
// which Opticore owns (§5).
var ErrReservedPath = errors.New("connection-request credentials are managed by the ACS")

// CreateTasks validates, seals secret values and stores tasks; more than one
// task forms an ordered batch. It does not wake the device.
func (svc *Service) CreateTasks(ctx context.Context, idOrSerial string, tasks []store.NewTask) ([]store.Task, error) {
	for i := range tasks {
		if err := svc.prepare(&tasks[i]); err != nil {
			return nil, fmt.Errorf("task %d (%s): %w", i+1, tasks[i].Name, err)
		}
	}
	return svc.Store.CreateTasks(ctx, idOrSerial, tasks)
}

// Enqueue stores tasks and wakes the device when anything is due now.
func (svc *Service) Enqueue(ctx context.Context, idOrSerial string, tasks []store.NewTask) ([]store.Task, error) {
	out, err := svc.CreateTasks(ctx, idOrSerial, tasks)
	if err != nil {
		return nil, err
	}
	due := false
	for _, t := range out {
		if !t.RunAt.After(time.Now()) {
			due = true
		}
	}
	if due && len(out) > 0 && svc.Waker != nil {
		// Coalesced fire-and-forget wake: many tasks queued in a burst share one
		// connection request (§5). Failures are logged by the waker.
		if t, err := svc.wakeTarget(ctx, out[0].DeviceID); err != nil {
			svc.warn("cannot wake after enqueue; tasks run at next inform", out[0].DeviceID, err)
		} else {
			svc.Waker.Request(t)
		}
	}
	return out, nil
}

func (svc *Service) prepare(t *store.NewTask) error {
	switch t.Name {
	case "get":
		var a GetArgs
		if err := json.Unmarshal(t.Args, &a); err != nil || len(a.Paths) == 0 {
			return errors.New(`args must be {"paths": [...]} with at least one path`)
		}
	case "set":
		var a SetArgs
		if err := json.Unmarshal(t.Args, &a); err != nil || len(a.Values) == 0 {
			return errors.New(`args must be {"values": [[path, value, type], ...]}`)
		}
		for i, v := range a.Values {
			if len(v) < 2 || len(v) > 3 || v[0] == "" || strings.HasSuffix(v[0], ".") {
				return fmt.Errorf("value %d: want [path, value] or [path, value, type] on a leaf", i+1)
			}
			if isCRPath(v[0]) {
				return ErrReservedPath
			}
			if len(v) == 2 {
				a.Values[i] = append(v, "xsd:string")
			}
			if store.IsSecretPath(v[0]) && !strings.HasPrefix(v[1], encPrefix) {
				sealed, err := store.SealSecret(svc.Key, []byte(v[1]))
				if err != nil {
					return err
				}
				a.Values[i][1] = encPrefix + sealed
			}
		}
		b, err := json.Marshal(a)
		if err != nil {
			return err
		}
		t.Args = b
	case "add_object", "delete_object":
		var a ObjectArgs
		if err := json.Unmarshal(t.Args, &a); err != nil || !strings.HasSuffix(a.Path, ".") {
			return errors.New(`args must be {"path": "Object."} ending in "."`)
		}
	case "reboot", "reset":
		if len(t.Args) == 0 {
			t.Args = json.RawMessage(`{}`)
		}
	default:
		return fmt.Errorf("task %q is not supported yet", t.Name)
	}
	return nil
}

// Wake sends a connection request now, using the credentials Opticore set.
func (svc *Service) Wake(ctx context.Context, idOrSerial string) error {
	if svc.Waker == nil {
		return nil
	}
	t, err := svc.wakeTarget(ctx, idOrSerial)
	if err != nil {
		return err
	}
	err = svc.Waker.Wake(ctx, t)
	if errors.Is(err, wake.ErrUnauthorized) {
		// The device refused the credentials we believe it holds (it was
		// factory reset, or a reboot restored older settings). Forget them so
		// the next Inform sets them again.
		if cerr := svc.Store.ClearCRCredentials(ctx, t.DeviceID); cerr == nil {
			svc.warn("wake refused: connection-request credentials will be re-set on the next inform", t.DeviceID, err)
		}
	}
	return err
}

func (svc *Service) wakeTarget(ctx context.Context, idOrSerial string) (wake.Target, error) {
	d, err := svc.Store.GetDevice(ctx, idOrSerial)
	if err != nil {
		return wake.Target{}, err
	}
	if d.CRURL == "" {
		return wake.Target{}, errors.New("device has not reported a ConnectionRequestURL")
	}
	t := wake.Target{DeviceID: d.ID, URL: d.CRURL, Username: d.CRUsername}
	if d.CRPassword != "" {
		p, err := store.OpenSecret(svc.Key, d.CRPassword)
		if err != nil {
			return wake.Target{}, fmt.Errorf("decrypt connection-request password: %w", err)
		}
		t.Password = string(p)
	}
	return t, nil
}

func (svc *Service) warn(msg, deviceID string, err error) {
	if svc.Logger != nil {
		svc.Logger.Warn(msg, "device_id", deviceID, "err", err)
	}
}

func isCRPath(path string) bool {
	return strings.HasSuffix(path, ".ManagementServer.ConnectionRequestUsername") ||
		strings.HasSuffix(path, ".ManagementServer.ConnectionRequestPassword")
}
