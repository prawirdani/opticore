package store

import (
	"context"
	"testing"
	"time"
)

// jobs.updated_at is the SSE job cursor: creation, a terminal task, FinishJobs
// and CancelJob all move it.
func TestJobUpdatedAtCursor(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	seedJobDevice(t, s, "ID-1", "SER1", "HG8145V5", "V1")

	jobA, tasksA, err := s.CreateJob(ctx, NewJob{
		Name: "A", Selector: JobSelector{Serials: []string{"SER1"}},
		Task: NewTask{Name: "reboot"}, MaxParallel: 1,
	})
	if err != nil {
		t.Fatalf("CreateJob A: %v", err)
	}
	if jobA.UpdatedAt.IsZero() {
		t.Fatal("jobA.UpdatedAt is zero")
	}
	time.Sleep(2 * time.Millisecond)

	jobB, _, err := s.CreateJob(ctx, NewJob{
		Name: "B", Selector: JobSelector{Serials: []string{"SER1"}},
		Task: NewTask{Name: "reboot"}, MaxParallel: 1,
	})
	if err != nil {
		t.Fatalf("CreateJob B: %v", err)
	}
	if !jobB.UpdatedAt.After(jobA.UpdatedAt) {
		t.Fatalf("jobB.UpdatedAt %v not after jobA %v", jobB.UpdatedAt, jobA.UpdatedAt)
	}

	jobs, err := s.JobsUpdatedSince(ctx, jobA.UpdatedAt, jobA.ID, 10)
	if err != nil {
		t.Fatalf("JobsUpdatedSince: %v", err)
	}
	if len(jobs) != 1 || jobs[0].ID != jobB.ID {
		t.Fatalf("after jobA = %+v, want only jobB", jobs)
	}
	jobs, err = s.JobsUpdatedSince(ctx, jobA.UpdatedAt.Add(-time.Hour), -1, 10)
	if err != nil {
		t.Fatalf("JobsUpdatedSince all: %v", err)
	}
	if len(jobs) != 2 || jobs[0].ID != jobA.ID || jobs[1].ID != jobB.ID {
		t.Fatalf("all jobs = %+v, want A then B", jobs)
	}

	// A terminal task touches its job's row through the 00004 trigger.
	time.Sleep(2 * time.Millisecond)
	if err := s.FinishTask(ctx, tasksA[0].ID, "done", nil, nil, ""); err != nil {
		t.Fatalf("FinishTask: %v", err)
	}
	got, err := s.GetJob(ctx, jobA.ID)
	if err != nil {
		t.Fatalf("GetJob A: %v", err)
	}
	if !got.UpdatedAt.After(jobB.UpdatedAt) {
		t.Fatalf("after terminal task UpdatedAt %v not after jobB %v", got.UpdatedAt, jobB.UpdatedAt)
	}
	jobs, err = s.JobsUpdatedSince(ctx, jobB.UpdatedAt, jobB.ID, 10)
	if err != nil {
		t.Fatalf("JobsUpdatedSince after task: %v", err)
	}
	if len(jobs) != 1 || jobs[0].ID != jobA.ID {
		t.Fatalf("after terminal task = %+v, want jobA", jobs)
	}
	taskUpdated := jobs[0].UpdatedAt

	// FinishJobs moves a running job to done.
	time.Sleep(2 * time.Millisecond)
	if n, err := s.FinishJobs(ctx); err != nil || n != 1 {
		t.Fatalf("FinishJobs = %d, %v, want 1, nil", n, err)
	}
	got, err = s.GetJob(ctx, jobA.ID)
	if err != nil {
		t.Fatalf("GetJob A: %v", err)
	}
	if got.Status != "done" {
		t.Fatalf("jobA status = %q, want done", got.Status)
	}
	if !got.UpdatedAt.After(taskUpdated) {
		t.Fatalf("after FinishJobs UpdatedAt %v not after %v", got.UpdatedAt, taskUpdated)
	}

	// CancelJob moves a job to cancelled.
	time.Sleep(2 * time.Millisecond)
	if err := s.CancelJob(ctx, jobB.ID); err != nil {
		t.Fatalf("CancelJob B: %v", err)
	}
	gotB, err := s.GetJob(ctx, jobB.ID)
	if err != nil {
		t.Fatalf("GetJob B: %v", err)
	}
	if gotB.Status != "cancelled" {
		t.Fatalf("jobB status = %q, want cancelled", gotB.Status)
	}
	if !gotB.UpdatedAt.After(jobB.UpdatedAt) {
		t.Fatalf("after CancelJob UpdatedAt %v not after %v", gotB.UpdatedAt, jobB.UpdatedAt)
	}
}
