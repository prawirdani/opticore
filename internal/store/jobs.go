package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	// ErrJobNotFound reports an unknown job id.
	ErrJobNotFound = errors.New("job not found")
	// ErrJobSelectorEmpty reports a POST /jobs with no selector at all.
	ErrJobSelectorEmpty = errors.New("job selector is empty: set serials, ids, product_class or software_version")
	// ErrJobNoDevices reports a selector that matched nothing.
	ErrJobNoDevices = errors.New("job selector matches no devices")
)

// UnknownDevicesError lists explicit ids/serials that are not in the devices
// table. A job is never half-created: the whole request fails.
type UnknownDevicesError struct {
	Keys []string
}

func (e *UnknownDevicesError) Error() string {
	return "unknown device ids or serials: " + strings.Join(e.Keys, ", ")
}

// Job is a row of jobs.
type Job struct {
	ID          int64
	Name        string
	Spec        json.RawMessage
	MaxParallel int
	Status      string
	CreatedAt   time.Time
}

// JobSelector selects the devices a job expands to. An explicit list and a
// product class/version filter may be combined: explicit devices still have
// to match the filter.
type JobSelector struct {
	Serials         []string `json:"serials"`
	IDs             []string `json:"ids"`
	ProductClass    string   `json:"product_class"`
	SoftwareVersion string   `json:"software_version"`
}

// NewJob is one bulk job to create. Task is the template copied per device.
type NewJob struct {
	Name        string
	Spec        json.RawMessage
	Selector    JobSelector
	Task        NewTask
	MaxParallel int
	RunAt       time.Time
}

// JobFailure is one failed task of a job, for the per-device failure list.
type JobFailure struct {
	DeviceID  string `json:"device_id"`
	TaskID    int64  `json:"task_id"`
	FaultCode *int   `json:"fault_code"`
	Fault     string `json:"fault"`
}

const jobCols = `id, name, spec, max_parallel, status, created_at`

func scanJob(row pgx.Row) (Job, error) {
	var (
		j    Job
		spec []byte
	)
	if err := row.Scan(&j.ID, &j.Name, &spec, &j.MaxParallel, &j.Status, &j.CreatedAt); err != nil {
		return Job{}, err
	}
	j.Spec = spec
	return j, nil
}

// CreateJob resolves the selector and creates the job row plus one task per
// device in a single transaction. Unknown explicit devices roll the whole
// job back.
func (s *Store) CreateJob(ctx context.Context, j NewJob) (Job, []Task, error) {
	if j.MaxParallel <= 0 {
		j.MaxParallel = 10
	}
	spec := j.Spec
	if len(spec) == 0 {
		spec = json.RawMessage(`{}`)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Job{}, nil, fmt.Errorf("store: create job: %w", err)
	}
	defer tx.Rollback(ctx)

	devices, err := selectJobDevices(ctx, tx, j.Selector)
	if err != nil {
		return Job{}, nil, err
	}
	if len(devices) == 0 {
		return Job{}, nil, ErrJobNoDevices
	}

	job, err := scanJob(tx.QueryRow(ctx,
		`INSERT INTO jobs (name, spec, max_parallel) VALUES ($1, $2::jsonb, $3)
		RETURNING `+jobCols, j.Name, []byte(spec), j.MaxParallel))
	if err != nil {
		return Job{}, nil, fmt.Errorf("store: create job: %w", err)
	}

	task := j.Task
	task.RunAt = j.RunAt.UTC()
	task.JobID = &job.ID
	now := time.Now().UTC()
	out := make([]Task, 0, len(devices))
	for _, d := range devices {
		tasks, err := s.insertTasks(ctx, tx, d.id, d.interval, nil, []NewTask{task}, now)
		if err != nil {
			return Job{}, nil, err
		}
		out = append(out, tasks...)
	}

	if err := tx.Commit(ctx); err != nil {
		return Job{}, nil, fmt.Errorf("store: create job: %w", err)
	}
	return job, out, nil
}

type jobDevice struct {
	id              string
	interval        int
	productClass    string
	softwareVersion string
}

// selectJobDevices resolves the selector. Explicit keys are checked first, so
// an unknown one is reported even when the class filter would have dropped it.
func selectJobDevices(ctx context.Context, tx pgx.Tx, sel JobSelector) ([]jobDevice, error) {
	keys := append(append([]string{}, sel.Serials...), sel.IDs...)
	if len(keys) == 0 {
		if sel.ProductClass == "" && sel.SoftwareVersion == "" {
			return nil, ErrJobSelectorEmpty
		}
		rows, err := tx.Query(ctx, `SELECT id, inform_interval, product_class, software_version
			FROM devices
			WHERE ($1 = '' OR product_class = $1)
				AND ($2 = '' OR software_version = $2)
			ORDER BY id`, sel.ProductClass, sel.SoftwareVersion)
		if err != nil {
			return nil, fmt.Errorf("store: select job devices: %w", err)
		}
		defer rows.Close()
		var out []jobDevice
		for rows.Next() {
			var d jobDevice
			if err := rows.Scan(&d.id, &d.interval, &d.productClass, &d.softwareVersion); err != nil {
				return nil, fmt.Errorf("store: select job devices: %w", err)
			}
			out = append(out, d)
		}
		return out, rows.Err()
	}

	rows, err := tx.Query(ctx, `SELECT id, serial, inform_interval, product_class, software_version
		FROM devices WHERE serial = ANY($1) OR id = ANY($1)`, keys)
	if err != nil {
		return nil, fmt.Errorf("store: select job devices: %w", err)
	}
	defer rows.Close()
	bySerial := map[string]jobDevice{}
	byID := map[string]jobDevice{}
	for rows.Next() {
		var id, serial string
		var d jobDevice
		if err := rows.Scan(&id, &serial, &d.interval, &d.productClass, &d.softwareVersion); err != nil {
			return nil, fmt.Errorf("store: select job devices: %w", err)
		}
		d.id = id
		byID[id] = d
		bySerial[serial] = d
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: select job devices: %w", err)
	}

	var missing []string
	seen := map[string]bool{}
	out := make([]jobDevice, 0, len(keys))
	for _, k := range keys {
		d, ok := byID[k]
		if !ok {
			d, ok = bySerial[k]
		}
		if !ok {
			missing = append(missing, k)
			continue
		}
		if seen[d.id] {
			continue
		}
		seen[d.id] = true
		if sel.ProductClass != "" && d.productClass != sel.ProductClass {
			continue
		}
		if sel.SoftwareVersion != "" && d.softwareVersion != sel.SoftwareVersion {
			continue
		}
		out = append(out, d)
	}
	if len(missing) > 0 {
		return nil, &UnknownDevicesError{Keys: missing}
	}
	return out, nil
}

// GetJob returns one job by id.
func (s *Store) GetJob(ctx context.Context, id int64) (Job, error) {
	j, err := scanJob(s.pool.QueryRow(ctx, `SELECT `+jobCols+` FROM jobs WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrJobNotFound
	}
	if err != nil {
		return Job{}, fmt.Errorf("store: get job: %w", err)
	}
	return j, nil
}

// ListJobs lists jobs newest first. Limit zero means 100.
func (s *Store) ListJobs(ctx context.Context, limit int) ([]Job, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+jobCols+` FROM jobs ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list jobs: %w", err)
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list jobs: %w", err)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// JobCounts counts a job's tasks by status.
func (s *Store) JobCounts(ctx context.Context, jobID int64) (map[string]int, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT status, count(*) FROM tasks WHERE job_id = $1 GROUP BY status`, jobID)
	if err != nil {
		return nil, fmt.Errorf("store: job counts: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, fmt.Errorf("store: job counts: %w", err)
		}
		out[status] = n
	}
	return out, rows.Err()
}

// JobFailures lists a job's failed and unknown tasks in id order.
func (s *Store) JobFailures(ctx context.Context, jobID int64) ([]JobFailure, error) {
	rows, err := s.pool.Query(ctx, `SELECT device_id, id, fault_code, fault FROM tasks
		WHERE job_id = $1 AND status IN ('failed','unknown') ORDER BY id`, jobID)
	if err != nil {
		return nil, fmt.Errorf("store: job failures: %w", err)
	}
	defer rows.Close()
	out := []JobFailure{}
	for rows.Next() {
		var f JobFailure
		if err := rows.Scan(&f.DeviceID, &f.TaskID, &f.FaultCode, &f.Fault); err != nil {
			return nil, fmt.Errorf("store: job failures: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// CancelJob marks the job cancelled and its pending tasks cancelled. Running
// tasks are left to finish.
func (s *Store) CancelJob(ctx context.Context, id int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: cancel job: %w", err)
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx, `UPDATE jobs SET status = 'cancelled' WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: cancel job: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrJobNotFound
	}
	if _, err := tx.Exec(ctx, `UPDATE tasks SET status = 'cancelled', updated_at = now()
		WHERE job_id = $1 AND status = 'pending'`, id); err != nil {
		return fmt.Errorf("store: cancel job tasks: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: cancel job: %w", err)
	}
	return nil
}

// RunningJobs returns the jobs the pump should consider.
func (s *Store) RunningJobs(ctx context.Context) ([]Job, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+jobCols+` FROM jobs WHERE status = 'running' ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: running jobs: %w", err)
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("store: running jobs: %w", err)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// JobRunningCount counts a job's running tasks.
func (s *Store) JobRunningCount(ctx context.Context, jobID int64) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM tasks WHERE job_id = $1 AND status = 'running'`, jobID).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: job running count: %w", err)
	}
	return n, nil
}

// DueJobTasks returns a job's runnable pending tasks in id order, for the
// concurrency pump.
// exclude skips tasks already woken, so the pump moves on to the next devices.
func (s *Store) DueJobTasks(ctx context.Context, jobID int64, limit int, exclude []int64) ([]Task, error) {
	if limit <= 0 {
		return []Task{}, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT `+taskCols+` FROM tasks
		WHERE job_id = $1 AND status = 'pending'
			AND run_at <= now() AND expires_at > now()
			AND NOT (id = ANY($3))
		ORDER BY id LIMIT $2`, jobID, limit, exclude)
	if err != nil {
		return nil, fmt.Errorf("store: due job tasks: %w", err)
	}
	return scanTasks(rows)
}

// FinishJobs marks running jobs done when they have no pending or running
// task left. Cancelled jobs are left alone.
func (s *Store) FinishJobs(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE jobs SET status = 'done'
		WHERE status = 'running' AND NOT EXISTS (
			SELECT 1 FROM tasks
			WHERE tasks.job_id = jobs.id AND tasks.status IN ('pending','running'))`)
	if err != nil {
		return 0, fmt.Errorf("store: finish jobs: %w", err)
	}
	return tag.RowsAffected(), nil
}
