package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prawirdani/opticore/client"
)

// The SDK builders must survive the real validator, not just the pure JSON
// tests in client/.
func TestSDKTaskBuildersAccepted(t *testing.T) {
	st := testStore(t)
	_, ts, _ := newTestServer(t, st)
	seedDevice(t, st, "00259E-HG8145V5-SDK1", "SDK1", time.Now().UTC(), "")

	c, err := client.New(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		task client.Task
	}{
		{"get", client.Get("InternetGatewayDevice.DeviceInfo.")},
		{"set", client.Set(client.Value{
			Path:  "InternetGatewayDevice.DeviceInfo.Description",
			Value: "sdk", Type: "xsd:string",
		})},
		{"add_object", client.AddObject("InternetGatewayDevice.WANDevice.1.WANConnectionDevice.1.WANPPPConnection.")},
		{"delete_object", client.DeleteObject("InternetGatewayDevice.WANDevice.1.WANConnectionDevice.1.WANPPPConnection.1.")},
		{"reboot", client.Reboot()},
		{"reset", client.Reset()},
		{"ping", client.Ping("8.8.8.8", client.PingCount(2), client.PingTimeout(time.Second), client.PingSize(64))},
	} {
		tasks, err := c.CreateTasks(ctx, "SDK1", []client.Task{tc.task}, "")
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if len(tasks) != 1 || tasks[0].Name != tc.name || tasks[0].ID == 0 {
			t.Errorf("%s: created %+v", tc.name, tasks)
		}
	}

	// A batch keeps its order, and the idempotency key replays the same rows.
	batch := []client.Task{client.Reboot(), client.Get()}
	first, err := c.CreateTasks(ctx, "SDK1", batch, "sdk-batch")
	if err != nil || len(first) != 2 {
		t.Fatalf("batch = %+v, %v", first, err)
	}
	replay, err := c.CreateTasks(ctx, "SDK1", batch, "sdk-batch")
	if err != nil || len(replay) != 2 {
		t.Fatalf("replay = %+v, %v", replay, err)
	}
	if first[0].ID != replay[0].ID || first[1].ID != replay[1].ID {
		t.Errorf("replay ids = %+v, want %+v", replay, first)
	}
}

// A Watch resumed from the start of the task table receives a task frame
// queued through the SDK.
func TestSDKWatchReceivesTask(t *testing.T) {
	st := testStore(t)
	_, ts, _ := newTestServer(t, st)
	seedDevice(t, st, "00259E-HG8145V5-SDK2", "SDK2", time.Now().UTC(), "")

	c, err := client.New(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	events, errs := c.Watch(ctx, client.WatchFilter{
		Types:       []string{"task"},
		DeviceID:    "00259E-HG8145V5-SDK2",
		LastEventID: "t0:0",
	})
	if _, err := c.CreateTasks(ctx, "SDK2", []client.Task{client.Reboot()}, ""); err != nil {
		t.Fatalf("CreateTasks: %v", err)
	}
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatal("watch closed before a task event")
			}
			if ev.Kind != "task" || ev.Task == nil {
				continue
			}
			if !strings.HasPrefix(ev.ID, "t") || ev.Task.Name != "reboot" {
				t.Errorf("task event = id %q, task %+v", ev.ID, ev.Task)
			}
			return
		case err, ok := <-errs:
			if ok && err != nil {
				t.Fatalf("watch error: %v", err)
			}
			t.Fatal("watch closed before a task event")
		case <-ctx.Done():
			t.Fatal("timed out waiting for a task event")
		}
	}
}
