package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func seedJobDevice(t *testing.T, s *Store, id, serial, class, version string) {
	t.Helper()
	at := time.Now().UTC()
	in := InformRecord{
		ID: id, Serial: serial, OUI: "00259E",
		ProductClass: class, SoftwareVersion: version,
		InformInterval: 300, CRURL: "http://10.20.3.41:7547/" + serial,
		At: at, CurrentTime: at,
	}
	if _, _, err := s.RecordInform(context.Background(), in); err != nil {
		t.Fatalf("RecordInform: %v", err)
	}
}

func TestCreateJobSelector(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	seedJobDevice(t, s, "ID-1", "SER1", "HG8145V5", "V1")
	seedJobDevice(t, s, "ID-2", "SER2", "HG8145V5", "V1")
	seedJobDevice(t, s, "ID-3", "SER3", "HG8245", "V2")

	runAt := time.Now().UTC().Add(30 * time.Minute).Truncate(time.Second)
	job, tasks, err := s.CreateJob(ctx, NewJob{
		Name:        "reboot-A",
		Spec:        []byte(`{"name":"reboot-A"}`),
		Selector:    JobSelector{ProductClass: "HG8145V5"},
		Task:        NewTask{Name: "reboot"},
		MaxParallel: 1,
		RunAt:       runAt,
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("tasks = %d, want 2", len(tasks))
	}
	for _, task := range tasks {
		if task.JobID == nil || *task.JobID != job.ID {
			t.Errorf("task %d job_id = %v, want %d", task.ID, task.JobID, job.ID)
		}
		if task.Status != "pending" || !task.RunAt.Equal(runAt) {
			t.Errorf("task %d = %s at %v, want pending at %v", task.ID, task.Status, task.RunAt, runAt)
		}
	}
	if job.Status != "running" || job.MaxParallel != 1 {
		t.Errorf("job = %+v", job)
	}
	got, err := s.GetJob(ctx, job.ID)
	if err != nil || got.Name != "reboot-A" || string(got.Spec) != `{"name": "reboot-A"}` {
		t.Errorf("GetJob = %+v, %v", got, err)
	}
	counts, err := s.JobCounts(ctx, job.ID)
	if err != nil || counts["pending"] != 2 {
		t.Errorf("JobCounts = %v, %v", counts, err)
	}

	// Explicit serial with the matching class.
	job2, tasks2, err := s.CreateJob(ctx, NewJob{
		Name: "one", Selector: JobSelector{Serials: []string{"SER3"}, ProductClass: "HG8245"},
		Task: NewTask{Name: "reboot"},
	})
	if err != nil {
		t.Fatalf("CreateJob explicit: %v", err)
	}
	if len(tasks2) != 1 || tasks2[0].DeviceID != "ID-3" || tasks2[0].JobID == nil || *tasks2[0].JobID != job2.ID {
		t.Errorf("explicit tasks = %+v", tasks2)
	}

	// A serial filtered out by class leaves an empty selection, not an error
	// about unknown devices.
	if _, _, err := s.CreateJob(ctx, NewJob{
		Name: "mixed", Selector: JobSelector{Serials: []string{"SER1"}, ProductClass: "HG8245"},
		Task: NewTask{Name: "reboot"},
	}); !errors.Is(err, ErrJobNoDevices) {
		t.Errorf("filtered-empty selector error = %v, want ErrJobNoDevices", err)
	}

	// An unknown explicit key fails the whole job and creates no row.
	jobsBefore := count(t, s, `SELECT count(*) FROM jobs`)
	_, _, err = s.CreateJob(ctx, NewJob{
		Name: "bad", Selector: JobSelector{Serials: []string{"SER1", "missing"}},
		Task: NewTask{Name: "reboot"},
	})
	var unknown *UnknownDevicesError
	if !errors.As(err, &unknown) || len(unknown.Keys) != 1 || unknown.Keys[0] != "missing" {
		t.Fatalf("unknown keys error = %v", err)
	}
	if n := count(t, s, `SELECT count(*) FROM jobs`); n != jobsBefore {
		t.Errorf("jobs = %d, want %d after a failed create", n, jobsBefore)
	}

	if _, _, err := s.CreateJob(ctx, NewJob{Name: "none", Task: NewTask{Name: "reboot"}}); !errors.Is(err, ErrJobSelectorEmpty) {
		t.Errorf("empty selector error = %v, want ErrJobSelectorEmpty", err)
	}

	list, err := s.ListJobs(ctx, 10)
	if err != nil || len(list) != 2 || list[0].ID != job2.ID {
		t.Errorf("ListJobs = %+v, %v", list, err)
	}
}

func TestCancelJobAndFinish(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	seedJobDevice(t, s, "ID-1", "SER1", "HG8145V5", "V1")
	seedJobDevice(t, s, "ID-2", "SER2", "HG8145V5", "V1")

	job, tasks, err := s.CreateJob(ctx, NewJob{
		Name: "cancel-me", Selector: JobSelector{ProductClass: "HG8145V5"},
		Task: NewTask{Name: "reboot"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.StartTask(ctx, tasks[0].ID); err != nil {
		t.Fatalf("StartTask: %v", err)
	}
	if err := s.CancelJob(ctx, job.ID); err != nil {
		t.Fatalf("CancelJob: %v", err)
	}
	if got, _ := s.GetJob(ctx, job.ID); got.Status != "cancelled" {
		t.Errorf("job status = %q, want cancelled", got.Status)
	}
	running, _ := s.GetTask(ctx, tasks[0].ID)
	cancelled, _ := s.GetTask(ctx, tasks[1].ID)
	if running.Status != "running" || cancelled.Status != "cancelled" {
		t.Errorf("task statuses = %s, %s, want running, cancelled", running.Status, cancelled.Status)
	}
	if n, err := s.FinishJobs(ctx); err != nil || n != 0 {
		t.Errorf("FinishJobs on cancelled job = %d, %v, want 0", n, err)
	}
	if err := s.CancelJob(ctx, 999999); !errors.Is(err, ErrJobNotFound) {
		t.Errorf("cancel missing job = %v, want ErrJobNotFound", err)
	}

	// A job is done once no task is pending or running; failures are listed.
	job2, tasks2, err := s.CreateJob(ctx, NewJob{
		Name: "finish-me", Selector: JobSelector{ProductClass: "HG8145V5"},
		Task: NewTask{Name: "reboot"},
	})
	if err != nil {
		t.Fatal(err)
	}
	code := 9005
	if err := s.FinishTask(ctx, tasks2[0].ID, "failed", nil, &code, "InvalidParameterValue"); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishTask(ctx, tasks2[1].ID, "done", nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	if n, err := s.FinishJobs(ctx); err != nil || n != 1 {
		t.Fatalf("FinishJobs = %d, %v, want 1", n, err)
	}
	if got, _ := s.GetJob(ctx, job2.ID); got.Status != "done" {
		t.Errorf("job2 status = %q, want done", got.Status)
	}
	failures, err := s.JobFailures(ctx, job2.ID)
	if err != nil || len(failures) != 1 {
		t.Fatalf("JobFailures = %+v, %v", failures, err)
	}
	if failures[0].DeviceID != tasks2[0].DeviceID || failures[0].TaskID != tasks2[0].ID ||
		failures[0].FaultCode == nil || *failures[0].FaultCode != code || failures[0].Fault != "InvalidParameterValue" {
		t.Errorf("failure = %+v", failures[0])
	}
}
