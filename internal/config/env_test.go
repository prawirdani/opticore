package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeEnv(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, ".env")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func exeErr() (string, error) { return "", errors.New("no executable") }

func TestLoadEnvFileExplicit(t *testing.T) {
	dir := t.TempDir()
	writeEnv(t, dir, "OPTICORE_TEST_EXPLICIT=one\n")
	t.Setenv("OPTICORE_ENV_FILE", "")
	p := filepath.Join(dir, ".env")

	got, err := loadEnvFile(p, exeErr)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != p {
		t.Fatalf("path = %q, want %q", got, p)
	}
	if v := os.Getenv("OPTICORE_TEST_EXPLICIT"); v != "one" {
		t.Fatalf("OPTICORE_TEST_EXPLICIT = %q, want one", v)
	}
}

func TestLoadEnvFileExplicitMissing(t *testing.T) {
	if _, err := loadEnvFile(filepath.Join(t.TempDir(), "nope"), exeErr); err == nil {
		t.Fatal("want an error for a missing explicit file")
	}
}

func TestLoadEnvFileEnvVarMissing(t *testing.T) {
	t.Setenv("OPTICORE_ENV_FILE", filepath.Join(t.TempDir(), "nope"))
	if _, err := loadEnvFile("", exeErr); err == nil {
		t.Fatal("want an error for a missing $OPTICORE_ENV_FILE")
	}
}

func TestLoadEnvFileDefaultsToCwd(t *testing.T) {
	t.Setenv("OPTICORE_ENV_FILE", "")
	dir := t.TempDir()
	writeEnv(t, dir, "OPTICORE_TEST_CWD=here\n")
	t.Chdir(dir)

	got, err := loadEnvFile("", exeErr)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != ".env" {
		t.Fatalf("path = %q, want .env", got)
	}
	if v := os.Getenv("OPTICORE_TEST_CWD"); v != "here" {
		t.Fatalf("OPTICORE_TEST_CWD = %q, want here", v)
	}
}

func TestLoadEnvFileFallsBackToExecutableDir(t *testing.T) {
	t.Setenv("OPTICORE_ENV_FILE", "")
	bin := t.TempDir()
	writeEnv(t, bin, "OPTICORE_TEST_EXE=there\n")
	t.Chdir(t.TempDir()) // empty working directory

	exe := func() (string, error) { return filepath.Join(bin, "opticore"), nil }
	got, err := loadEnvFile("", exe)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != filepath.Join(bin, ".env") {
		t.Fatalf("path = %q", got)
	}
	if v := os.Getenv("OPTICORE_TEST_EXE"); v != "there" {
		t.Fatalf("OPTICORE_TEST_EXE = %q, want there", v)
	}
}

func TestLoadEnvFileNone(t *testing.T) {
	t.Setenv("OPTICORE_ENV_FILE", "")
	t.Chdir(t.TempDir())

	got, err := loadEnvFile("", exeErr)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != "" {
		t.Fatalf("path = %q, want empty", got)
	}
}

func TestLoadEnvFileRealEnvWins(t *testing.T) {
	t.Setenv("OPTICORE_ENV_FILE", "")
	t.Setenv("OPTICORE_TEST_WINS", "real")
	dir := t.TempDir()
	writeEnv(t, dir, "OPTICORE_TEST_WINS=file\n")

	if _, err := loadEnvFile("", exeErr); err != nil {
		t.Fatalf("load: %v", err)
	}
	if v := os.Getenv("OPTICORE_TEST_WINS"); v != "real" {
		t.Fatalf("OPTICORE_TEST_WINS = %q, want real", v)
	}
}
