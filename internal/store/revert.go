package store

import (
	"context"
	"fmt"
)

// RevertableJobTasks returns a job's done set tasks that recorded prev, in id
// order. It is the source for a per-job revert (§8).
func (s *Store) RevertableJobTasks(ctx context.Context, jobID int64) ([]Task, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+taskCols+` FROM tasks
		WHERE job_id = $1 AND name = 'set' AND status = 'done'
			AND prev IS NOT NULL AND prev <> '{}'::jsonb
		ORDER BY id`, jobID)
	if err != nil {
		return nil, fmt.Errorf("store: revertable job tasks: %w", err)
	}
	return scanTasks(rows)
}
