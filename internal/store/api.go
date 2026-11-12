package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrTokenNotFound reports an unknown, revoked or malformed API token.
var ErrTokenNotFound = errors.New("api token not found")

// CreateToken mints a token and stores only its SHA-256 hex digest. The
// plaintext is returned once; it is never recoverable from the row.
func (s *Store) CreateToken(ctx context.Context, name string, scopes []string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("store: create token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	if _, err := s.pool.Exec(ctx, `INSERT INTO api_tokens (name, hash, scopes)
		VALUES ($1, $2, $3)`, name, hex.EncodeToString(sum[:]), scopes); err != nil {
		return "", fmt.Errorf("store: create token: %w", err)
	}
	return token, nil
}

// LookupToken resolves a plaintext token to its scopes. Revoked tokens are
// not found.
func (s *Store) LookupToken(ctx context.Context, plaintext string) ([]string, error) {
	sum := sha256.Sum256([]byte(plaintext))
	var scopes []string
	err := s.pool.QueryRow(ctx, `SELECT scopes FROM api_tokens
		WHERE hash = $1 AND revoked_at IS NULL`, hex.EncodeToString(sum[:])).Scan(&scopes)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrTokenNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: lookup token: %w", err)
	}
	return scopes, nil
}

// DevicePageFilter selects one keyset page of devices. The zero After values
// start at the newest device.
type DevicePageFilter struct {
	SerialPrefix    string
	ProductClass    string
	Limit           int
	AfterLastInform time.Time
	AfterID         string
}

// ListDevicePage lists devices newest-inform first, after a (last_inform, id)
// cursor. Limit zero means 100.
func (s *Store) ListDevicePage(ctx context.Context, f DevicePageFilter) ([]Device, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	var after *time.Time
	if !f.AfterLastInform.IsZero() {
		after = &f.AfterLastInform
	}
	rows, err := s.pool.Query(ctx, `SELECT `+deviceCols+` FROM devices
		WHERE ($1 = '' OR serial LIKE $1 || '%')
			AND ($2 = '' OR product_class = $2)
			AND ($3::timestamptz IS NULL OR (last_inform, id) < ($3, $4))
		ORDER BY last_inform DESC, id DESC LIMIT $5`,
		f.SerialPrefix, f.ProductClass, after, f.AfterID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list device page: %w", err)
	}
	defer rows.Close()
	devices := []Device{}
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list device page: %w", err)
		}
		devices = append(devices, d)
	}
	return devices, rows.Err()
}

// CountDevices counts devices matching the same filters as ListDevicePage.
func (s *Store) CountDevices(ctx context.Context, serialPrefix, productClass string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM devices
		WHERE ($1 = '' OR serial LIKE $1 || '%')
			AND ($2 = '' OR product_class = $2)`, serialPrefix, productClass).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count devices: %w", err)
	}
	return n, nil
}

// ListTasksFiltered lists a device's tasks newest first, optionally by status.
func (s *Store) ListTasksFiltered(ctx context.Context, deviceIDorSerial, status string, limit int) ([]Task, error) {
	if limit <= 0 {
		limit = 100
	}
	deviceID, _, err := resolveDevice(ctx, s.pool, deviceIDorSerial)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+taskCols+` FROM tasks
		WHERE device_id = $1 AND ($2 = '' OR status = $2)
		ORDER BY id DESC LIMIT $3`, deviceID, status, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list tasks filtered: %w", err)
	}
	return scanTasks(rows)
}

// GetTask returns one task by id.
func (s *Store) GetTask(ctx context.Context, id int64) (Task, error) {
	t, err := scanTask(s.pool.QueryRow(ctx,
		`SELECT `+taskCols+` FROM tasks WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Task{}, ErrTaskNotFound
	}
	if err != nil {
		return Task{}, fmt.Errorf("store: get task: %w", err)
	}
	return t, nil
}

// CancelTask cancels one pending task.
func (s *Store) CancelTask(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, `UPDATE tasks SET status = 'cancelled', updated_at = now()
		WHERE id = $1 AND status = 'pending'`, id)
	if err != nil {
		return fmt.Errorf("store: cancel task: %w", err)
	}
	if tag.RowsAffected() > 0 {
		return nil
	}
	var status string
	err = s.pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id = $1`, id).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrTaskNotFound
	}
	if err != nil {
		return fmt.Errorf("store: cancel task: %w", err)
	}
	return ErrTaskNotPending
}

// DeleteDevice forgets a device; tasks and events cascade.
func (s *Store) DeleteDevice(ctx context.Context, idOrSerial string) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM devices WHERE id = $1 OR serial = $1`, idOrSerial)
	if err != nil {
		return fmt.Errorf("store: delete device: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrDeviceNotFound
	}
	return nil
}

// TaskSummary counts a device's tasks by status.
func (s *Store) TaskSummary(ctx context.Context, deviceIDorSerial string) (map[string]int, error) {
	deviceID, _, err := resolveDevice(ctx, s.pool, deviceIDorSerial)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT status, count(*) FROM tasks WHERE device_id = $1 GROUP BY status`, deviceID)
	if err != nil {
		return nil, fmt.Errorf("store: task summary: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, fmt.Errorf("store: task summary: %w", err)
		}
		out[status] = n
	}
	return out, rows.Err()
}

const eventCols = `id, device_id, at, type, detail`

func scanEvent(row pgx.Row) (Event, error) {
	var (
		e      Event
		detail []byte
	)
	if err := row.Scan(&e.ID, &e.DeviceID, &e.At, &e.Type, &detail); err != nil {
		return Event{}, err
	}
	e.Detail = detail
	return e, nil
}

// ListEvents lists a device's events newest first. before zero starts at the
// newest event.
func (s *Store) ListEvents(ctx context.Context, deviceIDorSerial string, limit int, before time.Time) ([]Event, error) {
	if limit <= 0 {
		limit = 100
	}
	deviceID, _, err := resolveDevice(ctx, s.pool, deviceIDorSerial)
	if err != nil {
		return nil, err
	}
	var b *time.Time
	if !before.IsZero() {
		b = &before
	}
	rows, err := s.pool.Query(ctx, `SELECT `+eventCols+` FROM events
		WHERE device_id = $1 AND ($2::timestamptz IS NULL OR at < $2)
		ORDER BY at DESC, id DESC LIMIT $3`, deviceID, b, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list events: %w", err)
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list events: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// EventsSinceID returns events with id greater than afterID, oldest first.
func (s *Store) EventsSinceID(ctx context.Context, afterID int64, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.pool.Query(ctx, `SELECT `+eventCols+` FROM events
		WHERE id > $1 ORDER BY id LIMIT $2`, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: events since: %w", err)
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("store: events since: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// TasksUpdatedSince returns tasks changed after (since, sinceID), oldest
// first. The pair keeps same-microsecond updates from being skipped.
func (s *Store) TasksUpdatedSince(ctx context.Context, since time.Time, sinceID int64, limit int) ([]Task, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.pool.Query(ctx, `SELECT `+taskCols+` FROM tasks
		WHERE (updated_at, id) > ($1, $2)
		ORDER BY updated_at, id LIMIT $3`, since, sinceID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: tasks updated since: %w", err)
	}
	return scanTasks(rows)
}

// EventAt returns one event's timestamp, for mapping a resume cursor. A
// missing row (retention removed it) yields the zero time.
func (s *Store) EventAt(ctx context.Context, id int64) (time.Time, error) {
	var at time.Time
	err := s.pool.QueryRow(ctx, `SELECT at FROM events WHERE id = $1`, id).Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("store: event at: %w", err)
	}
	return at, nil
}

// Now returns the database clock, so SSE task cursors line up with the
// updated_at values the database writes.
func (s *Store) Now(ctx context.Context) (time.Time, error) {
	var t time.Time
	if err := s.pool.QueryRow(ctx, `SELECT now()`).Scan(&t); err != nil {
		return time.Time{}, fmt.Errorf("store: now: %w", err)
	}
	return t, nil
}

// MaxEventID returns the newest events.id, or 0.
func (s *Store) MaxEventID(ctx context.Context) (int64, error) {
	var id int64
	if err := s.pool.QueryRow(ctx, `SELECT coalesce(max(id), 0) FROM events`).Scan(&id); err != nil {
		return 0, fmt.Errorf("store: max event id: %w", err)
	}
	return id, nil
}

// MaxEventIDAt returns the newest events.id at or before at, or 0.
func (s *Store) MaxEventIDAt(ctx context.Context, at time.Time) (int64, error) {
	var id int64
	if err := s.pool.QueryRow(ctx,
		`SELECT coalesce(max(id), 0) FROM events WHERE at <= $1`, at).Scan(&id); err != nil {
		return 0, fmt.Errorf("store: max event id at: %w", err)
	}
	return id, nil
}
