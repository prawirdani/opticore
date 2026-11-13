package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordedCall struct {
	method string
	path   string
	query  string
	auth   string
	idem   string
	body   []byte
}

func TestClientRequests(t *testing.T) {
	var mu sync.Mutex
	var calls []recordedCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, recordedCall{
			method: r.Method, path: r.URL.Path, query: r.URL.RawQuery,
			auth: r.Header.Get("Authorization"), idem: r.Header.Get("Idempotency-Key"), body: b,
		})
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/devices":
			fmt.Fprint(w, `{"data":[{"id":"d1","serial":"S1"}],"meta":{"next_cursor":"c2","total":1}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/devices/d1":
			fmt.Fprint(w, `{"data":{"id":"d1","serial":"S1","params":{"A":{"value":"v"}}}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/devices/d1/params":
			fmt.Fprint(w, `{"data":[{"path":"A","value":"v","type":"xsd:string"}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/devices/d1/tasks":
			fmt.Fprint(w, `{"data":[{"id":7,"name":"reboot","status":"done"}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/devices/d1/events":
			fmt.Fprint(w, `{"data":[{"id":3,"device_id":"d1","type":"bootstrap"}]}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/devices/d1":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/devices/d1/tasks":
			fmt.Fprint(w, `{"data":[{"id":7,"name":"reboot","status":"pending"}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tasks/7":
			fmt.Fprint(w, `{"data":{"id":7,"name":"reboot","status":"done"}}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/devices/d1/tasks/7":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/devices/d1/wake":
			fmt.Fprint(w, `{"data":{"woken":true}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/devices/d1/tasks/7/revert":
			fmt.Fprint(w, `{"data":[{"id":8,"name":"set","status":"pending"}]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/jobs":
			fmt.Fprint(w, `{"data":{"id":4,"name":"j","status":"running","counts":{"total":2}}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/jobs":
			fmt.Fprint(w, `{"data":[{"id":4,"name":"j","status":"running","counts":{"total":2}}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/jobs/4":
			fmt.Fprint(w, `{"data":{"id":4,"name":"j","status":"done","counts":{"total":2}}}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/jobs/4":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/jobs/4/revert":
			fmt.Fprint(w, `{"data":{"id":5,"name":"revert","status":"running","counts":{"total":2}}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":{"code":"NOT_FOUND","message":"missing"}}`)
		}
	}))
	defer srv.Close()

	c, err := New(srv.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	page, err := c.ListDevices(ctx, ListDevicesOptions{
		SerialPrefix: "S", ProductClass: "P", Cursor: "c1", Limit: 2, Reveal: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Devices) != 1 || page.Total != 1 || page.NextCursor == nil || *page.NextCursor != "c2" {
		t.Errorf("page = %+v", page)
	}

	if d, err := c.GetDevice(ctx, "d1", true); err != nil || d.Params["A"].Value != "v" {
		t.Errorf("GetDevice = %+v, %v", d, err)
	}
	if ps, err := c.ListParams(ctx, "d1", "X.", true); err != nil || len(ps) != 1 || ps[0].Path != "A" {
		t.Errorf("ListParams = %+v, %v", ps, err)
	}
	if ts, err := c.ListTasks(ctx, "d1", ListTasksOptions{Status: "done", Limit: 5, Reveal: true}); err != nil ||
		len(ts) != 1 || ts[0].Status != "done" {
		t.Errorf("ListTasks = %+v, %v", ts, err)
	}
	before := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	if es, err := c.ListEvents(ctx, "d1", ListEventsOptions{Before: before, Limit: 3}); err != nil ||
		len(es) != 1 || es[0].Type != "bootstrap" {
		t.Errorf("ListEvents = %+v, %v", es, err)
	}
	if err := c.DeleteDevice(ctx, "d1"); err != nil {
		t.Error(err)
	}

	tasks, err := c.CreateTasks(ctx, "d1", []Task{Reboot()}, "idem-1")
	if err != nil || len(tasks) != 1 || tasks[0].ID != 7 {
		t.Fatalf("CreateTasks = %+v, %v", tasks, err)
	}
	if task, err := c.GetTask(ctx, 7, true); err != nil || task.Name != "reboot" {
		t.Errorf("GetTask = %+v, %v", task, err)
	}
	if err := c.CancelTask(ctx, "d1", 7); err != nil {
		t.Error(err)
	}
	if err := c.Wake(ctx, "d1"); err != nil {
		t.Error(err)
	}
	if rev, err := c.RevertTask(ctx, "d1", 7, "idem-2"); err != nil || len(rev) != 1 || rev[0].ID != 8 {
		t.Errorf("RevertTask = %+v, %v", rev, err)
	}

	spec := JobSpec{
		Name: "j", Task: Reboot(), MaxParallel: 2,
		Select: JobSelector{Serials: []string{"S1"}},
	}
	if job, err := c.CreateJob(ctx, spec); err != nil || job.ID != 4 {
		t.Errorf("CreateJob = %+v, %v", job, err)
	}
	if jobs, err := c.ListJobs(ctx, 3); err != nil || len(jobs) != 1 {
		t.Errorf("ListJobs = %+v, %v", jobs, err)
	}
	if job, err := c.GetJob(ctx, 4); err != nil || job.Status != "done" {
		t.Errorf("GetJob = %+v, %v", job, err)
	}
	if err := c.CancelJob(ctx, 4); err != nil {
		t.Error(err)
	}
	if job, err := c.RevertJob(ctx, 4); err != nil || job.ID != 5 {
		t.Errorf("RevertJob = %+v, %v", job, err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 16 {
		t.Fatalf("made %d calls, want 16: %+v", len(calls), calls)
	}
	for _, call := range calls {
		if call.auth != "Bearer tok" {
			t.Errorf("%s %s auth = %q", call.method, call.path, call.auth)
		}
	}

	byPath := map[string]recordedCall{}
	for _, call := range calls {
		byPath[call.method+" "+call.path] = call
	}
	for key, want := range map[string]string{
		"GET /v1/devices":           "cursor=c1&limit=2&product_class=P&reveal=1&serial_prefix=S",
		"GET /v1/devices/d1":        "reveal=1",
		"GET /v1/devices/d1/params": "prefix=X.&reveal=1",
		"GET /v1/devices/d1/tasks":  "limit=5&reveal=1&status=done",
		"GET /v1/devices/d1/events": "before=2026-11-13T10%3A00%3A00Z&limit=3",
		"GET /v1/tasks/7":           "reveal=1",
		"GET /v1/jobs":              "limit=3",
	} {
		call, ok := byPath[key]
		if !ok {
			t.Errorf("missing call %s", key)
			continue
		}
		if call.query != want {
			t.Errorf("%s query = %q, want %q", key, call.query, want)
		}
	}

	create := byPath["POST /v1/devices/d1/tasks"]
	if create.idem != "idem-1" {
		t.Errorf("CreateTasks idempotency = %q", create.idem)
	}
	var body []Task
	if err := json.Unmarshal(create.body, &body); err != nil || len(body) != 1 || body[0].Name != "reboot" {
		t.Errorf("CreateTasks body = %s, %v", create.body, err)
	}
	if byPath["POST /v1/devices/d1/tasks/7/revert"].idem != "idem-2" {
		t.Error("RevertTask idempotency header missing")
	}
	var jobBody JobSpec
	if err := json.Unmarshal(byPath["POST /v1/jobs"].body, &jobBody); err != nil ||
		jobBody.Name != "j" || jobBody.Task.Name != "reboot" || jobBody.MaxParallel != 2 {
		t.Errorf("CreateJob body = %s, %v", byPath["POST /v1/jobs"].body, err)
	}
}

func TestErrorDecoding(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"code":"DEVICE_NOT_FOUND","message":"no such device","details":{"id":"x"}}}`)
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "")

	_, err := c.GetDevice(context.Background(), "x", false)
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *Error", err)
	}
	if apiErr.Status != 404 || apiErr.Code != "DEVICE_NOT_FOUND" ||
		apiErr.Message != "no such device" || apiErr.Details["id"] != "x" {
		t.Errorf("error = %+v", apiErr)
	}

	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, "upstream down")
	}))
	defer plain.Close()
	c2, _ := New(plain.URL, "")
	_, err = c2.GetDevice(context.Background(), "x", false)
	if !errors.As(err, &apiErr) || apiErr.Status != 502 || apiErr.Message != "upstream down" {
		t.Errorf("plain error = %+v", err)
	}
}

func TestBuilders(t *testing.T) {
	for _, tc := range []struct {
		name string
		task Task
		want string
	}{
		{"get all", Get(), `{"name":"get","args":{}}`},
		{"get paths", Get("A.", "B."), `{"name":"get","args":{"paths":["A.","B."]}}`},
		{"set", Set(Value{Path: "A", Value: "b"}, Value{Path: "C", Value: "1", Type: "xsd:boolean"}),
			`{"name":"set","args":{"values":[["A","b","xsd:string"],["C","1","xsd:boolean"]]}}`},
		{"add", AddObject("X."), `{"name":"add_object","args":{"path":"X."}}`},
		{"delete", DeleteObject("X.3."), `{"name":"delete_object","args":{"path":"X.3."}}`},
		{"reboot", Reboot(), `{"name":"reboot","args":{}}`},
		{"reset", Reset(), `{"name":"reset","args":{}}`},
		{"ping", Ping("8.8.8.8"), `{"name":"ping","args":{"host":"8.8.8.8"}}`},
		{"ping options", Ping("8.8.8.8", PingCount(2), PingTimeout(1500*time.Millisecond), PingSize(32), PingInterface("wan")),
			`{"name":"ping","args":{"host":"8.8.8.8","count":2,"timeout_ms":1500,"size":32,"interface":"wan"}}`},
	} {
		b, err := json.Marshal(tc.task)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if string(b) != tc.want {
			t.Errorf("%s = %s, want %s", tc.name, b, tc.want)
		}
	}
}

func TestFaultHelper(t *testing.T) {
	code := 9005
	task := Task{Status: "failed", FaultCode: &code, FaultMessage: "Invalid parameter value"}
	f := task.Fault()
	if f == nil || f.Code != 9005 || f.String != "Invalid parameter value" {
		t.Fatalf("Fault = %+v", f)
	}
	if !strings.Contains(f.Error(), "9005") {
		t.Errorf("Error = %q", f.Error())
	}
	var target *Fault
	if !errors.As(fmt.Errorf("task failed: %w", task.Fault()), &target) || target.Code != 9005 {
		t.Errorf("errors.As failed: %v", target)
	}
	done := Task{Status: "done"}
	if done.Fault() != nil {
		t.Error("done task reported a fault")
	}
	var nilTask *Task
	if nilTask.Fault() != nil {
		t.Error("nil task reported a fault")
	}
}

func TestNewValidation(t *testing.T) {
	for _, base := range []string{"", "ftp://x", "://bad", "http://"} {
		if _, err := New(base, ""); err == nil {
			t.Errorf("New(%q) accepted", base)
		}
	}
	c, err := New("http://127.0.0.1:8081/", "tok")
	if err != nil || c.base.String() != "http://127.0.0.1:8081" {
		t.Errorf("New = %+v, %v", c, err)
	}
	hc := &http.Client{}
	c, _ = New("http://127.0.0.1:8081", "", WithHTTPClient(hc))
	if c.hc != hc {
		t.Error("WithHTTPClient did not apply")
	}
}
