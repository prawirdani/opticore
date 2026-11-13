// Package store is the PostgreSQL data layer: pgx/v5 with hand-written SQL.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/prawirdani/opticore/migrations"
)

// leaderLockKey is a constant advisory-lock key ("opticore" in ASCII).
const leaderLockKey int64 = 0x6F707469636F7265

var (
	ErrAnotherDaemon  = errors.New("another opticore daemon holds the advisory lock")
	ErrDeviceNotFound = errors.New("device not found")
	ErrTaskNotFound   = errors.New("task not found")
	ErrTaskNotPending = errors.New("task is not pending")
)

// Param is one cached parameter leaf, serialised into devices.params.
type Param struct {
	Value     string     `json:"value,omitempty"`
	Type      string     `json:"type,omitempty"`
	Writable  *bool      `json:"writable,omitempty"`
	Secret    bool       `json:"secret,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// Device is a row of devices, keyed by the ACS id.
type Device struct {
	ID              string
	Serial          string
	OUI             string
	ProductClass    string
	SoftwareVersion string
	FirstInform     time.Time
	LastInform      time.Time
	InformInterval  int
	LastAddr        string
	CRURL           string
	CRUsername      string
	CRPassword      string
	CRSetAt         *time.Time
	PendingReboot   bool
	LastFault       json.RawMessage
	Params          map[string]Param
}

// Task is a row of tasks.
type Task struct {
	ID        int64
	DeviceID  string
	JobID     *int64
	BatchID   *string
	BatchSeq  int
	Name      string
	Args      json.RawMessage
	Prev      json.RawMessage
	Result    json.RawMessage
	Status    string
	Attempts  int
	FaultCode *int
	Fault     string
	CreatedAt time.Time
	RunAt     time.Time
	ExpiresAt time.Time
	UpdatedAt time.Time
	IdemKey   *string
}

// Event is a row of events.
type Event struct {
	ID       int64
	DeviceID string
	At       time.Time
	Type     string
	Detail   json.RawMessage
}

// InformRecord is everything an Inform writes.
type InformRecord struct {
	ID              string
	Serial          string
	OUI             string
	ProductClass    string
	SoftwareVersion string
	InformInterval  int
	LastAddr        string
	CRURL           string
	// At is when the ACS received the Inform (server clock); zero means now.
	At time.Time
	// CurrentTime is the device's own clock from the Inform. A retried Inform
	// repeats it, which makes the event dedupe key stable. Implausible values
	// (zero, or before 2000, common right after boot) disable dedupe.
	CurrentTime time.Time
	// Bootstrap is a "0 BOOTSTRAP" inform: the device's configuration may have
	// been wiped, so the parameter cache is replaced instead of merged.
	Bootstrap bool
	Events    []string
	Params    map[string]Param
}

// Store owns the pool. TaskExpiry is the minimum pending task lifetime used by
// CreateTasks; zero means 10 minutes.
type Store struct {
	pool       *pgxpool.Pool
	TaskExpiry time.Duration

	leader        *pgxpool.Conn
	releaseLeader func()
}

// Open creates a pool and verifies the database is reachable.
func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close releases the leadership lock, if held, and the pool.
func (s *Store) Close() {
	if s.releaseLeader != nil {
		s.releaseLeader()
		s.releaseLeader = nil
	}
	s.pool.Close()
}

// Migrate applies the embedded goose migrations.
func (s *Store) Migrate(ctx context.Context) error {
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	db := stdlib.OpenDBFromPool(s.pool)
	defer db.Close()
	if err := goose.UpContext(ctx, db, "."); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	return nil
}

// SchemaVersion is the applied goose migration version.
func (s *Store) SchemaVersion(ctx context.Context) (int64, error) {
	var v int64
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(MAX(version_id), 0) FROM goose_db_version WHERE is_applied`).Scan(&v)
	return v, err
}

// IsMissingTable reports a Postgres undefined_table error (42P01): the schema
// was never migrated.
func IsMissingTable(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "42P01"
}

// AcquireLeader takes the single-daemon advisory lock on a pinned connection.
// Call it once at startup; the lock dies with the connection.
func (s *Store) AcquireLeader(ctx context.Context) (func(), error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: acquire leader connection: %w", err)
	}
	var once sync.Once
	var ok bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, leaderLockKey).Scan(&ok); err != nil {
		conn.Release()
		return nil, fmt.Errorf("store: advisory lock: %w", err)
	}
	if !ok {
		conn.Release()
		return nil, ErrAnotherDaemon
	}
	release := func() {
		once.Do(func() {
			_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, leaderLockKey)
			conn.Release()
		})
	}
	s.leader = conn
	s.releaseLeader = release
	return release, nil
}

// Ping reports database reachability.
func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

// NewestInform returns the newest devices.last_inform, or the zero time when
// no device has ever informed.
func (s *Store) NewestInform(ctx context.Context) (time.Time, error) {
	var at *time.Time
	if err := s.pool.QueryRow(ctx, `SELECT max(last_inform) FROM devices`).Scan(&at); err != nil {
		return time.Time{}, fmt.Errorf("store: newest inform: %w", err)
	}
	if at == nil {
		return time.Time{}, nil
	}
	return *at, nil
}

const deviceCols = `id, serial, oui, product_class, software_version, first_inform,
	last_inform, inform_interval, last_addr, cr_url, cr_username, cr_password,
	cr_set_at, pending_reboot, last_fault, params`

const taskCols = `id, device_id, job_id, batch_id::text, batch_seq, name, args,
	prev, result, status, attempts, fault_code, fault, created_at, run_at,
	expires_at, updated_at, idem_key`

func scanDevice(row pgx.Row) (Device, error) {
	var (
		d         Device
		lastFault []byte
		params    []byte
	)
	err := row.Scan(&d.ID, &d.Serial, &d.OUI, &d.ProductClass, &d.SoftwareVersion,
		&d.FirstInform, &d.LastInform, &d.InformInterval, &d.LastAddr, &d.CRURL,
		&d.CRUsername, &d.CRPassword, &d.CRSetAt, &d.PendingReboot, &lastFault, &params)
	if err != nil {
		return Device{}, err
	}
	if len(lastFault) > 0 {
		d.LastFault = lastFault
	}
	d.Params = map[string]Param{}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &d.Params); err != nil {
			return Device{}, fmt.Errorf("store: decode params: %w", err)
		}
	}
	return d, nil
}

func scanTask(row pgx.Row) (Task, error) {
	var (
		t      Task
		args   []byte
		prev   []byte
		result []byte
	)
	err := row.Scan(&t.ID, &t.DeviceID, &t.JobID, &t.BatchID, &t.BatchSeq,
		&t.Name, &args, &prev, &result, &t.Status, &t.Attempts, &t.FaultCode,
		&t.Fault, &t.CreatedAt, &t.RunAt, &t.ExpiresAt, &t.UpdatedAt, &t.IdemKey)
	if err != nil {
		return Task{}, err
	}
	t.Args = args
	t.Prev = prev
	t.Result = result
	return t, nil
}

func scanTasks(rows pgx.Rows) ([]Task, error) {
	defer rows.Close()
	tasks := []Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

// Pool exposes the connection pool for test setup (truncation, fixtures).
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// DaemonRunning reports whether some session holds the single-daemon advisory
// lock, i.e. an `opticore serve` is up against this database. It reads
// pg_locks instead of trying the lock, so a caller never takes it by accident.
func (s *Store) DaemonRunning(ctx context.Context) (bool, error) {
	var held bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM pg_locks
		WHERE locktype = 'advisory' AND granted AND objsubid = 1 -- the bigint form
		  AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
		  AND ((classid::bigint << 32) | objid::bigint) = $1)`, leaderLockKey).Scan(&held)
	if err != nil {
		return false, fmt.Errorf("store: daemon running: %w", err)
	}
	return held, nil
}

// EnsureDatabase creates the database named in dsn when the server reports it
// does not exist (SQLSTATE 3D000), using the same credentials against the
// "postgres" maintenance database. It is for `migrate` and `serve` only; the
// role needs CREATEDB, and any other connection error is returned unchanged.
func EnsureDatabase(ctx context.Context, dsn string) (created bool, err error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err == nil {
		conn.Close(ctx)
		return false, nil
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "3D000" {
		return false, fmt.Errorf("store: connect: %w", err)
	}
	cfg, perr := pgx.ParseConfig(dsn)
	if perr != nil {
		return false, fmt.Errorf("store: parse dsn: %w", perr)
	}
	name := cfg.Database
	cfg.Database = "postgres"
	admin, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return false, fmt.Errorf("store: create database %q: connect to postgres: %w", name, err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		if errors.As(err, &pgErr) && pgErr.Code == "42P04" { // created meanwhile
			return false, nil
		}
		return false, fmt.Errorf("store: create database %q: %w", name, err)
	}
	return true, nil
}
