package hook

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/prawirdani/opticore/client"
)

// The hook POST carries no credentials and no Authorization header.
func TestStepsAndHeaders(t *testing.T) {
	var got Step
	h := client.HookHandler(func(_ context.Context, s Step) ([]Task, error) {
		got = s
		if s.Step == 1 {
			return []Task{{Name: "set", Args: json.RawMessage(`{"values":[["A","b"]]}`)}}, nil
		}
		return nil, nil
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	c, err := New(srv.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	in := Step{Step: 1, Device: Device{ID: "d", Serial: "s"}, Events: []string{"0 BOOTSTRAP"},
		Params: map[string]Param{"a": {Value: "v"}}}
	reply, err := c.Step(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(reply.Tasks) != 1 || reply.Tasks[0].Name != "set" {
		t.Fatalf("reply = %+v", reply)
	}
	if got.Device.Serial != "s" || got.Params["a"].Value != "v" || got.Events[0] != "0 BOOTSTRAP" {
		t.Errorf("handler saw %+v", got)
	}

	var hdr http.Header
	raw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr = r.Header.Clone()
		w.Write([]byte(`{"tasks":[{"name":"reboot"}]}`))
	}))
	defer raw.Close()
	c2, err := New(raw.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c2.Step(context.Background(), Step{}); err != nil {
		t.Fatal(err)
	}
	if v := hdr.Get("Authorization"); v != "" {
		t.Errorf("hook POST sent Authorization %q, want none", v)
	}
	if v := hdr.Get("Content-Type"); v != "application/json" {
		t.Errorf("hook POST Content-Type = %q", v)
	}
}

func TestStepTimeoutAndBadReply(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer slow.Close()
	c, _ := New(slow.URL, 30*time.Millisecond)
	if _, err := c.Step(context.Background(), Step{}); err == nil {
		t.Fatal("timeout not reported")
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer bad.Close()
	c2, _ := New(bad.URL, time.Second)
	if _, err := c2.Step(context.Background(), Step{}); err == nil {
		t.Fatal("bad reply accepted")
	}

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer failing.Close()
	c3, _ := New(failing.URL, time.Second)
	if _, err := c3.Step(context.Background(), Step{}); err == nil {
		t.Fatal("non-2xx accepted")
	}
}

func TestUnixSocket(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "hook.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("unix socket unsupported here: %v", err)
	}
	srv := &http.Server{Handler: client.HookHandler(func(context.Context, Step) ([]Task, error) {
		return []Task{{Name: "reboot"}}, nil
	})}
	go srv.Serve(ln)
	defer srv.Close()

	c, err := New("unix:"+sock, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	reply, err := c.Step(context.Background(), Step{Step: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(reply.Tasks) != 1 || reply.Tasks[0].Name != "reboot" {
		t.Fatalf("reply = %+v", reply)
	}
}

func TestNewValidation(t *testing.T) {
	for _, url := range []string{
		"",
		"ftp://x",
		"unix:",
	} {
		if _, err := New(url, time.Second); err == nil {
			t.Errorf("New(%q) accepted", url)
		}
	}
	for _, url := range []string{"http://x", "https://x", "unix:/run/h.sock"} {
		if _, err := New(url, time.Second); err != nil {
			t.Errorf("New(%q): %v", url, err)
		}
	}
}
