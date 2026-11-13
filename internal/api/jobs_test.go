package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prawirdani/opticore/internal/store"
)

func TestJobsAPI(t *testing.T) {
	st := testStore(t)
	_, ts, _ := newTestServer(t, st)
	now := time.Now().UTC()
	seedDevice(t, st, "00259E-HG8145V5-SER1", "SER1", now, "")
	seedDevice(t, st, "00259E-HG8145V5-SER2", "SER2", now, "")

	body := map[string]any{
		"name":         "reboot-two",
		"task":         map[string]any{"name": "reboot", "args": map[string]any{}},
		"select":       map[string]any{"serials": []string{"SER1", "SER2"}},
		"max_parallel": 1,
	}
	resp := request(t, ts.Client(), http.MethodPost, ts.URL+"/v1/jobs", body)
	if resp.StatusCode != 202 {
		t.Fatalf("POST jobs = %d", resp.StatusCode)
	}
	var created struct {
		Data struct {
			ID     int64          `json:"id"`
			Status string         `json:"status"`
			Counts map[string]int `json:"counts"`
		} `json:"data"`
	}
	decode(t, resp, &created)
	if created.Data.ID == 0 || created.Data.Status != "running" || created.Data.Counts["pending"] != 2 {
		t.Fatalf("created = %+v", created.Data)
	}
	jobID := created.Data.ID

	var detail struct {
		Data struct {
			ID       int64              `json:"id"`
			Status   string             `json:"status"`
			Counts   map[string]int     `json:"counts"`
			Failures []store.JobFailure `json:"failures"`
		} `json:"data"`
	}
	resp = request(t, ts.Client(), http.MethodGet, fmt.Sprintf("%s/v1/jobs/%d", ts.URL, jobID), nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET job = %d", resp.StatusCode)
	}
	decode(t, resp, &detail)
	if detail.Data.ID != jobID || detail.Data.Counts["total"] != 2 || len(detail.Data.Failures) != 0 {
		t.Fatalf("detail = %+v", detail.Data)
	}

	var taskIDs []int64
	rows, err := st.Pool().Query(context.Background(),
		`SELECT id FROM tasks WHERE job_id = $1 ORDER BY id`, jobID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		taskIDs = append(taskIDs, id)
	}
	rows.Close()
	if len(taskIDs) != 2 {
		t.Fatalf("task ids = %v", taskIDs)
	}
	code := 9005
	if err := st.FinishTask(context.Background(), taskIDs[0], "failed", nil, &code, "InvalidParameterValue"); err != nil {
		t.Fatal(err)
	}

	resp = request(t, ts.Client(), http.MethodGet, fmt.Sprintf("%s/v1/jobs/%d", ts.URL, jobID), nil)
	decode(t, resp, &detail)
	if len(detail.Data.Failures) != 1 {
		t.Fatalf("failures = %+v", detail.Data.Failures)
	}
	f := detail.Data.Failures[0]
	if f.TaskID != taskIDs[0] || f.FaultCode == nil || *f.FaultCode != code ||
		f.Fault != "InvalidParameterValue" || !strings.HasPrefix(f.DeviceID, "00259E-HG8145V5-") {
		t.Errorf("failure = %+v", f)
	}

	var list struct {
		Data []struct {
			ID     int64          `json:"id"`
			Counts map[string]int `json:"counts"`
		} `json:"data"`
	}
	resp = request(t, ts.Client(), http.MethodGet, ts.URL+"/v1/jobs", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET jobs = %d", resp.StatusCode)
	}
	decode(t, resp, &list)
	if len(list.Data) != 1 || list.Data[0].ID != jobID || list.Data[0].Counts["failed"] != 1 {
		t.Errorf("list = %+v", list.Data)
	}

	// Cancel leaves the failed task alone and cancels the pending one.
	resp = request(t, ts.Client(), http.MethodDelete, fmt.Sprintf("%s/v1/jobs/%d", ts.URL, jobID), nil)
	if resp.StatusCode != 204 {
		t.Fatalf("DELETE job = %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = request(t, ts.Client(), http.MethodGet, fmt.Sprintf("%s/v1/jobs/%d", ts.URL, jobID), nil)
	decode(t, resp, &detail)
	if detail.Data.Status != "cancelled" || detail.Data.Counts["cancelled"] != 1 || detail.Data.Counts["failed"] != 1 {
		t.Errorf("cancelled job = %+v", detail.Data)
	}

	// Unknown serial: 400 listing it, and no extra job row.
	bad := map[string]any{
		"name": "bad", "task": map[string]any{"name": "reboot"},
		"select": map[string]any{"serials": []string{"SER1", "NOPE"}},
	}
	resp = request(t, ts.Client(), http.MethodPost, ts.URL+"/v1/jobs", bad)
	if resp.StatusCode != 400 {
		t.Fatalf("unknown serial = %d, want 400", resp.StatusCode)
	}
	var errBody struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	decode(t, resp, &errBody)
	if errBody.Error.Code != "VALIDATION" || !strings.Contains(errBody.Error.Message, "NOPE") {
		t.Errorf("unknown serial body = %+v", errBody.Error)
	}
	resp = request(t, ts.Client(), http.MethodGet, ts.URL+"/v1/jobs", nil)
	decode(t, resp, &list)
	if len(list.Data) != 1 {
		t.Errorf("jobs after failed create = %d, want 1", len(list.Data))
	}

	resp = request(t, ts.Client(), http.MethodPost, ts.URL+"/v1/jobs",
		map[string]any{"name": "empty", "task": map[string]any{"name": "reboot"}})
	if resp.StatusCode != 400 {
		t.Errorf("empty selector = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	resp = request(t, ts.Client(), http.MethodGet, ts.URL+"/v1/jobs/999999", nil)
	if resp.StatusCode != 404 || errorCode(t, resp) != "JOB_NOT_FOUND" {
		t.Errorf("missing job = %d", resp.StatusCode)
	}
	resp = request(t, ts.Client(), http.MethodDelete, ts.URL+"/v1/jobs/999999", nil)
	if resp.StatusCode != 404 || errorCode(t, resp) != "JOB_NOT_FOUND" {
		t.Errorf("cancel missing job = %d", resp.StatusCode)
	}
}
