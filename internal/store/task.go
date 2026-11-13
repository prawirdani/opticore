package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

var taskNames = map[string]bool{
	"get": true, "set": true, "add_object": true, "delete_object": true,
	"reboot": true, "reset": true, "download": true, "ping": true, "revert": true,
}

var taskStatuses = map[string]bool{
	"pending": true, "running": true, "done": true, "failed": true,
	"unknown": true, "cancelled": true, "expired": true,
}

// NewTask is one task to enqueue.
type NewTask struct {
	Name string
	Args json.RawMessage
	Prev json.RawMessage
	// RunAt zero means now.
	RunAt time.Time
	// ExpiresAt only extends the computed deadline; it never shortens it.
	ExpiresAt *time.Time
	IdemKey   *string
	JobID     *int64
}

const insertTaskSQL = `INSERT INTO tasks (device_id, job_id, batch_id, batch_seq,
	name, args, prev, status, run_at, expires_at, idem_key)
VALUES ($1, $2, $3::uuid, $4, $5, $6::jsonb, $7::jsonb, 'pending', $8, $9, $10)
ON CONFLICT DO NOTHING
RETURNING ` + taskCols

// CreateTasks enqueues tasks for a device id or serial. More than one task
// shares a new batch with 1-based batch_seq order.
func (s *Store) CreateTasks(ctx context.Context, deviceIDorSerial string, tasks []NewTask) ([]Task, error) {
	if len(tasks) == 0 {
		return []Task{}, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: create tasks: %w", err)
	}
	defer tx.Rollback(ctx)

	deviceID, interval, err := resolveDevice(ctx, tx, deviceIDorSerial)
	if err != nil {
		return nil, err
	}

	var batchID *string
	if len(tasks) > 1 {
		var id string
		if err := tx.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&id); err != nil {
			return nil, fmt.Errorf("store: create batch: %w", err)
		}
		batchID = &id
	}

	out, err := s.insertTasks(ctx, tx, deviceID, interval, batchID, tasks, time.Now().UTC())
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("store: create tasks: %w", err)
	}
	return out, nil
}

// insertTasks writes tasks for one device inside an existing transaction.
// CreateTasks and CreateJob share it so expiry and argument handling cannot
// diverge.
func (s *Store) insertTasks(ctx context.Context, tx pgx.Tx, deviceID string, interval int,
	batchID *string, tasks []NewTask, now time.Time) ([]Task, error) {
	floor := s.TaskExpiry
	if floor <= 0 {
		floor = 10 * time.Minute
	}
	out := make([]Task, 0, len(tasks))
	for i, t := range tasks {
		if !taskNames[t.Name] {
			return nil, fmt.Errorf("store: unknown task name %q", t.Name)
		}
		runAt := t.RunAt.UTC()
		if runAt.IsZero() {
			runAt = now
		}
		expires := runAt.Add(time.Duration(2*interval) * time.Second)
		if floorAt := runAt.Add(floor); floorAt.After(expires) {
			expires = floorAt
		}
		if t.ExpiresAt != nil && t.ExpiresAt.After(expires) {
			expires = t.ExpiresAt.UTC()
		}
		seq := 0
		if batchID != nil {
			seq = i + 1
		}
		args := t.Args
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		var prev []byte
		if len(t.Prev) > 0 {
			prev = t.Prev
		}

		task, err := scanTask(tx.QueryRow(ctx, insertTaskSQL, deviceID, t.JobID,
			batchID, seq, t.Name, []byte(args), prev, runAt, expires, t.IdemKey))
		if errors.Is(err, pgx.ErrNoRows) && t.IdemKey != nil {
			task, err = scanTask(tx.QueryRow(ctx, `SELECT `+taskCols+` FROM tasks
				WHERE device_id = $1 AND idem_key = $2`, deviceID, *t.IdemKey))
		}
		if err != nil {
			return nil, fmt.Errorf("store: create task: %w", err)
		}
		out = append(out, task)
	}
	return out, nil
}

// StartTask moves a pending task to running and increments attempts.
func (s *Store) StartTask(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE tasks SET status = 'running', attempts = attempts + 1, updated_at = now()
		WHERE id = $1 AND status = 'pending'`, id)
	if err != nil {
		return fmt.Errorf("store: start task: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrTaskNotPending
	}
	return nil
}

// FinishTask writes the terminal or retryable state of a task.
func (s *Store) FinishTask(ctx context.Context, id int64, status string, result json.RawMessage, faultCode *int, fault string) error {
	if !taskStatuses[status] {
		return fmt.Errorf("store: invalid task status %q", status)
	}
	var res []byte
	if len(result) > 0 {
		res = result
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE tasks SET status = $2, result = $3, fault_code = $4, fault = $5,
			updated_at = now() WHERE id = $1`, id, status, res, faultCode, fault)
	if err != nil {
		return fmt.Errorf("store: finish task: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrTaskNotFound
	}
	return nil
}

// CancelBatchRest cancels the pending tasks of a batch after the given seq.
func (s *Store) CancelBatchRest(ctx context.Context, batchID string, afterSeq int) error {
	if _, err := s.pool.Exec(ctx, `UPDATE tasks SET status = 'cancelled', updated_at = now()
		WHERE batch_id = $1::uuid AND batch_seq > $2 AND status = 'pending'`,
		batchID, afterSeq); err != nil {
		return fmt.Errorf("store: cancel batch: %w", err)
	}
	return nil
}

// RequeueTask returns a running task to pending for a retry.
func (s *Store) RequeueTask(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE tasks SET status = 'pending', updated_at = now()
		WHERE id = $1 AND status = 'running'`, id)
	if err != nil {
		return fmt.Errorf("store: requeue task: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("store: requeue task %d: task is not running", id)
	}
	return nil
}

// ListTasks lists a device's tasks newest first. Limit zero means 100.
func (s *Store) ListTasks(ctx context.Context, deviceIDorSerial string, limit int) ([]Task, error) {
	if limit <= 0 {
		limit = 100
	}
	deviceID, _, err := resolveDevice(ctx, s.pool, deviceIDorSerial)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+taskCols+`
		FROM tasks WHERE device_id = $1 ORDER BY id DESC LIMIT $2`, deviceID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list tasks: %w", err)
	}
	return scanTasks(rows)
}

// WaitingPing returns the device's running ping that is waiting for
// 8 DIAGNOSTICS COMPLETE, or nil when there is none.
func (s *Store) WaitingPing(ctx context.Context, deviceID string) (*Task, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+taskCols+` FROM tasks
		WHERE device_id = $1 AND name = 'ping' AND status = 'running'
		  AND result->>'phase' = 'requested'
		ORDER BY id LIMIT 1`, deviceID)
	if err != nil {
		return nil, fmt.Errorf("store: waiting ping: %w", err)
	}
	tasks, err := scanTasks(rows)
	if err != nil {
		return nil, fmt.Errorf("store: waiting ping: %w", err)
	}
	if len(tasks) == 0 {
		return nil, nil
	}
	return &tasks[0], nil
}

// SetTaskResult updates a task's result without changing its status.
func (s *Store) SetTaskResult(ctx context.Context, id int64, result json.RawMessage) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE tasks SET result = $2, updated_at = now() WHERE id = $1`, id, []byte(result))
	if err != nil {
		return fmt.Errorf("store: set task result: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrTaskNotFound
	}
	return nil
}

// ExpireTasks marks runnable-past tasks expired. It is the janitor's sweep.
// Running pings are included because they wait in running until the device
// reports 8 DIAGNOSTICS COMPLETE, which may never arrive.
func (s *Store) ExpireTasks(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE tasks SET status = 'expired', updated_at = now()
		WHERE expires_at <= now()
		  AND (status = 'pending' OR (status = 'running' AND name = 'ping'))`)
	if err != nil {
		return 0, fmt.Errorf("store: expire tasks: %w", err)
	}
	return tag.RowsAffected(), nil
}

// PurgeOld deletes finished tasks and events older than their retention. A
// zero retention keeps rows forever. Pending and running tasks are never
// touched, however old.
func (s *Store) PurgeOld(ctx context.Context, taskRetention, eventRetention time.Duration) (tasks, events int64, err error) {
	if taskRetention > 0 {
		tag, err := s.pool.Exec(ctx, `DELETE FROM tasks
			WHERE status IN ('done','failed','unknown','cancelled','expired')
			  AND updated_at < now() - $1::interval`, taskRetention.String())
		if err != nil {
			return 0, 0, fmt.Errorf("store: purge tasks: %w", err)
		}
		tasks = tag.RowsAffected()
	}
	if eventRetention > 0 {
		tag, err := s.pool.Exec(ctx, `DELETE FROM events WHERE at < now() - $1::interval`, eventRetention.String())
		if err != nil {
			return tasks, 0, fmt.Errorf("store: purge events: %w", err)
		}
		events = tag.RowsAffected()
	}
	return tasks, events, nil
}

func resolveDevice(ctx context.Context, q rowQuerier, idOrSerial string) (string, int, error) {
	var (
		id       string
		interval int
	)
	err := q.QueryRow(ctx, `SELECT id, inform_interval FROM devices
		WHERE id = $1 OR serial = $1 ORDER BY (id = $1) DESC LIMIT 1`,
		idOrSerial).Scan(&id, &interval)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, ErrDeviceNotFound
	}
	if err != nil {
		return "", 0, fmt.Errorf("store: resolve device: %w", err)
	}
	return id, interval, nil
}

// PendingTasks returns the device's runnable pending tasks, in run order.
// The CWMP session re-reads it when its queue drains, to pick up tasks
// enqueued while the session was open.
func (s *Store) PendingTasks(ctx context.Context, deviceID string) ([]Task, error) {
	rows, err := s.pool.Query(ctx, selectPendingTasksSQL, deviceID)
	if err != nil {
		return nil, fmt.Errorf("store: pending tasks: %w", err)
	}
	return scanTasks(rows)
}

// SetTaskPrev records the values a set task is about to overwrite (§8).
func (s *Store) SetTaskPrev(ctx context.Context, id int64, prev json.RawMessage) error {
	tag, err := s.pool.Exec(ctx, `UPDATE tasks SET prev = $2 WHERE id = $1`, id, []byte(prev))
	if err != nil {
		return fmt.Errorf("store: set task prev: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrTaskNotFound
	}
	return nil
}
