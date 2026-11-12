package hook

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHandlerAuthAndSteps(t *testing.T) {
	var got Step
	h := Handler("s3cret", func(_ context.Context, s Step) ([]Task, error) {
		got = s
		if s.Step == 1 {
			return []Task{{Name: "set", Args: json.RawMessage(`{"values":[["A","b"]]}`)}}, nil
		}
		return nil, nil
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	c, err := New(srv.URL, "s3cret", time.Second)
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

	// Wrong secret is rejected.
	bad, _ := New(srv.URL, "wrong", time.Second)
	if _, err := bad.Step(context.Background(), in); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v, want 401", err)
	}
}

func TestStepTimeoutAndBadReply(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer slow.Close()
	c, _ := New(slow.URL, "s", 30*time.Millisecond)
	if _, err := c.Step(context.Background(), Step{}); err == nil {
		t.Fatal("timeout not reported")
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer bad.Close()
	c2, _ := New(bad.URL, "s", time.Second)
	if _, err := c2.Step(context.Background(), Step{}); err == nil {
		t.Fatal("bad reply accepted")
	}
}

func TestUnixSocket(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "hook.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("unix socket unsupported here: %v", err)
	}
	srv := &http.Server{Handler: Handler("s", func(context.Context, Step) ([]Task, error) {
		return []Task{{Name: "reboot"}}, nil
	})}
	go srv.Serve(ln)
	defer srv.Close()

	c, err := New("unix:"+sock, "s", time.Second)
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
	for _, tc := range []struct{ url, secret string }{
		{"", "s"},
		{"http://x", ""},
		{"ftp://x", "s"},
		{"unix:", "s"},
	} {
		if _, err := New(tc.url, tc.secret, time.Second); err == nil {
			t.Errorf("New(%q,%q) accepted", tc.url, tc.secret)
		}
	}
}

func TestMatches(t *testing.T) {
	for _, tc := range []struct {
		events, triggers []string
		want             bool
	}{
		{[]string{"0 BOOTSTRAP"}, []string{"0 BOOTSTRAP"}, true},
		{[]string{"0 BOOTSTRAP", "1 BOOT"}, []string{"0"}, true},
		{[]string{"2 PERIODIC"}, []string{"0 BOOTSTRAP"}, false},
		{[]string{"2 PERIODIC"}, nil, false},
	} {
		if got := Matches(tc.events, tc.triggers); got != tc.want {
			t.Errorf("Matches(%v,%v) = %v", tc.events, tc.triggers, got)
		}
	}
}
