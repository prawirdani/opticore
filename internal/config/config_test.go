package config

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

func testKey() string {
	return base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
}

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func required() map[string]string {
	return map[string]string{
		"OPTICORE_DB_DSN":     "postgres://example/opticore",
		"OPTICORE_SECRET_KEY": testKey(),
	}
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(env(required()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"CWMPAddr", cfg.CWMPAddr, ":7547"},
		{"CWMPGPVBatch", cfg.CWMPGPVBatch, 32},
		{"CWMPSessionTimeout", cfg.CWMPSessionTimeout, 30 * time.Second},
		{"CWMPCRTimeout", cfg.CWMPCRTimeout, 2 * time.Second},
		{"CWMPRequestTimeout", cfg.CWMPRequestTimeout, 10 * time.Second},
		{"CWMPMaxBody", cfg.CWMPMaxBody, int64(10 << 20)},
		{"CWMPMaxSessions", cfg.CWMPMaxSessions, 1000},
		{"APIAddr", cfg.APIAddr, ":8081"},
		{"FileAddr", cfg.FileAddr, ":8082"},
		{"FirmwareDir", cfg.FirmwareDir, "/var/lib/opticore/firmware"},
		{"HookStepTimeout", cfg.HookStepTimeout, 2 * time.Second},
		{"HookTimeout", cfg.HookTimeout, 10 * time.Second},
		{"TaskExpiry", cfg.TaskExpiry, 10 * time.Minute},
		{"TaskMaxAttempts", cfg.TaskMaxAttempts, 5},
		{"TaskRetention", cfg.TaskRetention, 720 * time.Hour},
		{"EventRetention", cfg.EventRetention, 720 * time.Hour},
		{"LogLevel", cfg.LogLevel, "info"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if len(cfg.SecretKey) != 32 {
		t.Errorf("SecretKey length = %d, want 32", len(cfg.SecretKey))
	}
	if len(cfg.HookEvents) != 1 || cfg.HookEvents[0] != "0 BOOTSTRAP" {
		t.Errorf("HookEvents = %v, want [0 BOOTSTRAP]", cfg.HookEvents)
	}
	if cfg.HookURL != "" || cfg.HookPrefetch != nil {
		t.Errorf("unset hook fields = %q %v", cfg.HookURL, cfg.HookPrefetch)
	}
}

func TestLoadOverrides(t *testing.T) {
	m := required()
	m["CWMP_GPV_BATCH"] = "8"
	m["CWMP_MAX_BODY"] = "1MB"
	m["CWMP_SESSION_TIMEOUT"] = "45s"
	m["CWMP_ADDR"] = "10.0.0.5:7547"
	m["OPTICORE_HOOK_URL"] = "unix:/run/provisioner.sock"
	m["OPTICORE_HOOK_SECRET"] = "s3cret"
	m["OPTICORE_HOOK_EVENTS"] = "0 BOOTSTRAP, 1 BOOT"
	m["OPTICORE_HOOK_PREFETCH"] = "InternetGatewayDevice.WANDevice.,InternetGatewayDevice.DeviceInfo."
	m["OPTICORE_LOG_LEVEL"] = "DEBUG"
	m["OPTICORE_TASK_EXPIRY"] = "1h"

	cfg, err := Load(env(m))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.CWMPGPVBatch != 8 || cfg.CWMPMaxBody != 1_000_000 || cfg.CWMPSessionTimeout != 45*time.Second {
		t.Errorf("overrides not applied: %+v", cfg)
	}
	if cfg.CWMPAddr != "10.0.0.5:7547" || cfg.LogLevel != "debug" || cfg.TaskExpiry != time.Hour {
		t.Errorf("overrides not applied: %+v", cfg)
	}
	if len(cfg.HookEvents) != 2 || cfg.HookEvents[1] != "1 BOOT" {
		t.Errorf("HookEvents = %v", cfg.HookEvents)
	}
	if len(cfg.HookPrefetch) != 2 {
		t.Errorf("HookPrefetch = %v", cfg.HookPrefetch)
	}
}

func TestLoadErrors(t *testing.T) {
	shortKey := base64.StdEncoding.EncodeToString(make([]byte, 16))
	tests := []struct {
		name      string
		mutate    func(map[string]string)
		wantParts []string
	}{
		{
			name: "missing required",
			mutate: func(m map[string]string) {
				delete(m, "OPTICORE_DB_DSN")
				delete(m, "OPTICORE_SECRET_KEY")
			},
			wantParts: []string{"OPTICORE_DB_DSN", "OPTICORE_SECRET_KEY"},
		},
		{
			name:      "short key",
			mutate:    func(m map[string]string) { m["OPTICORE_SECRET_KEY"] = shortKey },
			wantParts: []string{"32 bytes"},
		},
		{
			name:      "invalid base64 key",
			mutate:    func(m map[string]string) { m["OPTICORE_SECRET_KEY"] = "not base64!" },
			wantParts: []string{"base64"},
		},
		{
			name:      "bad duration",
			mutate:    func(m map[string]string) { m["CWMP_SESSION_TIMEOUT"] = "30" },
			wantParts: []string{"CWMP_SESSION_TIMEOUT", "duration"},
		},
		{
			name:      "zero duration",
			mutate:    func(m map[string]string) { m["OPTICORE_TASK_EXPIRY"] = "0s" },
			wantParts: []string{"OPTICORE_TASK_EXPIRY"},
		},
		{
			name:      "bad address",
			mutate:    func(m map[string]string) { m["CWMP_ADDR"] = "7547" },
			wantParts: []string{"CWMP_ADDR", "host:port"},
		},
		{
			name:      "bad size unit",
			mutate:    func(m map[string]string) { m["CWMP_MAX_BODY"] = "10XB" },
			wantParts: []string{"CWMP_MAX_BODY", "unknown size unit"},
		},
		{
			name:      "non-positive integer",
			mutate:    func(m map[string]string) { m["CWMP_GPV_BATCH"] = "0" },
			wantParts: []string{"CWMP_GPV_BATCH"},
		},
		{
			name: "hook url without secret",
			mutate: func(m map[string]string) {
				m["OPTICORE_HOOK_URL"] = "http://provisioner:9000"
			},
			wantParts: []string{"OPTICORE_HOOK_SECRET"},
		},
		{
			name: "hook url bad scheme",
			mutate: func(m map[string]string) {
				m["OPTICORE_HOOK_URL"] = "ftp://provisioner"
				m["OPTICORE_HOOK_SECRET"] = "x"
			},
			wantParts: []string{"OPTICORE_HOOK_URL"},
		},
		{
			name:      "bad log level",
			mutate:    func(m map[string]string) { m["OPTICORE_LOG_LEVEL"] = "trace" },
			wantParts: []string{"OPTICORE_LOG_LEVEL"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := required()
			tc.mutate(m)
			_, err := Load(env(m))
			if err == nil {
				t.Fatal("Load returned nil error")
			}
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("error type %T, want *ValidationError", err)
			}
			for _, part := range tc.wantParts {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("error %q does not mention %q", err, part)
				}
			}
		})
	}
}

func TestLoadListsAllProblems(t *testing.T) {
	m := map[string]string{
		"CWMP_ADDR":      "nope",
		"CWMP_GPV_BATCH": "-1",
	}
	_, err := Load(env(m))
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error type %T, want *ValidationError", err)
	}
	// DSN, key, address, batch.
	if len(ve.Problems) < 4 {
		t.Errorf("Problems = %v, want at least 4", ve.Problems)
	}
}

func TestLoadClient(t *testing.T) {
	m := required()
	m["CWMP_ADDR"] = "not an address" // clients do not read CWMP settings
	cfg, err := LoadClient(env(m))
	if err != nil {
		t.Fatalf("LoadClient: %v", err)
	}
	if cfg.DBDSN == "" || len(cfg.SecretKey) != 32 {
		t.Errorf("LoadClient missing required values: %+v", cfg)
	}

	delete(m, "OPTICORE_SECRET_KEY")
	if _, err := LoadClient(env(m)); err == nil || !strings.Contains(err.Error(), "OPTICORE_SECRET_KEY") {
		t.Errorf("LoadClient error = %v, want missing key", err)
	}
}

func TestParseSize(t *testing.T) {
	tests := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"10MiB", 10 << 20, true},
		{"1MB", 1_000_000, true},
		{"512", 512, true},
		{"1KiB", 1024, true},
		{"1.5MiB", 1572864, true},
		{"2 GB", 2_000_000_000, true},
		{"", 0, false},
		{"10XB", 0, false},
		{"-1", 0, false},
		{"0", 0, false},
	}
	for _, tc := range tests {
		got, err := ParseSize(tc.in)
		if tc.ok && err != nil {
			t.Errorf("ParseSize(%q) error: %v", tc.in, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("ParseSize(%q) = %d, want error", tc.in, got)
		}
		if tc.ok && got != tc.want {
			t.Errorf("ParseSize(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestCWMPSecurity(t *testing.T) {
	m := required()
	m["CWMP_ALLOW_CIDRS"] = "172.16.106.0/24, 10.0.0.5"
	m["CWMP_AUTH_USERNAME"] = "acs"
	m["CWMP_AUTH_PASSWORD"] = "pw"
	cfg, err := Load(env(m))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.CWMPAllowCIDRs) != 2 || cfg.CWMPAllowCIDRs[1].String() != "10.0.0.5/32" || !cfg.CWMPCRMatchSource {
		t.Errorf("cfg = %+v %v", cfg.CWMPAllowCIDRs, cfg.CWMPCRMatchSource)
	}
	m["CWMP_ALLOW_CIDRS"] = "nope"
	m["CWMP_AUTH_PASSWORD"] = ""
	_, err = Load(env(m))
	if err == nil || !strings.Contains(err.Error(), "CWMP_ALLOW_CIDRS") || !strings.Contains(err.Error(), "CWMP_AUTH_USERNAME") {
		t.Errorf("err = %v", err)
	}
}

func TestAPIAuthToggle(t *testing.T) {
	cfg, err := Load(env(required()))
	if err != nil || !cfg.APIAuth {
		t.Fatalf("default APIAuth = %v, %v; want true", cfg.APIAuth, err)
	}
	m := required()
	m["OPTICORE_API_AUTH"] = "false"
	cfg, err = Load(env(m))
	if err != nil || cfg.APIAuth {
		t.Fatalf("OPTICORE_API_AUTH=false → %v, %v", cfg.APIAuth, err)
	}
	m["OPTICORE_API_AUTH"] = "maybe"
	if _, err := Load(env(m)); err == nil || !strings.Contains(err.Error(), "OPTICORE_API_AUTH") {
		t.Errorf("bogus value accepted: %v", err)
	}
}
