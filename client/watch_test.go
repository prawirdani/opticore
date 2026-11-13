package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func recvEvent(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("event channel closed early")
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for an event")
	}
	return Event{}
}

func TestWatchParsesFrames(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("type"); got != "bootstrap,task" {
			t.Errorf("type = %q", got)
		}
		if got := r.URL.Query().Get("device_id"); got != "d1" {
			t.Errorf("device_id = %q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		fmt.Fprint(w, ": keepalive\n\n")
		fmt.Fprint(w, "id: e1\nevent: bootstrap\ndata: {\"device_id\":\"d1\",\"at\":\"2026-11-13T10:00:00Z\",\"type\":\"bootstrap\",\"oui\":\"00259E\"}\n\n")
		f.Flush()
		fmt.Fprint(w, "id: t7:123\nevent: task\ndata: {\"id\":7,\"name\":\"reboot\",\"status\":\"done\"}\n\n")
		f.Flush()
		fmt.Fprint(w, "id: j3:456\nevent: job\ndata: {\"id\":3,\"name\":\"j\",\"status\":\"done\",\"counts\":{\"total\":2}}\n\n")
		f.Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	c, _ := New(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	events, errs := c.Watch(ctx, WatchFilter{Types: []string{"bootstrap", "task"}, DeviceID: "d1"})

	first := recvEvent(t, events)
	if first.ID != "e1" || first.Kind != "bootstrap" || first.Type != "bootstrap" ||
		first.DeviceID != "d1" || first.Detail["oui"] != "00259E" {
		t.Errorf("inform event = %+v", first)
	}
	if first.At.UTC().Format(time.RFC3339) != "2026-11-13T10:00:00Z" {
		t.Errorf("at = %v", first.At)
	}
	second := recvEvent(t, events)
	if second.ID != "t7:123" || second.Kind != "task" || second.Task == nil || second.Task.ID != 7 {
		t.Errorf("task event = %+v", second)
	}
	third := recvEvent(t, events)
	if third.ID != "j3:456" || third.Kind != "job" || third.Job == nil || third.Job.Counts["total"] != 2 {
		t.Errorf("job event = %+v", third)
	}

	cancel()
	select {
	case err, ok := <-errs:
		if ok && err != nil {
			t.Errorf("watch error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("error channel did not close")
	}
}

func TestWatchReconnectResumes(t *testing.T) {
	var conns atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := conns.Add(1)
		f := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		switch n {
		case 1:
			if got := r.Header.Get("Last-Event-ID"); got != "" {
				t.Errorf("first Last-Event-ID = %q", got)
			}
			fmt.Fprint(w, "id: e1\nevent: bootstrap\ndata: {\"device_id\":\"d1\",\"type\":\"bootstrap\"}\n\n")
			f.Flush()
			// Drop the connection after one frame.
			return
		case 2:
			if got := r.Header.Get("Last-Event-ID"); got != "e1" {
				t.Errorf("second Last-Event-ID = %q, want e1", got)
			}
			fmt.Fprint(w, "id: e2\nevent: bootstrap\ndata: {\"device_id\":\"d1\",\"type\":\"periodic\"}\n\n")
			f.Flush()
			<-r.Context().Done()
		default:
			t.Errorf("unexpected connection %d", n)
			<-r.Context().Done()
		}
	}))
	defer srv.Close()

	c, _ := New(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, _ := c.Watch(ctx, WatchFilter{})

	if ev := recvEvent(t, events); ev.ID != "e1" {
		t.Errorf("first event = %+v", ev)
	}
	if ev := recvEvent(t, events); ev.ID != "e2" {
		t.Errorf("resumed event = %+v", ev)
	}
	if n := conns.Load(); n < 2 {
		t.Errorf("connections = %d, want at least 2", n)
	}
	cancel()
}

func TestWatchTerminalError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"code":"STREAM_GONE","message":"no such stream","details":{}}}`)
	}))
	defer srv.Close()

	c, _ := New(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	events, errs := c.Watch(ctx, WatchFilter{})

	select {
	case _, ok := <-events:
		if ok {
			t.Error("got an event from a 404")
		}
	case <-ctx.Done():
		t.Fatal("events channel did not close")
	}
	select {
	case err, ok := <-errs:
		if !ok || err == nil {
			t.Fatal("no terminal error")
		}
		var apiErr *Error
		if !errors.As(err, &apiErr) || apiErr.Status != 404 || apiErr.Code != "STREAM_GONE" {
			t.Errorf("terminal error = %+v", err)
		}
	case <-ctx.Done():
		t.Fatal("error channel did not deliver")
	}
}
