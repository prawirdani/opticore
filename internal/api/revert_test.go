package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prawirdani/opticore/internal/acs"
	"github.com/prawirdani/opticore/internal/store"
)

// doneSet creates a finished set task with prev through the service, so the
// stored args are written exactly as the API would write them.
func doneSet(t *testing.T, st *store.Store, svc *acs.Service, device, prev string) store.Task {
	t.Helper()
	ctx := context.Background()
	tasks, err := svc.CreateTasks(ctx, device, []store.NewTask{{
		Name: "set",
		Args: json.RawMessage(`{"values":[["InternetGatewayDevice.ManagementServer.Password","newsecret","xsd:string"]]}`),
	}})
	if err != nil {
		t.Fatalf("CreateTasks: %v", err)
	}
	if err := st.SetTaskPrev(ctx, tasks[0].ID, json.RawMessage(prev)); err != nil {
		t.Fatalf("SetTaskPrev: %v", err)
	}
	if err := st.FinishTask(ctx, tasks[0].ID, "done", nil, nil, ""); err != nil {
		t.Fatalf("FinishTask: %v", err)
	}
	return tasks[0]
}

func postRevert(t *testing.T, ts *httptest.Server, url, idem string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

func TestRevertTaskAPI(t *testing.T) {
	st := testStore(t)
	_, ts, svc := newTestServer(t, st)
	now := time.Now().UTC()
	seedDevice(t, st, "00259E-HG8145V5-SER1", "SER1", now, "")
	seedDevice(t, st, "00259E-HG8145V5-SER2", "SER2", now, "")

	orig := doneSet(t, st, svc, "SER1",
		`{"InternetGatewayDevice.ManagementServer.Password":["oldsecret","xsd:string"]}`)

	url := fmt.Sprintf("%s/v1/devices/SER1/tasks/%d/revert", ts.URL, orig.ID)
	resp := postRevert(t, ts, url, "")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("revert = %d, want 202", resp.StatusCode)
	}
	var body struct {
		Data []struct {
			ID     int64  `json:"id"`
			Name   string `json:"name"`
			Status string `json:"status"`
			Args   struct {
				Values [][]string `json:"values"`
			} `json:"args"`
		} `json:"data"`
	}
	decode(t, resp, &body)
	if len(body.Data) != 1 || body.Data[0].Name != "set" || body.Data[0].Status != "pending" {
		t.Fatalf("revert response = %+v", body.Data)
	}
	revertID := body.Data[0].ID

	// The new task stores the raw value; the device receives it as written.
	got, err := st.GetTask(context.Background(), revertID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got.Args), "oldsecret") {
		t.Errorf("revert args not stored raw: %s", got.Args)
	}
	if strings.Contains(string(got.Args), "enc:") {
		t.Errorf("revert args still sealed: %s", got.Args)
	}
	if body.Data[0].Args.Values[0][1] != "oldsecret" {
		t.Errorf("response args = %+v, want the stored value", body.Data[0].Args.Values)
	}

	// Idempotency-Key replays the same revert task.
	first := postRevert(t, ts, url, "revert-1")
	if first.StatusCode != http.StatusAccepted {
		t.Fatalf("idempotent revert = %d", first.StatusCode)
	}
	var firstBody struct {
		Data []struct {
			ID int64 `json:"id"`
		} `json:"data"`
	}
	decode(t, first, &firstBody)
	second := postRevert(t, ts, url, "revert-1")
	var secondBody struct {
		Data []struct {
			ID int64 `json:"id"`
		} `json:"data"`
	}
	decode(t, second, &secondBody)
	if len(firstBody.Data) != 1 || len(secondBody.Data) != 1 || firstBody.Data[0].ID != secondBody.Data[0].ID {
		t.Errorf("idempotent replays = %+v and %+v", firstBody.Data, secondBody.Data)
	}

	// Wrong device and unknown task both look like a missing task.
	wrong := fmt.Sprintf("%s/v1/devices/SER2/tasks/%d/revert", ts.URL, orig.ID)
	if resp := postRevert(t, ts, wrong, ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("wrong device = %d, want 404", resp.StatusCode)
		resp.Body.Close()
	} else if code := errorCode(t, resp); code != "TASK_NOT_FOUND" {
		t.Errorf("wrong device code = %q", code)
	}
	missing := fmt.Sprintf("%s/v1/devices/SER1/tasks/999999/revert", ts.URL)
	if resp := postRevert(t, ts, missing, ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing task = %d, want 404", resp.StatusCode)
		resp.Body.Close()
	}
	bad := fmt.Sprintf("%s/v1/devices/SER1/tasks/x/revert", ts.URL)
	if resp := postRevert(t, ts, bad, ""); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad task id = %d, want 400", resp.StatusCode)
		resp.Body.Close()
	}

	// A pending set cannot be reverted, a done non-set can not, and a done
	// set without prev can not.
	pending, err := svc.CreateTasks(context.Background(), "SER1", []store.NewTask{{
		Name: "set", Args: json.RawMessage(`{"values":[["InternetGatewayDevice.ManagementServer.Username","x","xsd:string"]]}`),
	}})
	if err != nil {
		t.Fatal(err)
	}
	pendingURL := fmt.Sprintf("%s/v1/devices/SER1/tasks/%d/revert", ts.URL, pending[0].ID)
	if resp := postRevert(t, ts, pendingURL, ""); resp.StatusCode != http.StatusConflict {
		t.Errorf("pending set = %d, want 409", resp.StatusCode)
		resp.Body.Close()
	}

	reboot, err := svc.CreateTasks(context.Background(), "SER1", []store.NewTask{{Name: "reboot"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishTask(context.Background(), reboot[0].ID, "done", nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	rebootURL := fmt.Sprintf("%s/v1/devices/SER1/tasks/%d/revert", ts.URL, reboot[0].ID)
	if resp := postRevert(t, ts, rebootURL, ""); resp.StatusCode != http.StatusConflict {
		t.Errorf("done reboot = %d, want 409", resp.StatusCode)
		resp.Body.Close()
	}

	empty, err := svc.CreateTasks(context.Background(), "SER1", []store.NewTask{{
		Name: "set", Args: json.RawMessage(`{"values":[["InternetGatewayDevice.ManagementServer.Username","y","xsd:string"]]}`),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishTask(context.Background(), empty[0].ID, "done", nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	emptyURL := fmt.Sprintf("%s/v1/devices/SER1/tasks/%d/revert", ts.URL, empty[0].ID)
	if resp := postRevert(t, ts, emptyURL, ""); resp.StatusCode != http.StatusConflict {
		t.Errorf("empty prev = %d, want 409", resp.StatusCode)
		resp.Body.Close()
	}
}

func TestRevertJobAPI(t *testing.T) {
	st := testStore(t)
	_, ts, svc := newTestServer(t, st)
	now := time.Now().UTC()
	seedDevice(t, st, "00259E-HG8145V5-SER1", "SER1", now, "")
	seedDevice(t, st, "00259E-HG8145V5-SER2", "SER2", now, "")

	job, tasks, err := svc.CreateJob(context.Background(), store.NewJob{
		Name:     "set-two",
		Selector: store.JobSelector{Serials: []string{"SER1", "SER2"}},
		Task: store.NewTask{Name: "set", Args: json.RawMessage(
			`{"values":[["InternetGatewayDevice.ManagementServer.Username","new","xsd:string"]]}`)},
		MaxParallel: 1,
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	for _, task := range tasks {
		if err := st.SetTaskPrev(context.Background(), task.ID, json.RawMessage(
			`{"InternetGatewayDevice.ManagementServer.Username":["old","xsd:string"]}`)); err != nil {
			t.Fatal(err)
		}
		if err := st.FinishTask(context.Background(), task.ID, "done", nil, nil, ""); err != nil {
			t.Fatal(err)
		}
	}

	resp := postRevert(t, ts, fmt.Sprintf("%s/v1/jobs/%d/revert", ts.URL, job.ID), "")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("job revert = %d, want 202", resp.StatusCode)
	}
	var body struct {
		Data struct {
			ID          int64          `json:"id"`
			Status      string         `json:"status"`
			MaxParallel int            `json:"max_parallel"`
			Counts      map[string]int `json:"counts"`
		} `json:"data"`
	}
	decode(t, resp, &body)
	if body.Data.ID == 0 || body.Data.ID == job.ID || body.Data.MaxParallel != 1 {
		t.Fatalf("new job = %+v", body.Data)
	}
	if body.Data.Status != "running" || body.Data.Counts["pending"] != 2 || body.Data.Counts["total"] != 2 {
		t.Errorf("new job counts = %+v", body.Data)
	}

	rows, err := st.Pool().Query(context.Background(),
		`SELECT name, status FROM tasks WHERE job_id = $1`, body.Data.ID)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for rows.Next() {
		var name, status string
		if err := rows.Scan(&name, &status); err != nil {
			t.Fatal(err)
		}
		if name != "set" || status != "pending" {
			t.Errorf("revert task = %s %s", name, status)
		}
		n++
	}
	rows.Close()
	if n != 2 {
		t.Errorf("revert tasks = %d, want 2", n)
	}

	// Nothing done and revertable in a reboot job.
	noop, _, err := svc.CreateJob(context.Background(), store.NewJob{
		Name:     "reboot-one",
		Selector: store.JobSelector{Serials: []string{"SER1"}},
		Task:     store.NewTask{Name: "reboot"}, MaxParallel: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	resp = postRevert(t, ts, fmt.Sprintf("%s/v1/jobs/%d/revert", ts.URL, noop.ID), "")
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("empty job revert = %d, want 409", resp.StatusCode)
		resp.Body.Close()
	} else if code := errorCode(t, resp); code != "CONFLICT" {
		t.Errorf("empty job code = %q", code)
	}

	resp = postRevert(t, ts, fmt.Sprintf("%s/v1/jobs/999999/revert", ts.URL), "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing job = %d, want 404", resp.StatusCode)
		resp.Body.Close()
	} else if code := errorCode(t, resp); code != "JOB_NOT_FOUND" {
		t.Errorf("missing job code = %q", code)
	}
}
