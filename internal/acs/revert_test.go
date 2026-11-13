package acs

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prawirdani/opticore/internal/store"
)

const (
	ssidPath = "InternetGatewayDevice.LANDevice.1.WLANConfiguration.1.SSID"
	passPath = "InternetGatewayDevice.ManagementServer.Password"
)

func TestRevertTaskRestoresPrevValues(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	serial := "48575443BD7589AB"
	h.sess.inform(serial, []string{"0 BOOTSTRAP", "1 BOOT"})
	h.sess.run()

	h.sess.params = map[string]string{ssidPath: "OldNet", passPath: "oldpass"}
	if _, err := h.svc.Enqueue(ctx, serial, []store.NewTask{{
		Name: "set", Args: json.RawMessage(`{"values":[["` + ssidPath + `","NewNet","xsd:string"],["` + passPath + `","newpass","xsd:string"]]}`),
	}}); err != nil {
		t.Fatal(err)
	}
	h.sess.inform(serial, []string{"2 PERIODIC"})
	h.sess.run()

	ts, err := h.st.ListTasks(ctx, serial, 10)
	if err != nil || len(ts) != 1 || ts[0].Status != "done" {
		t.Fatalf("tasks = %+v, %v", ts, err)
	}
	orig := ts[0]
	if strings.Contains(string(orig.Prev), "oldpass") || !strings.Contains(string(orig.Prev), encPrefix) {
		t.Fatalf("prev not sealed at rest: %s", orig.Prev)
	}

	out, err := h.svc.RevertTask(ctx, serial, orig.ID, nil)
	if err != nil {
		t.Fatalf("RevertTask: %v", err)
	}
	if len(out) != 1 || out[0].Name != "set" || out[0].Status != "pending" {
		t.Fatalf("revert tasks = %+v", out)
	}
	if strings.Contains(string(out[0].Args), "oldpass") || !strings.Contains(string(out[0].Args), encPrefix) {
		t.Errorf("revert args not sealed at rest: %s", out[0].Args)
	}

	// The device now holds the values the original set wrote.
	h.sess.params[ssidPath], h.sess.params[passPath] = "NewNet", "newpass"
	h.sess.inform(serial, []string{"2 PERIODIC"})
	h.sess.run()

	sets := h.sess.taskSets()
	if len(sets) != 2 {
		t.Fatalf("device saw %d sets, want 2", len(sets))
	}
	got := map[string]string{}
	for _, v := range sets[1] {
		got[v[0]] = v[1]
	}
	if got[ssidPath] != "OldNet" || got[passPath] != "oldpass" {
		t.Errorf("revert sent %v, want the old values", got)
	}
}

func TestRevertTaskRejectsBadTasks(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	serial := "48575443BD7589AB"
	h.sess.inform(serial, []string{"0 BOOTSTRAP", "1 BOOT"})
	h.sess.run()
	h.sess.inform("OTHER0001", []string{"0 BOOTSTRAP", "1 BOOT"})
	h.sess.run()

	setArgs := json.RawMessage(`{"values":[["` + ssidPath + `","x","xsd:string"]]}`)
	pending, err := h.svc.CreateTasks(ctx, serial, []store.NewTask{{Name: "set", Args: setArgs}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.RevertTask(ctx, serial, pending[0].ID, nil); !errors.Is(err, ErrTaskNotDone) {
		t.Errorf("pending revert = %v, want ErrTaskNotDone", err)
	}

	reboot, err := h.svc.CreateTasks(ctx, serial, []store.NewTask{{Name: "reboot"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.FinishTask(ctx, reboot[0].ID, "done", nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.RevertTask(ctx, serial, reboot[0].ID, nil); !errors.Is(err, ErrNotASetTask) {
		t.Errorf("reboot revert = %v, want ErrNotASetTask", err)
	}

	empty, err := h.st.CreateTasks(ctx, serial, []store.NewTask{{Name: "set", Args: setArgs}})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.FinishTask(ctx, empty[0].ID, "done", nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.RevertTask(ctx, serial, empty[0].ID, nil); !errors.Is(err, ErrNoPrev) {
		t.Errorf("empty-prev revert = %v, want ErrNoPrev", err)
	}

	done, err := h.st.CreateTasks(ctx, serial, []store.NewTask{{Name: "set", Args: setArgs}})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.SetTaskPrev(ctx, done[0].ID, json.RawMessage(`{"`+ssidPath+`":["old","xsd:string"]}`)); err != nil {
		t.Fatal(err)
	}
	if err := h.st.FinishTask(ctx, done[0].ID, "done", nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.RevertTask(ctx, "OTHER0001", done[0].ID, nil); !errors.Is(err, store.ErrTaskNotFound) {
		t.Errorf("wrong-device revert = %v, want ErrTaskNotFound", err)
	}
	if _, err := h.svc.RevertTask(ctx, serial, 999999, nil); !errors.Is(err, store.ErrTaskNotFound) {
		t.Errorf("missing-task revert = %v, want ErrTaskNotFound", err)
	}
}

func TestRevertTaskIdempotencyKey(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	serial := "48575443BD7589AB"
	h.sess.inform(serial, []string{"0 BOOTSTRAP", "1 BOOT"})
	h.sess.run()

	orig, err := h.st.CreateTasks(ctx, serial, []store.NewTask{
		{Name: "set", Args: json.RawMessage(`{"values":[["` + ssidPath + `","x","xsd:string"]]}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.SetTaskPrev(ctx, orig[0].ID, json.RawMessage(`{"`+ssidPath+`":["old","xsd:string"]}`)); err != nil {
		t.Fatal(err)
	}
	if err := h.st.FinishTask(ctx, orig[0].ID, "done", nil, nil, ""); err != nil {
		t.Fatal(err)
	}

	key := "revert-once"
	first, err := h.svc.RevertTask(ctx, serial, orig[0].ID, &key)
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.svc.RevertTask(ctx, serial, orig[0].ID, &key)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || len(second) != 1 || first[0].ID != second[0].ID {
		t.Errorf("replays = %+v and %+v, want the same task", first, second)
	}
}

func TestRevertJobKeepsMaxParallel(t *testing.T) {
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
	svc := &Service{Store: st, Key: testKey, Waker: fake, PumpWindow: 20 * time.Millisecond}
	job, tasks, err := svc.CreateJob(ctx, store.NewJob{
		Name:     "set-all",
		Selector: store.JobSelector{Serials: []string{"S1", "S2", "S3"}},
		Task: store.NewTask{Name: "set", Args: json.RawMessage(
			`{"values":[["InternetGatewayDevice.ManagementServer.Username","new","xsd:string"]]}`)},
		MaxParallel: 1,
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if len(tasks) != 3 {
		t.Fatalf("tasks = %d, want 3", len(tasks))
	}
	for _, task := range tasks {
		if err := st.SetTaskPrev(ctx, task.ID, json.RawMessage(
			`{"InternetGatewayDevice.ManagementServer.Username":["old","xsd:string"]}`)); err != nil {
			t.Fatal(err)
		}
		if err := st.FinishTask(ctx, task.ID, "done", nil, nil, ""); err != nil {
			t.Fatal(err)
		}
	}

	revertJob, revertTasks, err := svc.RevertJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("RevertJob: %v", err)
	}
	if revertJob.ID == job.ID || revertJob.MaxParallel != 1 {
		t.Errorf("new job = %+v, want a new job with max_parallel 1", revertJob)
	}
	if len(revertTasks) != 3 {
		t.Fatalf("revert tasks = %d, want 3", len(revertTasks))
	}
	devices := map[string]bool{}
	for _, task := range revertTasks {
		if task.JobID == nil || *task.JobID != revertJob.ID {
			t.Errorf("task %d job = %v, want %d", task.ID, task.JobID, revertJob.ID)
		}
		if task.Name != "set" || !strings.Contains(string(task.Args), "old") {
			t.Errorf("task %d = %s %s, want a set of the old value", task.ID, task.Name, task.Args)
		}
		devices[task.DeviceID] = true
	}
	if len(devices) != 3 {
		t.Errorf("devices = %d, want 3", len(devices))
	}

	if err := svc.PumpJobs(ctx); err != nil {
		t.Fatalf("PumpJobs: %v", err)
	}
	if got := fake.count(); got != 1 {
		t.Errorf("woken = %d, want 1 (the job's max_parallel)", got)
	}

	// A job with nothing revertable is refused instead of creating an empty job.
	noop, _, err := svc.CreateJob(ctx, store.NewJob{
		Name:     "reboot-all",
		Selector: store.JobSelector{Serials: []string{"S1"}},
		Task:     store.NewTask{Name: "reboot"}, MaxParallel: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.RevertJob(ctx, noop.ID); !errors.Is(err, ErrNothingToRevert) {
		t.Errorf("empty job revert = %v, want ErrNothingToRevert", err)
	}
}
