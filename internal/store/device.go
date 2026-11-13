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

type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

const upsertDeviceSQL = `INSERT INTO devices (id, serial, oui, product_class,
	software_version, first_inform, last_inform, inform_interval, last_addr,
	cr_url, params)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (id) DO UPDATE SET
	last_inform = GREATEST(devices.last_inform, EXCLUDED.last_inform),
	last_addr = EXCLUDED.last_addr,
	software_version = CASE WHEN EXCLUDED.software_version <> ''
		THEN EXCLUDED.software_version ELSE devices.software_version END,
	cr_url = CASE WHEN EXCLUDED.cr_url <> ''
		THEN EXCLUDED.cr_url ELSE devices.cr_url END,
	inform_interval = CASE WHEN EXCLUDED.inform_interval > 0
		THEN EXCLUDED.inform_interval ELSE devices.inform_interval END,
	params = CASE WHEN $12 THEN EXCLUDED.params ELSE devices.params || EXCLUDED.params END,
	pending_reboot = CASE WHEN $12 THEN false ELSE devices.pending_reboot END
RETURNING ` + deviceCols

const insertEventSQL = `INSERT INTO events (device_id, at, type, detail, dedupe_key)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (device_id, dedupe_key) WHERE dedupe_key IS NOT NULL DO NOTHING`

const selectPendingTasksSQL = `SELECT ` + taskCols + ` FROM tasks
WHERE device_id = $1 AND status = 'pending'
	AND run_at <= now() AND expires_at > now()
ORDER BY batch_id NULLS LAST, batch_seq, id`

// RecordInform writes one Inform in a single batch inside a transaction:
// device upsert, one deduped event per event code, and the device's runnable
// pending tasks.
func (s *Store) RecordInform(ctx context.Context, in InformRecord) (Device, []Task, error) {
	if in.ID == "" {
		return Device{}, nil, errors.New("store: inform record has no device id")
	}
	at := in.At
	if at.IsZero() {
		at = time.Now().UTC()
	}
	params, err := encodeParams(in.Params)
	if err != nil {
		return Device{}, nil, fmt.Errorf("store: encode params: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Device{}, nil, fmt.Errorf("store: record inform: %w", err)
	}
	defer tx.Rollback(ctx)

	b := &pgx.Batch{}
	b.Queue(upsertDeviceSQL, in.ID, in.Serial, in.OUI, in.ProductClass,
		in.SoftwareVersion, at, at, in.InformInterval, in.LastAddr, in.CRURL, params, in.Bootstrap)
	for _, code := range in.Events {
		typ := eventType(code)
		detail, err := json.Marshal(struct {
			Codes []string `json:"codes"`
			Raw   string   `json:"raw"`
		}{Codes: in.Events, Raw: code})
		if err != nil {
			return Device{}, nil, fmt.Errorf("store: encode event detail: %w", err)
		}
		var key *string
		if in.CurrentTime.Year() >= 2000 {
			k := typ + "|" + in.CurrentTime.UTC().Format(time.RFC3339Nano) + "|" + code
			key = &k
		}
		b.Queue(insertEventSQL, in.ID, at, typ, detail, key)
	}
	b.Queue(selectPendingTasksSQL, in.ID)

	br := tx.SendBatch(ctx, b)
	dev, err := scanDevice(br.QueryRow())
	if err != nil {
		br.Close()
		return Device{}, nil, fmt.Errorf("store: record inform: %w", err)
	}
	for range in.Events {
		if _, err := br.Exec(); err != nil {
			br.Close()
			return Device{}, nil, fmt.Errorf("store: record inform event: %w", err)
		}
	}
	taskRows, err := br.Query()
	if err != nil {
		br.Close()
		return Device{}, nil, fmt.Errorf("store: record inform tasks: %w", err)
	}
	tasks, err := scanTasks(taskRows)
	if err != nil {
		br.Close()
		return Device{}, nil, fmt.Errorf("store: record inform tasks: %w", err)
	}
	if err := br.Close(); err != nil {
		return Device{}, nil, fmt.Errorf("store: record inform: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Device{}, nil, fmt.Errorf("store: record inform: %w", err)
	}
	return dev, tasks, nil
}

// eventType maps a CWMP event code such as "0 BOOTSTRAP" to an event type.
func eventType(code string) string {
	num, _, _ := strings.Cut(strings.TrimSpace(code), " ")
	switch num {
	case "0":
		return "bootstrap"
	case "1":
		return "boot"
	case "2":
		return "periodic"
	case "4":
		return "value_change"
	case "6":
		return "connection_request"
	case "7":
		return "transfer_complete"
	case "8":
		return "diagnostics_complete"
	default:
		return "inform"
	}
}

// GetDevice resolves an ACS id or a serial.
func (s *Store) GetDevice(ctx context.Context, idOrSerial string) (Device, error) {
	d, err := scanDevice(s.pool.QueryRow(ctx,
		`SELECT `+deviceCols+` FROM devices WHERE id = $1 OR serial = $1
		ORDER BY (id = $1) DESC LIMIT 1`, idOrSerial))
	if errors.Is(err, pgx.ErrNoRows) {
		return Device{}, ErrDeviceNotFound
	}
	if err != nil {
		return Device{}, fmt.Errorf("store: get device: %w", err)
	}
	return d, nil
}

// ListFilter selects devices for ListDevices. Limit zero means 100.
type ListFilter struct {
	SerialPrefix string
	ProductClass string
	Limit        int
}

// ListDevices lists devices newest-inform first.
func (s *Store) ListDevices(ctx context.Context, f ListFilter) ([]Device, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `SELECT `+deviceCols+` FROM devices
		WHERE ($1 = '' OR serial LIKE $1 || '%')
			AND ($2 = '' OR product_class = $2)
		ORDER BY last_inform DESC LIMIT $3`, f.SerialPrefix, f.ProductClass, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list devices: %w", err)
	}
	defer rows.Close()
	devices := []Device{}
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list devices: %w", err)
		}
		devices = append(devices, d)
	}
	return devices, rows.Err()
}

// MergeParams merges parameter leaves into the device's cache.
func (s *Store) MergeParams(ctx context.Context, deviceID string, params map[string]Param) error {
	b, err := encodeParams(params)
	if err != nil {
		return fmt.Errorf("store: encode params: %w", err)
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE devices SET params = params || $2::jsonb WHERE id = $1`, deviceID, b)
	if err != nil {
		return fmt.Errorf("store: merge params: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrDeviceNotFound
	}
	return nil
}

// SetCRCredentials stores the connection-request password.
func (s *Store) SetCRCredentials(ctx context.Context, deviceID, user, encPassword string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE devices
		SET cr_username = $2, cr_password = $3, cr_set_at = now()
		WHERE id = $1`, deviceID, user, encPassword)
	if err != nil {
		return fmt.Errorf("store: set cr credentials: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrDeviceNotFound
	}
	return nil
}

// SetLastFault records the device's most recent fault.
func (s *Store) SetLastFault(ctx context.Context, deviceID string, fault json.RawMessage) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE devices SET last_fault = $2 WHERE id = $1`, deviceID, []byte(fault))
	if err != nil {
		return fmt.Errorf("store: set last fault: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrDeviceNotFound
	}
	return nil
}

// SetPendingReboot records that a SetParameterValues returned Status 1.
func (s *Store) SetPendingReboot(ctx context.Context, deviceID string, pending bool) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE devices SET pending_reboot = $2 WHERE id = $1`, deviceID, pending)
	if err != nil {
		return fmt.Errorf("store: set pending reboot: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrDeviceNotFound
	}
	return nil
}

// ClearCRCredentials forgets the connection-request credentials, so the next
// Inform sets them again. Used when a wake is refused: the device clearly does
// not hold what we think it does.
func (s *Store) ClearCRCredentials(ctx context.Context, deviceID string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE devices
		SET cr_username = '', cr_password = '', cr_set_at = NULL
		WHERE id = $1`, deviceID)
	if err != nil {
		return fmt.Errorf("store: clear cr credentials: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrDeviceNotFound
	}
	return nil
}
