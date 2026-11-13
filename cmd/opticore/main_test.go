package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTailLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "opticore.log")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := tailLines(path, 2)
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	if len(got) != 2 || got[0] != "two" || got[1] != "three" {
		t.Fatalf("tail = %q, want the last two lines", got)
	}

	all, err := tailLines(path, 10)
	if err != nil || len(all) != 3 {
		t.Fatalf("tail(10) = %q, %v; want all three lines", all, err)
	}

	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := tailLines(empty, 5); err != nil || len(got) != 0 {
		t.Fatalf("empty file = %q, %v; want nothing", got, err)
	}

	if _, err := tailLines(filepath.Join(dir, "missing"), 5); err == nil {
		t.Fatal("want an error for a missing file")
	}
}
