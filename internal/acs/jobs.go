package acs

import (
	"context"
	"fmt"
	"time"

	"github.com/prawirdani/opticore/internal/store"
)

// CreateJob validates and seals the task template, then expands the selector
// into one task per device. It does not wake anyone; call PumpJobs for that.
func (svc *Service) CreateJob(ctx context.Context, j store.NewJob) (store.Job, []store.Task, error) {
	if err := svc.prepare(&j.Task); err != nil {
		return store.Job{}, nil, fmt.Errorf("task (%s): %w", j.Task.Name, err)
	}
	return svc.Store.CreateJob(ctx, j)
}

// PumpJobs wakes due tasks of running jobs up to each job's max_parallel.
//
// The cap is approximate: it counts tasks in status running plus tasks this
// process woke recently (until PumpWindow passes). Without the memory, every
// pump tick would wake another max_parallel devices before the first batch
// has even started.
//
// shortcut: one daemon only (the leader lock enforces it), and the woken set
// is memory-only; after a restart the pump may wake up to max_parallel more
// devices. Upgrade if task starts become slower than the pump interval.
func (svc *Service) PumpJobs(ctx context.Context) error {
	if svc.Store == nil {
		return nil
	}
	if _, err := svc.Store.FinishJobs(ctx); err != nil {
		return err
	}

	svc.pumpMu.Lock()
	defer svc.pumpMu.Unlock()
	if svc.Waker == nil {
		return nil
	}
	window := svc.PumpWindow
	if window <= 0 {
		window = 5 * time.Minute
	}
	now := time.Now()
	if svc.woken == nil {
		svc.woken = map[int64]map[int64]time.Time{}
	}
	for jobID, tasks := range svc.woken {
		for taskID, at := range tasks {
			if now.Sub(at) >= window {
				delete(tasks, taskID)
			}
		}
		if len(tasks) == 0 {
			delete(svc.woken, jobID)
		}
	}

	jobs, err := svc.Store.RunningJobs(ctx)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		running, err := svc.Store.JobRunningCount(ctx, j.ID)
		if err != nil {
			return err
		}
		allowed := j.MaxParallel - running - len(svc.woken[j.ID])
		if allowed <= 0 {
			continue
		}
		exclude := make([]int64, 0, len(svc.woken[j.ID]))
		for id := range svc.woken[j.ID] {
			exclude = append(exclude, id)
		}
		tasks, err := svc.Store.DueJobTasks(ctx, j.ID, allowed, exclude)
		if err != nil {
			return err
		}
		for _, t := range tasks {
			if svc.woken[j.ID] == nil {
				svc.woken[j.ID] = map[int64]time.Time{}
			}
			svc.woken[j.ID][t.ID] = now
			target, err := svc.wakeTarget(ctx, t.DeviceID)
			if err != nil {
				svc.warn("job pump: cannot wake; task runs at next inform", t.DeviceID, err)
				continue
			}
			svc.Waker.Request(target)
		}
	}
	return nil
}
