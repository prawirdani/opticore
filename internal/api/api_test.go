package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/prawirdani/opticore/internal/acs"
	"github.com/prawirdani/opticore/internal/store"
	"github.com/prawirdani/opticore/internal/wake"
)

var testDSN string

func TestMain(m *testing.M) {
	base := os.Getenv("OPTICORE_TEST_DSN")
	if base == "" {
		os.Exit(m.Run())
	}
	ctx := context.Background()
	schema := "api_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		fmt.Fprintf(os.Stderr, "api tests: connect: %v\n", err)
		os.Exit(1)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		fmt.Fprintf(os.Stderr, "api tests: create schema: %v\n", err)
		os.Exit(1)
	}
	testDSN = withSearchPath(base, schema)
	s, err := store.Open(ctx, testDSN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "api tests: open: %v\n", err)
		os.Exit(1)
	}
	if err := s.Migrate(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "api tests: migrate: %v\n", err)
		os.Exit(1)
	}
	s.Close()

	code := m.Run()
	if _, err := admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
		fmt.Fprintf(os.Stderr, "api tests: drop schema: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	admin.Close(ctx)
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

func testStore(t *testing.T) *store.Store {
	t.Helper()
	if testDSN == "" {
		t.Skip("OPTICORE_TEST_DSN not set")
	}
	s, err := store.Open(context.Background(), testDSN)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(s.Close)
	if _, err := s.Pool().Exec(context.Background(), `TRUNCATE devices, tasks, events,
		jobs, firmware RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return s
}

func newTestServer(t *testing.T, st *store.Store) (*Server, *httptest.Server, *acs.Service) {
	t.Helper()
	svc := &acs.Service{Store: st}
	srv := New(st, svc, log.New(io.Discard, "", 0))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	t.Cleanup(srv.Close)
	return srv, ts, svc
}

func seedDevice(t *testing.T, st *store.Store, id, serial string, at time.Time, crURL string) {
	t.Helper()
	in := store.InformRecord{
		ID:              id,
		Serial:          serial,
		OUI:             "00259E",
		ProductClass:    "HG8145V5",
		SoftwareVersion: "V5R020C10S271",
		InformInterval:  300,
		LastAddr:        "10.20.3.41:49152",
		CRURL:           crURL,
		At:              at,
		CurrentTime:     at,
		Events:          []string{"0 BOOTSTRAP"},
		Params: map[string]store.Param{
			"InternetGatewayDevice.DeviceInfo.SoftwareVersion": {
				Value: "V5R020C10S271", Type: "xsd:string",
				UpdatedAt: &at,
			},
			"InternetGatewayDevice.ManagementServer.Password":     {Secret: true},
			"InternetGatewayDevice.ManagementServer.ParameterKey": {Value: "visible", Type: "xsd:string"},
		},
	}
	if _, _, err := st.RecordInform(context.Background(), in); err != nil {
		t.Fatalf("RecordInform: %v", err)
	}
	if crURL != "" {
		if err := st.SetCRCredentials(context.Background(), id, "acs", ""); err != nil {
			t.Fatalf("SetCRCredentials: %v", err)
		}
	}
}

func request(t *testing.T, client *http.Client, method, url string, body any) *http.Response {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

func decode(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

func errorCode(t *testing.T, resp *http.Response) string {
	t.Helper()
	var body struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	decode(t, resp, &body)
	if body.Error.Details == nil {
		t.Errorf("error details = nil, want {}")
	}
	return body.Error.Code
}

// The /v1 API takes no credentials: the listener address is the control.
func TestNoAuthorizationRequired(t *testing.T) {
	st := testStore(t)
	_, ts, _ := newTestServer(t, st)
	seedDevice(t, st, "00259E-HG8145V5-SER1", "SER1", time.Now().UTC(), "")

	resp := request(t, ts.Client(), http.MethodGet, ts.URL+"/v1/devices", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/devices without a token = %d, want 200", resp.StatusCode)
	}
	resp = request(t, ts.Client(), http.MethodPost, ts.URL+"/v1/devices/SER1/tasks",
		map[string]any{"name": "reboot"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST without a token = %d, want 202", resp.StatusCode)
	}
	resp = request(t, ts.Client(), http.MethodGet, ts.URL+"/v1/devices/SER1/params?reveal=1", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reveal without a token = %d, want 200", resp.StatusCode)
	}
}

func TestDeviceJSONAndFreshness(t *testing.T) {
	st := testStore(t)
	_, ts, _ := newTestServer(t, st)
	now := time.Now().UTC()
	seedDevice(t, st, "00259E-HG8145V5-SER1", "SER1", now, "http://10.20.3.41:7547/abc")

	var list struct {
		Data []struct {
			ID           string          `json:"id"`
			Serial       string          `json:"serial"`
			OUI          string          `json:"oui"`
			ProductClass string          `json:"product_class"`
			LastInform   time.Time       `json:"last_inform"`
			CRURL        string          `json:"cr_url"`
			CRReady      bool            `json:"cr_ready"`
			Freshness    string          `json:"freshness"`
			ParamsCount  int             `json:"params_count"`
			LastFault    json.RawMessage `json:"last_fault"`
		} `json:"data"`
		Meta struct {
			NextCursor *string `json:"next_cursor"`
			Total      int     `json:"total"`
		} `json:"meta"`
	}
	resp := request(t, ts.Client(), http.MethodGet, ts.URL+"/v1/devices", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET devices = %d", resp.StatusCode)
	}
	decode(t, resp, &list)
	if len(list.Data) != 1 || list.Meta.Total != 1 {
		t.Fatalf("list = %+v", list)
	}
	d := list.Data[0]
	if d.ID != "00259E-HG8145V5-SER1" || d.Serial != "SER1" || d.OUI != "00259E" ||
		d.ProductClass != "HG8145V5" || d.CRURL == "" || !d.CRReady ||
		d.Freshness != "fresh" || d.ParamsCount != 3 {
		t.Errorf("device JSON = %+v", d)
	}
	if string(d.LastFault) != "null" {
		t.Errorf("last_fault = %s, want null", d.LastFault)
	}

	var detail struct {
		Data struct {
			ID     string               `json:"id"`
			Params map[string]paramJSON `json:"params"`
			Tasks  map[string]int       `json:"tasks"`
		} `json:"data"`
	}
	resp = request(t, ts.Client(), http.MethodGet, ts.URL+"/v1/devices/SER1", nil)
	decode(t, resp, &detail)
	if detail.Data.ID == "" || len(detail.Data.Params) != 3 {
		t.Errorf("detail = %+v", detail.Data)
	}
	if detail.Data.Tasks["total"] != 0 {
		t.Errorf("task summary = %v, want total 0", detail.Data.Tasks)
	}

	// freshness boundaries: 2 intervals = fresh, 6 = stale, beyond = offline.
	at := time.Now().UTC().Add(-3 * 300 * time.Second)
	if _, err := st.Pool().Exec(context.Background(),
		`UPDATE devices SET last_inform = $1 WHERE serial = 'SER1'`, at); err != nil {
		t.Fatal(err)
	}
	resp = request(t, ts.Client(), http.MethodGet, ts.URL+"/v1/devices", nil)
	decode(t, resp, &list)
	if list.Data[0].Freshness != "stale" {
		t.Errorf("freshness at 3 intervals = %q, want stale", list.Data[0].Freshness)
	}
	at = time.Now().UTC().Add(-7 * 300 * time.Second)
	if _, err := st.Pool().Exec(context.Background(),
		`UPDATE devices SET last_inform = $1 WHERE serial = 'SER1'`, at); err != nil {
		t.Fatal(err)
	}
	resp = request(t, ts.Client(), http.MethodGet, ts.URL+"/v1/devices", nil)
	decode(t, resp, &list)
	if list.Data[0].Freshness != "offline" {
		t.Errorf("freshness at 7 intervals = %q, want offline", list.Data[0].Freshness)
	}
}

func TestParamsMaskingAndReveal(t *testing.T) {
	st := testStore(t)
	_, ts, _ := newTestServer(t, st)
	seedDevice(t, st, "00259E-HG8145V5-SER1", "SER1", time.Now().UTC(), "")

	var out struct {
		Data []struct {
			Path  string `json:"path"`
			Value string `json:"value"`
			Type  string `json:"type"`
		} `json:"data"`
	}
	resp := request(t, ts.Client(), http.MethodGet, ts.URL+"/v1/devices/SER1/params", nil)
	decode(t, resp, &out)
	values := map[string]string{}
	for _, p := range out.Data {
		values[p.Path] = p.Value
	}
	if values["InternetGatewayDevice.ManagementServer.Password"] != "••••" {
		t.Errorf("secret value = %q, want mask", values["InternetGatewayDevice.ManagementServer.Password"])
	}
	if values["InternetGatewayDevice.ManagementServer.ParameterKey"] != "visible" {
		t.Errorf("ParameterKey = %q, want visible", values["InternetGatewayDevice.ManagementServer.ParameterKey"])
	}

	resp = request(t, ts.Client(), http.MethodGet, ts.URL+"/v1/devices/SER1/params?reveal=1", nil)
	decode(t, resp, &out)
	for _, p := range out.Data {
		if p.Path == "InternetGatewayDevice.ManagementServer.Password" && p.Value != "••••" {
			t.Errorf("secret with reveal = %q, want mask (value is never cached)", p.Value)
		}
	}

	resp = request(t, ts.Client(), http.MethodGet, ts.URL+"/v1/devices/SER1/params?prefix=InternetGatewayDevice.ManagementServer.", nil)
	decode(t, resp, &out)
	if len(out.Data) != 2 {
		t.Errorf("prefix params = %d, want 2", len(out.Data))
	}
}

func TestBatchCreationAndIdempotency(t *testing.T) {
	st := testStore(t)
	_, ts, _ := newTestServer(t, st)
	seedDevice(t, st, "00259E-HG8145V5-SER1", "SER1", time.Now().UTC(), "")

	batch := []map[string]any{
		{"name": "reboot", "args": map[string]any{}},
		{"name": "get", "args": map[string]any{"paths": []string{"InternetGatewayDevice."}}},
	}
	body, _ := json.Marshal(batch)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/devices/SER1/tasks", bytes.NewReader(body))
	req.Header.Set("Idempotency-Key", "req-1")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var first struct {
		Data []struct {
			ID       int64   `json:"id"`
			BatchID  *string `json:"batch_id"`
			BatchSeq int     `json:"batch_seq"`
		} `json:"data"`
	}
	if resp.StatusCode != 202 {
		t.Fatalf("create batch = %d", resp.StatusCode)
	}
	decode(t, resp, &first)
	if len(first.Data) != 2 || first.Data[0].BatchID == nil ||
		*first.Data[0].BatchID != *first.Data[1].BatchID ||
		first.Data[0].BatchSeq != 1 || first.Data[1].BatchSeq != 2 {
		t.Fatalf("batch = %+v", first.Data)
	}

	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/v1/devices/SER1/tasks", bytes.NewReader(body))
	req.Header.Set("Idempotency-Key", "req-1")
	resp, err = ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var replay struct {
		Data []struct {
			ID int64 `json:"id"`
		} `json:"data"`
	}
	if resp.StatusCode != 202 {
		t.Fatalf("replay = %d", resp.StatusCode)
	}
	decode(t, resp, &replay)
	if len(replay.Data) != 2 || replay.Data[0].ID != first.Data[0].ID || replay.Data[1].ID != first.Data[1].ID {
		t.Fatalf("replay ids = %+v, want %+v", replay.Data, first.Data)
	}
	var count int
	if err := st.Pool().QueryRow(context.Background(), `SELECT count(*) FROM tasks`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("tasks = %d, want 2", count)
	}

	// unknown name
	resp = request(t, ts.Client(), http.MethodPost, ts.URL+"/v1/devices/SER1/tasks",
		map[string]any{"name": "download"})
	if resp.StatusCode != 400 || errorCode(t, resp) != "VALIDATION" {
		t.Errorf("unknown name = %d", resp.StatusCode)
	}

	// reserved connection-request path
	resp = request(t, ts.Client(), http.MethodPost, ts.URL+"/v1/devices/SER1/tasks",
		map[string]any{"name": "set", "args": map[string]any{"values": [][]string{
			{"InternetGatewayDevice.ManagementServer.ConnectionRequestPassword", "x", "xsd:string"},
		}}})
	if resp.StatusCode != 400 || errorCode(t, resp) != "VALIDATION" {
		t.Errorf("reserved path = %d", resp.StatusCode)
	}

	// unknown device
	resp = request(t, ts.Client(), http.MethodPost, ts.URL+"/v1/devices/NOPE/tasks",
		map[string]any{"name": "reboot"})
	if resp.StatusCode != 404 || errorCode(t, resp) != "DEVICE_NOT_FOUND" {
		t.Errorf("unknown device = %d", resp.StatusCode)
	}
}

func TestTaskRevealMasksSecrets(t *testing.T) {
	st := testStore(t)
	_, ts, _ := newTestServer(t, st)
	seedDevice(t, st, "00259E-HG8145V5-SER1", "SER1", time.Now().UTC(), "")

	resp := request(t, ts.Client(), http.MethodPost, ts.URL+"/v1/devices/SER1/tasks",
		map[string]any{"name": "set", "args": map[string]any{"values": [][]string{
			{"InternetGatewayDevice.ManagementServer.Password", "hunter2", "xsd:string"},
			{"InternetGatewayDevice.ManagementServer.Username", "visibleuser", "xsd:string"},
		}}})
	var created struct {
		Data []struct {
			ID int64 `json:"id"`
		} `json:"data"`
	}
	if resp.StatusCode != 202 {
		t.Fatalf("create = %d", resp.StatusCode)
	}
	decode(t, resp, &created)
	if err := st.SetTaskPrev(context.Background(), created.Data[0].ID, json.RawMessage(
		`{"InternetGatewayDevice.ManagementServer.Password":["oldsecret","xsd:string"],"InternetGatewayDevice.ManagementServer.Username":["olduser","xsd:string"]}`)); err != nil {
		t.Fatal(err)
	}

	var task struct {
		Data struct {
			Args struct {
				Values [][]string `json:"values"`
			} `json:"args"`
			Prev map[string][]string `json:"prev"`
		} `json:"data"`
	}
	resp = request(t, ts.Client(), http.MethodGet,
		fmt.Sprintf("%s/v1/tasks/%d", ts.URL, created.Data[0].ID), nil)
	decode(t, resp, &task)
	if task.Data.Args.Values[0][1] != "••••" {
		t.Errorf("masked task arg = %q", task.Data.Args.Values[0][1])
	}
	if task.Data.Args.Values[1][1] != "visibleuser" {
		t.Errorf("non-secret task arg = %q, want visible", task.Data.Args.Values[1][1])
	}
	if task.Data.Prev["InternetGatewayDevice.ManagementServer.Password"][0] != "••••" {
		t.Errorf("masked prev = %q", task.Data.Prev["InternetGatewayDevice.ManagementServer.Password"][0])
	}
	if task.Data.Prev["InternetGatewayDevice.ManagementServer.Username"][0] != "olduser" {
		t.Errorf("non-secret prev = %q, want olduser", task.Data.Prev["InternetGatewayDevice.ManagementServer.Username"][0])
	}

	resp = request(t, ts.Client(), http.MethodGet,
		fmt.Sprintf("%s/v1/tasks/%d?reveal=1", ts.URL, created.Data[0].ID), nil)
	decode(t, resp, &task)
	if task.Data.Args.Values[0][1] != "hunter2" {
		t.Errorf("revealed task arg = %q", task.Data.Args.Values[0][1])
	}
	if task.Data.Prev["InternetGatewayDevice.ManagementServer.Password"][0] != "oldsecret" {
		t.Errorf("revealed prev = %q", task.Data.Prev["InternetGatewayDevice.ManagementServer.Password"][0])
	}
}

func TestCancelPendingVsConflict(t *testing.T) {
	st := testStore(t)
	_, ts, _ := newTestServer(t, st)
	seedDevice(t, st, "00259E-HG8145V5-SER1", "SER1", time.Now().UTC(), "")

	resp := request(t, ts.Client(), http.MethodPost, ts.URL+"/v1/devices/SER1/tasks",
		map[string]any{"name": "reboot"})
	var created struct {
		Data []struct {
			ID int64 `json:"id"`
		} `json:"data"`
	}
	if resp.StatusCode != 202 {
		t.Fatalf("create = %d", resp.StatusCode)
	}
	decode(t, resp, &created)
	id := created.Data[0].ID

	resp = request(t, ts.Client(), http.MethodDelete,
		fmt.Sprintf("%s/v1/devices/SER1/tasks/%d", ts.URL, id), nil)
	if resp.StatusCode != 204 {
		t.Errorf("cancel pending = %d, want 204", resp.StatusCode)
	}
	resp.Body.Close()

	resp = request(t, ts.Client(), http.MethodDelete,
		fmt.Sprintf("%s/v1/devices/SER1/tasks/%d", ts.URL, id), nil)
	if resp.StatusCode != 409 || errorCode(t, resp) != "CONFLICT" {
		t.Errorf("cancel cancelled = %d", resp.StatusCode)
	}

	resp = request(t, ts.Client(), http.MethodDelete, ts.URL+"/v1/devices/SER1/tasks/999999", nil)
	if resp.StatusCode != 404 || errorCode(t, resp) != "TASK_NOT_FOUND" {
		t.Errorf("cancel missing = %d", resp.StatusCode)
	}
}

func TestWakeConflictAndUnreachable(t *testing.T) {
	st := testStore(t)
	_, ts, svc := newTestServer(t, st)
	seedDevice(t, st, "00259E-HG8145V5-SER1", "SER1", time.Now().UTC(), "")

	resp := request(t, ts.Client(), http.MethodPost, ts.URL+"/v1/devices/SER1/wake", nil)
	if resp.StatusCode != 409 || errorCode(t, resp) != "CONFLICT" {
		t.Errorf("wake without CR URL = %d", resp.StatusCode)
	}

	// An unreachable CR URL: a closed loopback port refuses fast.
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closedURL := closed.URL
	closed.Close()
	if _, err := st.Pool().Exec(context.Background(),
		`UPDATE devices SET cr_url = $1 WHERE serial = 'SER1'`, closedURL); err != nil {
		t.Fatal(err)
	}
	svc.Waker = wake.New(200*time.Millisecond, nil)
	resp = request(t, ts.Client(), http.MethodPost, ts.URL+"/v1/devices/SER1/wake", nil)
	if resp.StatusCode != 502 || errorCode(t, resp) != "DEVICE_UNREACHABLE" {
		t.Errorf("wake unreachable = %d", resp.StatusCode)
	}

	// A reachable CR endpoint succeeds.
	reachable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer reachable.Close()
	if _, err := st.Pool().Exec(context.Background(),
		`UPDATE devices SET cr_url = $1 WHERE serial = 'SER1'`, reachable.URL); err != nil {
		t.Fatal(err)
	}
	resp = request(t, ts.Client(), http.MethodPost, ts.URL+"/v1/devices/SER1/wake", nil)
	if resp.StatusCode != 202 {
		t.Errorf("wake reachable = %d, want 202", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestDeleteDeviceCascades(t *testing.T) {
	st := testStore(t)
	_, ts, _ := newTestServer(t, st)
	seedDevice(t, st, "00259E-HG8145V5-SER1", "SER1", time.Now().UTC(), "")
	if _, err := st.CreateTasks(context.Background(), "SER1", []store.NewTask{{Name: "reboot"}}); err != nil {
		t.Fatal(err)
	}

	resp := request(t, ts.Client(), http.MethodDelete, ts.URL+"/v1/devices/SER1", nil)
	if resp.StatusCode != 204 {
		t.Errorf("delete = %d, want 204", resp.StatusCode)
	}
	resp.Body.Close()

	resp = request(t, ts.Client(), http.MethodGet, ts.URL+"/v1/devices/SER1", nil)
	if resp.StatusCode != 404 || errorCode(t, resp) != "DEVICE_NOT_FOUND" {
		t.Errorf("get after delete = %d", resp.StatusCode)
	}
	var tasks, events int
	if err := st.Pool().QueryRow(context.Background(), `SELECT count(*) FROM tasks`).Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if err := st.Pool().QueryRow(context.Background(), `SELECT count(*) FROM events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if tasks != 0 || events != 0 {
		t.Errorf("cascade left tasks=%d events=%d", tasks, events)
	}
}

type sseFrame struct {
	id, event, data string
}

func readSSE(br *bufio.Reader) (sseFrame, error) {
	var f sseFrame
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return f, err
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if f.event != "" {
				return f, nil
			}
		case strings.HasPrefix(line, "id: "):
			f.id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			f.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			f.data = strings.TrimPrefix(line, "data: ")
		}
	}
}

func TestSSEBootstrapTaskAndResume(t *testing.T) {
	st := testStore(t)
	_, ts, _ := newTestServer(t, st)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/v1/events", nil)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("connect SSE: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("SSE status = %d", resp.StatusCode)
	}
	br := bufio.NewReader(resp.Body)

	// The stream is live now; these writes must reach it.
	seedDevice(t, st, "00259E-HG8145V5-SER1", "SER1", time.Now().UTC(), "")
	if _, err := st.CreateTasks(ctx, "SER1", []store.NewTask{{Name: "reboot"}}); err != nil {
		t.Fatalf("CreateTasks: %v", err)
	}

	var bootID string
	gotTask := false
	for !gotTask {
		f, err := readSSE(br)
		if err != nil {
			t.Fatalf("read SSE: %v", err)
		}
		switch f.event {
		case "bootstrap":
			bootID = f.id
			var data map[string]any
			if err := json.Unmarshal([]byte(f.data), &data); err != nil {
				t.Errorf("bootstrap data: %v", err)
			}
			if data["device_id"] != "00259E-HG8145V5-SER1" {
				t.Errorf("bootstrap device_id = %v", data["device_id"])
			}
		case "task":
			if bootID == "" {
				t.Fatal("task arrived before bootstrap")
			}
			gotTask = true
		}
	}

	// Resume after the bootstrap id: no bootstrap replay, task replays.
	cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer ccancel()
	req2, _ := http.NewRequestWithContext(cctx, http.MethodGet, ts.URL+"/v1/events", nil)
	req2.Header.Set("Last-Event-ID", bootID)
	resp2, err := ts.Client().Do(req2)
	if err != nil {
		t.Fatalf("resume SSE: %v", err)
	}
	defer resp2.Body.Close()
	br2 := bufio.NewReader(resp2.Body)
	f, err := readSSE(br2)
	if err != nil {
		t.Fatalf("read resume: %v", err)
	}
	if f.event == "bootstrap" {
		t.Errorf("resume replayed bootstrap (id %s)", f.id)
	}
	if f.event != "task" {
		t.Errorf("resume first event = %q, want task", f.event)
	}
}

func TestDeviceCursorPagination(t *testing.T) {
	st := testStore(t)
	_, ts, _ := newTestServer(t, st)
	base := time.Now().UTC().Truncate(time.Second)
	seedDevice(t, st, "00259E-HG8145V5-SER1", "SER1", base.Add(-2*time.Minute), "")
	seedDevice(t, st, "00259E-HG8145V5-SER2", "SER2", base.Add(-time.Minute), "")
	seedDevice(t, st, "00259E-HG8145V5-SER3", "SER3", base, "")

	type page struct {
		Data []struct {
			Serial string `json:"serial"`
		} `json:"data"`
		Meta struct {
			NextCursor *string `json:"next_cursor"`
			Total      int     `json:"total"`
		} `json:"meta"`
	}
	var p1 page
	resp := request(t, ts.Client(), http.MethodGet, ts.URL+"/v1/devices?limit=2", nil)
	decode(t, resp, &p1)
	if len(p1.Data) != 2 || p1.Data[0].Serial != "SER3" || p1.Data[1].Serial != "SER2" {
		t.Fatalf("page 1 = %+v", p1)
	}
	if p1.Meta.NextCursor == nil || p1.Meta.Total != 3 {
		t.Fatalf("page 1 meta = %+v", p1.Meta)
	}
	var p2 page
	resp = request(t, ts.Client(), http.MethodGet,
		ts.URL+"/v1/devices?limit=2&cursor="+url.QueryEscape(*p1.Meta.NextCursor), nil)
	decode(t, resp, &p2)
	if len(p2.Data) != 1 || p2.Data[0].Serial != "SER1" {
		t.Fatalf("page 2 = %+v", p2)
	}
	if p2.Meta.NextCursor != nil {
		t.Errorf("page 2 next cursor = %v, want nil", *p2.Meta.NextCursor)
	}
	if p2.Meta.Total != 3 {
		t.Errorf("page 2 total = %d", p2.Meta.Total)
	}

	var filtered page
	resp = request(t, ts.Client(), http.MethodGet, ts.URL+"/v1/devices?serial_prefix=SER2", nil)
	decode(t, resp, &filtered)
	if len(filtered.Data) != 1 || filtered.Data[0].Serial != "SER2" {
		t.Errorf("filtered = %+v", filtered)
	}
}
