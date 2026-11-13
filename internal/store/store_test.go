package store

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

var testDSN string // schema-scoped DSN; empty when integration tests are skipped

func TestMain(m *testing.M) {
	base := os.Getenv("OPTICORE_TEST_DSN")
	if base == "" {
		os.Exit(m.Run())
	}

	ctx := context.Background()
	schema := "test_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		fmt.Fprintf(os.Stderr, "store tests: connect: %v\n", err)
		os.Exit(1)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		fmt.Fprintf(os.Stderr, "store tests: create schema: %v\n", err)
		os.Exit(1)
	}

	testDSN = withSearchPath(base, schema)
	s, err := Open(ctx, testDSN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "store tests: open: %v\n", err)
		os.Exit(1)
	}
	if err := s.Migrate(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "store tests: migrate: %v\n", err)
		os.Exit(1)
	}
	s.Close()

	code := m.Run()
	if _, err := admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
		fmt.Fprintf(os.Stderr, "store tests: drop schema: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	if err := admin.Close(ctx); err != nil && code == 0 {
		fmt.Fprintf(os.Stderr, "store tests: close admin: %v\n", err)
		code = 1
	}
	os.Exit(code)
}

func withSearchPath(dsn, schema string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}

func testStore(t *testing.T) *Store {
	t.Helper()
	if testDSN == "" {
		t.Skip("OPTICORE_TEST_DSN not set")
	}
	ctx := context.Background()
	s, err := Open(ctx, testDSN)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(s.Close)
	if _, err := s.pool.Exec(ctx, `TRUNCATE devices, tasks, events, jobs,
		firmware, api_tokens RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return s
}

func count(t *testing.T, s *Store, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func sampleInform(at time.Time) InformRecord {
	return InformRecord{
		ID:              "00259E-HG8145V5-48575443BD7589AB",
		Serial:          "48575443BD7589AB",
		OUI:             "00259E",
		ProductClass:    "HG8145V5",
		SoftwareVersion: "V5R020C10S271",
		InformInterval:  300,
		LastAddr:        "10.20.3.41:49152",
		CRURL:           "http://10.20.3.41:7547/abc",
		At:              at,
		CurrentTime:     at,
		Events:          []string{"0 BOOTSTRAP", "1 BOOT"},
		Params: map[string]Param{
			"InternetGatewayDevice.ManagementServer.ConnectionRequestURL": {
				Value: "http://10.20.3.41:7547/abc", Type: "xsd:string",
			},
			"InternetGatewayDevice.ManagementServer.Password":     {Value: "hunter2"},
			"InternetGatewayDevice.ManagementServer.ParameterKey": {Value: "visible"},
		},
	}
}

// A factory reset (0 BOOTSTRAP) must not keep pre-reset leaves in the cache.
func TestBootstrapReplacesParamsAndClearsPendingReboot(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	at := time.Now().UTC()
	in := sampleInform(at)
	if _, _, err := s.RecordInform(ctx, in); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPendingReboot(ctx, in.ID, true); err != nil {
		t.Fatal(err)
	}

	reset := sampleInform(at.Add(time.Minute))
	reset.Bootstrap = true
	reset.CurrentTime = reset.At
	reset.Events = []string{"0 BOOTSTRAP"}
	reset.Params = map[string]Param{
		"InternetGatewayDevice.DeviceInfo.SoftwareVersion": {Value: "V5R020C10S271", Type: "xsd:string"},
	}
	dev, _, err := s.RecordInform(ctx, reset)
	if err != nil {
		t.Fatal(err)
	}
	if _, still := dev.Params["InternetGatewayDevice.ManagementServer.ConnectionRequestURL"]; still {
		t.Error("pre-reset parameter survived the bootstrap")
	}
	if len(dev.Params) != 1 {
		t.Errorf("params = %v, want only the inform parameters", dev.Params)
	}
	if dev.PendingReboot {
		t.Error("pending_reboot survived the bootstrap")
	}
}

func TestRecordInformUpsertAndDedupe(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)
	in := sampleInform(at)

	dev, tasks, err := s.RecordInform(ctx, in)
	if err != nil {
		t.Fatalf("RecordInform: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("tasks = %d, want 0", len(tasks))
	}
	if dev.ID != in.ID || dev.Serial != in.Serial || dev.OUI != in.OUI || dev.ProductClass != in.ProductClass {
		t.Errorf("device identity = %+v", dev)
	}
	if !dev.FirstInform.Equal(at) || !dev.LastInform.Equal(at) {
		t.Errorf("inform times = %v %v, want %v", dev.FirstInform, dev.LastInform, at)
	}
	if dev.InformInterval != 300 || dev.LastAddr != in.LastAddr || dev.CRURL != in.CRURL {
		t.Errorf("device inform fields = %+v", dev)
	}
	if p := dev.Params["InternetGatewayDevice.ManagementServer.Password"]; !p.Secret || p.Value != "" {
		t.Errorf("secret param = %+v, want secret marker only", p)
	}
	if p := dev.Params["InternetGatewayDevice.ManagementServer.ParameterKey"]; p.Secret || p.Value != "visible" {
		t.Errorf("ParameterKey param = %+v, want visible value", p)
	}
	if n := count(t, s, `SELECT count(*) FROM events WHERE device_id = $1`, in.ID); n != 2 {
		t.Errorf("events = %d, want 2", n)
	}

	// A retried Inform repeats CurrentTime: no new device or event rows.
	if _, _, err := s.RecordInform(ctx, in); err != nil {
		t.Fatalf("RecordInform retry: %v", err)
	}
	if n := count(t, s, `SELECT count(*) FROM devices`); n != 1 {
		t.Errorf("devices = %d, want 1", n)
	}
	if n := count(t, s, `SELECT count(*) FROM events WHERE device_id = $1`, in.ID); n != 2 {
		t.Errorf("events after retry = %d, want 2", n)
	}

	// Device clock before 2000 (fresh boot): dedupe off, every inform is kept.
	boot := in
	boot.CurrentTime = time.Date(1970, 1, 1, 0, 0, 5, 0, time.UTC)
	boot.Events = []string{"1 BOOT"}
	for range 2 {
		if _, _, err := s.RecordInform(ctx, boot); err != nil {
			t.Fatalf("RecordInform 1970: %v", err)
		}
	}
	if n := count(t, s, `SELECT count(*) FROM events WHERE device_id = $1 AND dedupe_key IS NULL`, in.ID); n != 2 {
		t.Errorf("undeduped 1970 events = %d, want 2", n)
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM events WHERE dedupe_key IS NULL`); err != nil {
		t.Fatal(err)
	}

	in.At = at.Add(time.Minute)
	in.CurrentTime = in.At
	in.Events = []string{"2 PERIODIC"}
	in.SoftwareVersion = "V5R020C10S999"
	in.LastAddr = "10.20.3.42:49153"
	dev, _, err = s.RecordInform(ctx, in)
	if err != nil {
		t.Fatalf("RecordInform later: %v", err)
	}
	if !dev.FirstInform.Equal(at) || !dev.LastInform.Equal(in.At) {
		t.Errorf("inform times = %v %v", dev.FirstInform, dev.LastInform)
	}
	if dev.SoftwareVersion != in.SoftwareVersion || dev.LastAddr != in.LastAddr {
		t.Errorf("later inform did not update device: %+v", dev)
	}
	if n := count(t, s, `SELECT count(*) FROM events WHERE device_id = $1`, in.ID); n != 3 {
		t.Errorf("events = %d, want 3", n)
	}
}

func TestPendingTaskSelectionOrder(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Second)
	in := sampleInform(at)
	in.Events = nil
	in.Params = nil
	if _, _, err := s.RecordInform(ctx, in); err != nil {
		t.Fatalf("RecordInform: %v", err)
	}

	now := time.Now().UTC()
	batch, err := s.CreateTasks(ctx, in.Serial, []NewTask{
		{Name: "get", Args: []byte(`{"path":"InternetGatewayDevice."}`), RunAt: now.Add(-2 * time.Minute)},
		{Name: "set", Args: []byte(`{"values":[]}`), RunAt: now.Add(-2 * time.Minute)},
	})
	if err != nil {
		t.Fatalf("CreateTasks batch: %v", err)
	}
	if batch[0].BatchID == nil || batch[1].BatchID == nil ||
		*batch[0].BatchID != *batch[1].BatchID {
		t.Fatalf("batch ids = %v %v, want shared non-nil", batch[0].BatchID, batch[1].BatchID)
	}
	if batch[0].BatchSeq != 1 || batch[1].BatchSeq != 2 {
		t.Errorf("batch seq = %d %d, want 1 2", batch[0].BatchSeq, batch[1].BatchSeq)
	}

	if _, err := s.CreateTasks(ctx, in.ID, []NewTask{
		{Name: "reboot", RunAt: now.Add(time.Hour)},       // not yet runnable
		{Name: "ping", RunAt: now.Add(-20 * time.Minute)}, // past expires
	}); err != nil {
		t.Fatalf("CreateTasks noise: %v", err)
	}

	_, tasks, err := s.RecordInform(ctx, in)
	if err != nil {
		t.Fatalf("RecordInform second: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("pending tasks = %d, want 2: %+v", len(tasks), tasks)
	}
	if tasks[0].ID != batch[0].ID || tasks[1].ID != batch[1].ID {
		t.Errorf("pending order = %d,%d want %d,%d", tasks[0].ID, tasks[1].ID, batch[0].ID, batch[1].ID)
	}
}

func TestCreateTasksIdempotencyAndCancel(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, _, err := s.RecordInform(ctx, sampleInform(time.Now().UTC())); err != nil {
		t.Fatalf("RecordInform: %v", err)
	}
	idem := "req-1"
	first, err := s.CreateTasks(ctx, "48575443BD7589AB", []NewTask{
		{Name: "get", Args: []byte(`{"path":"InternetGatewayDevice."}`), IdemKey: &idem},
	})
	if err != nil {
		t.Fatalf("CreateTasks: %v", err)
	}
	second, err := s.CreateTasks(ctx, "48575443BD7589AB", []NewTask{
		{Name: "get", Args: []byte(`{"path":"InternetGatewayDevice."}`), IdemKey: &idem},
	})
	if err != nil {
		t.Fatalf("CreateTasks retry: %v", err)
	}
	if len(first) != 1 || len(second) != 1 || first[0].ID != second[0].ID {
		t.Fatalf("idempotency: first=%+v second=%+v", first, second)
	}
	if n := count(t, s, `SELECT count(*) FROM tasks`); n != 1 {
		t.Errorf("tasks = %d, want 1", n)
	}
	if first[0].BatchID != nil {
		t.Errorf("single task batch = %v, want nil", *first[0].BatchID)
	}

	if _, err := s.CreateTasks(ctx, "no-such-serial", []NewTask{{Name: "get"}}); !errors.Is(err, ErrDeviceNotFound) {
		t.Errorf("unknown device error = %v, want ErrDeviceNotFound", err)
	}

	batch, err := s.CreateTasks(ctx, "48575443BD7589AB", []NewTask{
		{Name: "get"}, {Name: "set"}, {Name: "reboot"},
	})
	if err != nil {
		t.Fatalf("CreateTasks batch: %v", err)
	}
	if err := s.CancelBatchRest(ctx, *batch[0].BatchID, 1); err != nil {
		t.Fatalf("CancelBatchRest: %v", err)
	}
	var cancelled int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM tasks
		WHERE batch_id = $1::uuid AND status = 'cancelled'`, *batch[0].BatchID).Scan(&cancelled); err != nil {
		t.Fatal(err)
	}
	if cancelled != 2 {
		t.Errorf("cancelled = %d, want 2", cancelled)
	}

	// Expiry sweep.
	if _, err := s.pool.Exec(ctx, `UPDATE tasks SET run_at = now() - interval '1 hour',
		expires_at = now() - interval '1 hour' WHERE id = $1`, batch[0].ID); err != nil {
		t.Fatal(err)
	}
	if n, err := s.ExpireTasks(ctx); err != nil || n != 1 {
		t.Errorf("ExpireTasks = %d, %v, want 1, nil", n, err)
	}
}

func TestAdvisoryLock(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	release, err := s.AcquireLeader(ctx)
	if err != nil {
		t.Fatalf("AcquireLeader: %v", err)
	}

	other, err := Open(ctx, testDSN)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer other.Close()
	if _, err := other.AcquireLeader(ctx); !errors.Is(err, ErrAnotherDaemon) {
		t.Fatalf("second AcquireLeader = %v, want ErrAnotherDaemon", err)
	}

	release()
	if _, err := other.AcquireLeader(ctx); err != nil {
		t.Fatalf("AcquireLeader after release: %v", err)
	}
}

func TestHealthHelpers(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got, err := s.NewestInform(ctx); err != nil || !got.IsZero() {
		t.Fatalf("NewestInform empty = %v, %v", got, err)
	}
	at := time.Now().UTC().Truncate(time.Second)
	if _, _, err := s.RecordInform(ctx, sampleInform(at)); err != nil {
		t.Fatalf("RecordInform: %v", err)
	}
	if got, err := s.NewestInform(ctx); err != nil || !got.Equal(at) {
		t.Fatalf("NewestInform = %v, %v, want %v", got, err, at)
	}
}

func TestPurgeOld(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, _, err := s.RecordInform(ctx, sampleInform(time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	ts, err := s.CreateTasks(ctx, "48575443BD7589AB", []NewTask{{Name: "get"}, {Name: "get"}, {Name: "get"}})
	if err != nil {
		t.Fatal(err)
	}
	// old done, old pending, fresh done
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.pool.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE tasks SET status='done', updated_at = now() - interval '40 days' WHERE id = $1`, ts[0].ID)
	exec(`UPDATE tasks SET updated_at = now() - interval '40 days' WHERE id = $1`, ts[1].ID)
	exec(`UPDATE tasks SET status='done' WHERE id = $1`, ts[2].ID)
	exec(`UPDATE events SET at = now() - interval '40 days'`)

	nt, ne, err := s.PurgeOld(ctx, 30*24*time.Hour, 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if nt != 1 || ne == 0 {
		t.Errorf("purged tasks=%d events=%d, want 1 and >0", nt, ne)
	}
	if n := count(t, s, `SELECT count(*) FROM tasks`); n != 2 {
		t.Errorf("tasks left = %d, want 2 (old pending kept)", n)
	}
	if nt, ne, _ := s.PurgeOld(ctx, 0, 0); nt != 0 || ne != 0 {
		t.Error("zero retention must keep everything")
	}
}
