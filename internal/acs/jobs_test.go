package acs

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/prawirdani/opticore/internal/store"
	"github.com/prawirdani/opticore/internal/wake"
)

type countingWaker struct {
	mu        sync.Mutex
	requested []wake.Target
}

func (w *countingWaker) Request(t wake.Target) {
	w.mu.Lock()
	w.requested = append(w.requested, t)
	w.mu.Unlock()
}

func (w *countingWaker) Wake(context.Context, wake.Target) error { return nil }

func (w *countingWaker) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.requested)
}

func TestPumpJobsHonoursMaxParallel(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for _, serial := range []string{"S1", "S2", "S3"} {
		in := store.InformRecord{
			ID: "ID-" + serial, Serial: serial, OUI: "00259E",
			ProductClass: "HG8145V5", SoftwareVersion: "V1", InformInterval: 300,
			CRURL: "http://10.20.3.41:7547/" + serial, At: now, CurrentTime: now,
		}
		if _, _, err := st.RecordInform(ctx, in); err != nil {
			t.Fatalf("RecordInform: %v", err)
		}
	}

	fake := &countingWaker{}
	svc := &Service{Store: st, Waker: fake, PumpWindow: 20 * time.Millisecond}
	job, tasks, err := svc.CreateJob(ctx, store.NewJob{
		Name:     "reboot-all",
		Selector: store.JobSelector{Serials: []string{"S1", "S2", "S3"}},
		Task:     store.NewTask{Name: "reboot"}, MaxParallel: 1,
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if len(tasks) != 3 {
		t.Fatalf("tasks = %d, want 3", len(tasks))
	}

	if err := svc.PumpJobs(ctx); err != nil {
		t.Fatalf("PumpJobs: %v", err)
	}
	if got := fake.count(); got != 1 {
		t.Fatalf("woken = %d, want 1 (max_parallel)", got)
	}
	// The memory of the woken task holds the cap until the window passes.
	if err := svc.PumpJobs(ctx); err != nil {
		t.Fatalf("PumpJobs again: %v", err)
	}
	if got := fake.count(); got != 1 {
		t.Fatalf("woken after second pump = %d, want 1", got)
	}

	// The first device finishes; after the window the next one is woken.
	if err := st.FinishTask(ctx, tasks[0].ID, "done", nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	if err := svc.PumpJobs(ctx); err != nil {
		t.Fatalf("PumpJobs after finish: %v", err)
	}
	if got := fake.count(); got != 2 {
		t.Fatalf("woken = %d, want 2 after one task finished", got)
	}

	for _, task := range tasks[1:] {
		if err := st.FinishTask(ctx, task.ID, "done", nil, nil, ""); err != nil {
			t.Fatalf("FinishTask: %v", err)
		}
	}
	if err := svc.PumpJobs(ctx); err != nil {
		t.Fatalf("PumpJobs final: %v", err)
	}
	got, err := st.GetJob(ctx, job.ID)
	if err != nil || got.Status != "done" {
		t.Errorf("job = %+v, %v, want done", got, err)
	}
}
