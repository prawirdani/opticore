package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prawirdani/opticore/internal/store"
)

type sseJobView struct {
	ID          int64              `json:"id"`
	Name        string             `json:"name"`
	Status      string             `json:"status"`
	MaxParallel int                `json:"max_parallel"`
	CreatedAt   time.Time          `json:"created_at"`
	Counts      map[string]int     `json:"counts"`
	Failures    []store.JobFailure `json:"failures"`
}

// connectSSE opens the stream and returns its reader. Cancel closes the body.
func connectSSE(t *testing.T, ts *httptest.Server, query, lastID string) (*http.Response, *bufio.Reader, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	u := ts.URL + "/v1/events"
	if query != "" {
		u += "?" + query
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		cancel()
		t.Fatalf("new SSE request: %v", err)
	}
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		cancel()
		t.Fatalf("connect SSE: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		cancel()
		resp.Body.Close()
		t.Fatalf("SSE status = %d", resp.StatusCode)
	}
	return resp, bufio.NewReader(resp.Body), cancel
}

func readJobEvent(t *testing.T, br *bufio.Reader) (sseFrame, sseJobView) {
	t.Helper()
	for {
		f, err := readSSE(br)
		if err != nil {
			t.Fatalf("read SSE: %v", err)
		}
		if f.event != "job" {
			continue
		}
		var v sseJobView
		if err := json.Unmarshal([]byte(f.data), &v); err != nil {
			t.Fatalf("job data: %v", err)
		}
		return f, v
	}
}

// readSSEWithin reports whether another frame arrives within d. The read is
// abandoned on timeout; the caller closes the body.
func readSSEWithin(br *bufio.Reader, d time.Duration) (sseFrame, bool) {
	ch := make(chan sseFrame, 1)
	go func() {
		if f, err := readSSE(br); err == nil {
			ch <- f
		}
	}()
	select {
	case f := <-ch:
		return f, true
	case <-time.After(d):
		return sseFrame{}, false
	}
}

func createSSEJob(t *testing.T, st *store.Store, serials ...string) (store.Job, []store.Task) {
	t.Helper()
	job, tasks, err := st.CreateJob(context.Background(), store.NewJob{
		Name:     "sse-job",
		Selector: store.JobSelector{Serials: serials},
		Task:     store.NewTask{Name: "reboot"},
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	return job, tasks
}

func TestSSEJobTerminalTaskCounts(t *testing.T) {
	st := testStore(t)
	_, ts, _ := newTestServer(t, st)
	seedDevice(t, st, "00259E-HG8145V5-SER1", "SER1", time.Now().UTC(), "")
	seedDevice(t, st, "00259E-HG8145V5-SER2", "SER2", time.Now().UTC(), "")
	job, tasks := createSSEJob(t, st, "SER1", "SER2")

	resp, br, cancel := connectSSE(t, ts, "type=job", "")
	defer cancel()
	defer resp.Body.Close()

	if err := st.FinishTask(context.Background(), tasks[0].ID, "done", nil, nil, ""); err != nil {
		t.Fatalf("FinishTask: %v", err)
	}
	f, v := readJobEvent(t, br)
	if v.ID != job.ID {
		t.Fatalf("job id = %d, want %d", v.ID, job.ID)
	}
	if v.Status != "running" {
		t.Errorf("status = %q, want running", v.Status)
	}
	if v.Counts["done"] != 1 || v.Counts["total"] != 2 || v.Counts["pending"] != 1 {
		t.Errorf("counts = %v, want done=1 pending=1 total=2", v.Counts)
	}
	if !strings.HasPrefix(f.id, "j"+strconv.FormatInt(job.ID, 10)+":") {
		t.Errorf("job event id = %q", f.id)
	}
}

func TestSSEJobCancelStatus(t *testing.T) {
	st := testStore(t)
	_, ts, _ := newTestServer(t, st)
	seedDevice(t, st, "00259E-HG8145V5-SER1", "SER1", time.Now().UTC(), "")
	job, _ := createSSEJob(t, st, "SER1")

	resp, br, cancel := connectSSE(t, ts, "type=job", "")
	defer cancel()
	defer resp.Body.Close()

	if err := st.CancelJob(context.Background(), job.ID); err != nil {
		t.Fatalf("CancelJob: %v", err)
	}
	_, v := readJobEvent(t, br)
	if v.ID != job.ID || v.Status != "cancelled" {
		t.Fatalf("job = %+v, want %d cancelled", v, job.ID)
	}
	if v.Counts["cancelled"] != 1 || v.Counts["total"] != 1 {
		t.Errorf("counts = %v, want cancelled=1 total=1", v.Counts)
	}
}

func TestSSEJobFinishStatus(t *testing.T) {
	st := testStore(t)
	_, ts, _ := newTestServer(t, st)
	seedDevice(t, st, "00259E-HG8145V5-SER1", "SER1", time.Now().UTC(), "")
	job, tasks := createSSEJob(t, st, "SER1")
	if err := st.FinishTask(context.Background(), tasks[0].ID, "done", nil, nil, ""); err != nil {
		t.Fatalf("FinishTask: %v", err)
	}

	// Connect after the task finished, so the count event is not replayed.
	resp, br, cancel := connectSSE(t, ts, "type=job", "")
	defer cancel()
	defer resp.Body.Close()

	if n, err := st.FinishJobs(context.Background()); err != nil || n != 1 {
		t.Fatalf("FinishJobs = %d, %v", n, err)
	}
	_, v := readJobEvent(t, br)
	if v.ID != job.ID || v.Status != "done" {
		t.Fatalf("job = %+v, want %d done", v, job.ID)
	}
	if v.Counts["done"] != 1 || v.Counts["total"] != 1 {
		t.Errorf("counts = %v, want done=1 total=1", v.Counts)
	}
}

func TestSSEJobResume(t *testing.T) {
	st := testStore(t)
	_, ts, _ := newTestServer(t, st)
	seedDevice(t, st, "00259E-HG8145V5-SER1", "SER1", time.Now().UTC(), "")
	seedDevice(t, st, "00259E-HG8145V5-SER2", "SER2", time.Now().UTC(), "")
	job, tasks := createSSEJob(t, st, "SER1", "SER2")

	resp, br, cancel := connectSSE(t, ts, "type=job", "")
	defer cancel()
	defer resp.Body.Close()

	if err := st.FinishTask(context.Background(), tasks[0].ID, "done", nil, nil, ""); err != nil {
		t.Fatalf("FinishTask 1: %v", err)
	}
	first, v1 := readJobEvent(t, br)
	if v1.Counts["done"] != 1 {
		t.Fatalf("first counts = %v, want done=1", v1.Counts)
	}

	time.Sleep(3 * time.Millisecond)
	if err := st.FinishTask(context.Background(), tasks[1].ID, "done", nil, nil, ""); err != nil {
		t.Fatalf("FinishTask 2: %v", err)
	}
	second, v2 := readJobEvent(t, br)
	if v2.Counts["done"] != 2 {
		t.Fatalf("second counts = %v, want done=2", v2.Counts)
	}

	// Resume after the first job event: the second snapshot must arrive, the
	// first must not be replayed.
	resp2, br2, cancel2 := connectSSE(t, ts, "type=job", first.id)
	defer cancel2()
	defer resp2.Body.Close()

	resumed, v3 := readJobEvent(t, br2)
	if v3.Counts["done"] != 2 {
		t.Errorf("resumed counts = %v, want done=2 (older event replayed)", v3.Counts)
	}
	if resumed.id != second.id {
		t.Errorf("resumed id = %q, want %q", resumed.id, second.id)
	}

	// And a newer status change is not missed either.
	if n, err := st.FinishJobs(context.Background()); err != nil || n != 1 {
		t.Fatalf("FinishJobs = %d, %v", n, err)
	}
	_, v4 := readJobEvent(t, br2)
	if v4.ID != job.ID || v4.Status != "done" {
		t.Fatalf("after resume job = %+v, want %d done", v4, job.ID)
	}
}

func TestSSEOldCursorFormats(t *testing.T) {
	st := testStore(t)
	_, ts, _ := newTestServer(t, st)
	seedDevice(t, st, "00259E-HG8145V5-SER1", "SER1", time.Now().Add(-time.Minute).UTC(), "")
	job, tasks := createSSEJob(t, st, "SER1")
	if err := st.FinishTask(context.Background(), tasks[0].ID, "done", nil, nil, ""); err != nil {
		t.Fatalf("FinishTask: %v", err)
	}

	var eventID int64
	if err := st.Pool().QueryRow(context.Background(),
		`SELECT max(id) FROM events`).Scan(&eventID); err != nil {
		t.Fatalf("max event id: %v", err)
	}
	resp, br, cancel := connectSSE(t, ts, "type=job", "e"+strconv.FormatInt(eventID, 10))
	defer cancel()
	defer resp.Body.Close()
	_, v := readJobEvent(t, br)
	if v.ID != job.ID {
		t.Fatalf("e cursor job = %d, want %d", v.ID, job.ID)
	}

	task, err := st.GetTask(context.Background(), tasks[0].ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	oldTask := "t" + strconv.FormatInt(task.ID, 10) + ":" +
		strconv.FormatInt(task.UpdatedAt.UnixNano(), 10)
	resp2, br2, cancel2 := connectSSE(t, ts, "type=job", oldTask)
	defer cancel2()
	defer resp2.Body.Close()
	_, v2 := readJobEvent(t, br2)
	if v2.ID != job.ID {
		t.Fatalf("t cursor job = %d, want %d", v2.ID, job.ID)
	}
}

func TestSSETypeFilter(t *testing.T) {
	st := testStore(t)
	_, ts, _ := newTestServer(t, st)
	seedDevice(t, st, "00259E-HG8145V5-SER1", "SER1", time.Now().UTC(), "")

	// type=task: the terminal task is emitted, its job event is not.
	_, tasks := createSSEJob(t, st, "SER1")
	resp, br, cancel := connectSSE(t, ts, "type=task", "")
	if err := st.FinishTask(context.Background(), tasks[0].ID, "done", nil, nil, ""); err != nil {
		cancel()
		resp.Body.Close()
		t.Fatalf("FinishTask: %v", err)
	}
	f, err := readSSE(br)
	if err != nil {
		cancel()
		resp.Body.Close()
		t.Fatalf("read task: %v", err)
	}
	if f.event != "task" {
		t.Errorf("type=task first event = %q, want task", f.event)
	}
	if leaked, ok := readSSEWithin(br, time.Second); ok && leaked.event == "job" {
		t.Errorf("type=task leaked a job event")
	}
	cancel()
	resp.Body.Close()

	// type=job: only the job event arrives, even though a task also moved.
	job, tasks2 := createSSEJob(t, st, "SER1")
	resp2, br2, cancel2 := connectSSE(t, ts, "type=job", "")
	defer cancel2()
	defer resp2.Body.Close()
	if err := st.FinishTask(context.Background(), tasks2[0].ID, "done", nil, nil, ""); err != nil {
		t.Fatalf("FinishTask 2: %v", err)
	}
	f2, err := readSSE(br2)
	if err != nil {
		t.Fatalf("read job: %v", err)
	}
	if f2.event != "job" {
		t.Fatalf("type=job first event = %q, want job", f2.event)
	}
	var v sseJobView
	if err := json.Unmarshal([]byte(f2.data), &v); err != nil {
		t.Fatalf("job data: %v", err)
	}
	if v.ID != job.ID {
		t.Errorf("job id = %d, want %d", v.ID, job.ID)
	}
}
