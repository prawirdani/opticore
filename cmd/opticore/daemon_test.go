package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestPidFilePath(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	if got := pidFile(env(map[string]string{"OPTICORE_PID_FILE": "/run/o.pid"})); got != "/run/o.pid" {
		t.Fatalf("explicit pid file = %q", got)
	}
	if got := pidFile(env(map[string]string{"OPTICORE_LOG_FILE": "/var/log/o.log"})); got != "/var/log/o.log.pid" {
		t.Fatalf("log-derived pid file = %q", got)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(filepath.Dir(exe), "opticore.pid")
	if got := pidFile(env(nil)); got != want {
		t.Fatalf("default pid file = %q, want %q", got, want)
	}
}

func TestPidRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "opticore.pid")
	if err := writePID(path); err != nil {
		t.Fatalf("write: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(b)) != strconv.Itoa(os.Getpid()) {
		t.Fatalf("pid file holds %q", b)
	}
	if pid := livePID(path); pid != os.Getpid() {
		t.Fatalf("livePID = %d, want self", pid)
	}
	removePID(path)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("pid file survived removePID")
	}

	// A file naming another process is left alone.
	other := filepath.Join(t.TempDir(), "other.pid")
	if err := os.WriteFile(other, []byte("999999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	removePID(other)
	if livePID(other) != 0 {
		t.Fatal("a dead pid must not be reported as live")
	}
}

func TestIsServeArgs(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{[]string{"/usr/local/bin/opticore", "serve"}, true},
		{[]string{"./bin/opticore", "serve", "-d"}, true},
		{[]string{"C:\\opticore.exe", "serve"}, true},
		{[]string{"/usr/local/bin/opticore", "tui"}, false},
		{[]string{"/usr/local/bin/opticore", "stop"}, false},
		{[]string{"/usr/local/bin/opticore"}, false},
		{[]string{"/tmp/go-build/opticore.test", "serve"}, false},
		{nil, false},
	}
	for _, c := range cases {
		if got := isServeArgs(c.args); got != c.want {
			t.Errorf("isServeArgs(%q) = %v, want %v", c.args, got, c.want)
		}
	}
	if isServe(os.Getpid()) {
		t.Fatal("the test binary must not look like an opticore serve")
	}
}
