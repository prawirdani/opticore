package acs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/prawirdani/opticore/internal/store"
)

var (
	// ErrNotASetTask reports a revert of a task that is not a set.
	ErrNotASetTask = errors.New("only set tasks can be reverted")
	// ErrTaskNotDone reports a revert of a task that did not finish.
	ErrTaskNotDone = errors.New("only a done task can be reverted")
	// ErrNoPrev reports a set task with no recorded previous values.
	ErrNoPrev = errors.New("task has no recorded previous values")
	// ErrNothingToRevert reports a job with no done set task that recorded prev.
	ErrNothingToRevert = errors.New("job has no done set tasks with previous values")
)

// RevertTask queues one ordinary set task carrying the values the original
// set overwrote. Sealed prev values are decrypted here and sealed again by
// Enqueue, so a revert never stores plaintext secrets (§8).
func (svc *Service) RevertTask(ctx context.Context, idOrSerial string, taskID int64, idemKey *string) ([]store.Task, error) {
	d, err := svc.Store.GetDevice(ctx, idOrSerial)
	if err != nil {
		return nil, err
	}
	t, err := svc.Store.GetTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if t.DeviceID != d.ID {
		return nil, store.ErrTaskNotFound
	}
	if t.Name != "set" {
		return nil, ErrNotASetTask
	}
	if t.Status != "done" {
		return nil, ErrTaskNotDone
	}
	args, err := revertArgs(svc.Key, t.Prev)
	if err != nil {
		return nil, err
	}
	return svc.Enqueue(ctx, d.ID, []store.NewTask{{Name: "set", Args: args, IdemKey: idemKey}})
}

// RevertJob creates one revert set per done set task of jobID, inside a new
// job that keeps the original max_parallel cap so a mass revert does not wake
// the whole fleet at once.
func (svc *Service) RevertJob(ctx context.Context, jobID int64) (store.Job, []store.Task, error) {
	job, err := svc.Store.GetJob(ctx, jobID)
	if err != nil {
		return store.Job{}, nil, err
	}
	tasks, err := svc.Store.RevertableJobTasks(ctx, jobID)
	if err != nil {
		return store.Job{}, nil, err
	}
	if len(tasks) == 0 {
		return store.Job{}, nil, ErrNothingToRevert
	}

	perDevice := map[string][]store.NewTask{}
	ids := make([]string, 0, len(tasks))
	seen := map[string]bool{}
	for _, t := range tasks {
		args, err := revertArgs(svc.Key, t.Prev)
		if err != nil {
			return store.Job{}, nil, fmt.Errorf("task %d: %w", t.ID, err)
		}
		nt := store.NewTask{Name: "set", Args: args}
		if err := svc.prepare(&nt); err != nil {
			return store.Job{}, nil, fmt.Errorf("task %d: %w", t.ID, err)
		}
		if !seen[t.DeviceID] {
			seen[t.DeviceID] = true
			ids = append(ids, t.DeviceID)
		}
		perDevice[t.DeviceID] = append(perDevice[t.DeviceID], nt)
	}

	spec, err := json.Marshal(map[string]int64{"revert_of": jobID})
	if err != nil {
		return store.Job{}, nil, err
	}
	return svc.Store.CreateJob(ctx, store.NewJob{
		Name:        fmt.Sprintf("revert of job %d", jobID),
		Spec:        spec,
		Selector:    store.JobSelector{IDs: ids},
		Task:        store.NewTask{Name: "set"},
		PerDevice:   perDevice,
		MaxParallel: job.MaxParallel,
	})
}

// revertArgs turns a stored prev map back into set args, decrypting sealed
// secret values so Enqueue can seal them with a fresh nonce.
func revertArgs(key []byte, prev json.RawMessage) (json.RawMessage, error) {
	if len(prev) == 0 {
		return nil, ErrNoPrev
	}
	var m map[string][2]string
	if err := json.Unmarshal(prev, &m); err != nil {
		return nil, fmt.Errorf("decode prev: %w", err)
	}
	if len(m) == 0 {
		return nil, ErrNoPrev
	}
	paths := make([]string, 0, len(m))
	for p := range m {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	values := make([][]string, 0, len(paths))
	for _, p := range paths {
		v, typ := m[p][0], m[p][1]
		if strings.HasPrefix(v, encPrefix) {
			plain, err := store.OpenSecret(key, strings.TrimPrefix(v, encPrefix))
			if err != nil {
				return nil, fmt.Errorf("decrypt %s: %w", p, err)
			}
			v = string(plain)
		}
		if typ == "" {
			typ = "xsd:string"
		}
		values = append(values, []string{p, v, typ})
	}
	return json.Marshal(SetArgs{Values: values})
}
