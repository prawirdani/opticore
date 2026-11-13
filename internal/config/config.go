// Package config loads and validates Opticore configuration from environment
// variables. See README Appendix B.
package config

import (
	"encoding/base64"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Config holds every environment variable the daemon understands.
type Config struct {
	DBDSN     string
	SecretKey []byte // AES-256-GCM key, decoded from base64

	CWMPAddr           string
	CWMPGPVBatch       int
	CWMPSessionTimeout time.Duration
	CWMPCRTimeout      time.Duration
	CWMPRequestTimeout time.Duration
	CWMPMaxBody        int64
	CWMPMaxSessions    int
	CWMPAllowCIDRs     []netip.Prefix // device source allowlist; empty: any
	CWMPCRMatchSource  bool           // ConnectionRequestURL host must be the Inform's source IP
	CWMPAuthUsername   string         // device-to-ACS credentials; empty: none
	CWMPAuthPassword   string

	APIAddr string

	// API security.
	APIAuth          bool // require a bearer token on /v1
	APIInsecureLocal bool // accept unauthenticated /v1 on loopback only
	APIRequireReveal bool // ?reveal=1 needs a token with the reveal scope

	HookURL         string
	HookSecret      string
	HookEvents      []string
	HookPrefetch    []string
	HookStepTimeout time.Duration
	HookTimeout     time.Duration

	TaskExpiry      time.Duration
	TaskMaxAttempts int
	TaskRetention   time.Duration
	EventRetention  time.Duration

	LogLevel string
	LogFile  string // JSON log destination; empty means stderr
}

// ValidationError lists every problem found while loading configuration.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	return "invalid configuration: " + strings.Join(e.Problems, "; ")
}

type loader struct {
	getenv   func(string) string
	problems []string
}

func (l *loader) fail(format string, args ...any) {
	l.problems = append(l.problems, fmt.Sprintf(format, args...))
}

func (l *loader) str(name, def string) string {
	if v := strings.TrimSpace(l.getenv(name)); v != "" {
		return v
	}
	return def
}

func (l *loader) dur(name string, def time.Duration) time.Duration {
	raw := strings.TrimSpace(l.getenv(name))
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		l.fail("%s: %q is not a positive duration", name, raw)
		return def
	}
	return d
}

func (l *loader) integer(name string, def int) int {
	raw := strings.TrimSpace(l.getenv(name))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		l.fail("%s: %q is not a positive integer", name, raw)
		return def
	}
	return n
}

func (l *loader) size(name string, def int64) int64 {
	raw := strings.TrimSpace(l.getenv(name))
	if raw == "" {
		return def
	}
	n, err := ParseSize(raw)
	if err != nil {
		l.fail("%s: %v", name, err)
		return def
	}
	return n
}

func (l *loader) boolean(name string, def bool) bool {
	raw := strings.ToLower(strings.TrimSpace(l.getenv(name)))
	switch raw {
	case "":
		return def
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	l.fail("%s: %q is not a boolean", name, raw)
	return def
}

func (l *loader) addr(name, def string) string {
	v := l.str(name, def)
	if _, _, err := net.SplitHostPort(v); err != nil {
		l.fail("%s: %q is not a host:port address", name, v)
	}
	return v
}

func (l *loader) list(name, def string) []string {
	raw := l.str(name, def)
	if raw == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// prefixes parses a comma list of CIDRs or bare IPs.
func (l *loader) prefixes(name string) []netip.Prefix {
	var out []netip.Prefix
	for _, v := range l.list(name, "") {
		if !strings.Contains(v, "/") {
			a, err := netip.ParseAddr(v)
			if err != nil {
				l.fail("%s: %q is not an IP or CIDR", name, v)
				continue
			}
			out = append(out, netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
			continue
		}
		p, err := netip.ParsePrefix(v)
		if err != nil {
			l.fail("%s: %q is not an IP or CIDR", name, v)
			continue
		}
		out = append(out, p.Masked())
	}
	return out
}

// Load reads the full daemon configuration and validates every value.
func Load(getenv func(string) string) (Config, error) {
	l := &loader{getenv: getenv}
	cfg := Config{
		DBDSN:              l.str("OPTICORE_DB_DSN", ""),
		CWMPAddr:           l.addr("CWMP_ADDR", ":7547"),
		CWMPGPVBatch:       l.integer("CWMP_GPV_BATCH", 32),
		CWMPSessionTimeout: l.dur("CWMP_SESSION_TIMEOUT", 30*time.Second),
		CWMPCRTimeout:      l.dur("CWMP_CR_TIMEOUT", 2*time.Second),
		CWMPRequestTimeout: l.dur("CWMP_REQUEST_TIMEOUT", 10*time.Second),
		CWMPMaxBody:        l.size("CWMP_MAX_BODY", 10<<20),
		CWMPMaxSessions:    l.integer("CWMP_MAX_SESSIONS", 1000),
		CWMPAllowCIDRs:     l.prefixes("CWMP_ALLOW_CIDRS"),
		CWMPCRMatchSource:  l.boolean("CWMP_CR_URL_MATCH_SOURCE", true),
		CWMPAuthUsername:   l.str("CWMP_AUTH_USERNAME", ""),
		CWMPAuthPassword:   l.str("CWMP_AUTH_PASSWORD", ""),
		APIAddr:            l.addr("OPTICORE_API_ADDR", ":8081"),
		APIAuth:            l.boolean("OPTICORE_API_AUTH", true),
		APIInsecureLocal:   l.boolean("OPTICORE_API_INSECURE_LOCAL", false),
		APIRequireReveal:   l.boolean("OPTICORE_API_REQUIRE_REVEAL", true),
		HookURL:            l.str("OPTICORE_HOOK_URL", ""),
		HookSecret:         l.str("OPTICORE_HOOK_SECRET", ""),
		HookEvents:         l.list("OPTICORE_HOOK_EVENTS", "0 BOOTSTRAP"),
		HookPrefetch:       l.list("OPTICORE_HOOK_PREFETCH", ""),
		HookStepTimeout:    l.dur("OPTICORE_HOOK_STEP_TIMEOUT", 2*time.Second),
		HookTimeout:        l.dur("OPTICORE_HOOK_TIMEOUT", 10*time.Second),
		TaskExpiry:         l.dur("OPTICORE_TASK_EXPIRY", 10*time.Minute),
		TaskMaxAttempts:    l.integer("OPTICORE_TASK_MAX_ATTEMPTS", 5),
		TaskRetention:      l.dur("OPTICORE_TASK_RETENTION", 720*time.Hour),
		EventRetention:     l.dur("OPTICORE_EVENT_RETENTION", 720*time.Hour),
		LogLevel:           strings.ToLower(l.str("OPTICORE_LOG_LEVEL", "info")),
		LogFile:            l.str("OPTICORE_LOG_FILE", ""),
	}

	if cfg.DBDSN == "" {
		l.fail("OPTICORE_DB_DSN is required")
	}
	cfg.SecretKey = l.secretKey()

	switch cfg.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		l.fail("OPTICORE_LOG_LEVEL: %q is not one of debug, info, warn, error", cfg.LogLevel)
	}

	if (cfg.CWMPAuthUsername == "") != (cfg.CWMPAuthPassword == "") {
		l.fail("CWMP_AUTH_USERNAME and CWMP_AUTH_PASSWORD must be set together")
	}

	if cfg.HookURL != "" {
		if cfg.HookSecret == "" {
			l.fail("OPTICORE_HOOK_SECRET is required when OPTICORE_HOOK_URL is set")
		}
		if !strings.HasPrefix(cfg.HookURL, "http://") &&
			!strings.HasPrefix(cfg.HookURL, "https://") &&
			!strings.HasPrefix(cfg.HookURL, "unix:") {
			l.fail("OPTICORE_HOOK_URL: %q must be http://, https:// or unix:", cfg.HookURL)
		}
	}

	if len(l.problems) > 0 {
		return cfg, &ValidationError{Problems: l.problems}
	}
	return cfg, nil
}

// LoadClient reads just what CLI subcommands and the TUI need.
func LoadClient(getenv func(string) string) (Config, error) {
	l := &loader{getenv: getenv}
	cfg := Config{
		DBDSN:    l.str("OPTICORE_DB_DSN", ""),
		LogLevel: strings.ToLower(l.str("OPTICORE_LOG_LEVEL", "info")),
		LogFile:  l.str("OPTICORE_LOG_FILE", ""),
		// Only the TUI needs this, for its wake action.
		CWMPCRTimeout: l.dur("CWMP_CR_TIMEOUT", 2*time.Second),
	}
	if cfg.DBDSN == "" {
		l.fail("OPTICORE_DB_DSN is required")
	}
	cfg.SecretKey = l.secretKey()
	if len(l.problems) > 0 {
		return cfg, &ValidationError{Problems: l.problems}
	}
	return cfg, nil
}

func (l *loader) secretKey() []byte {
	raw := strings.TrimSpace(l.getenv("OPTICORE_SECRET_KEY"))
	if raw == "" {
		l.fail("OPTICORE_SECRET_KEY is required")
		return nil
	}
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding,
	} {
		if key, err := enc.DecodeString(raw); err == nil {
			if len(key) != 32 {
				l.fail("OPTICORE_SECRET_KEY must decode to 32 bytes, got %d", len(key))
				return nil
			}
			return key
		}
	}
	l.fail("OPTICORE_SECRET_KEY is not valid base64")
	return nil
}

var sizeRE = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)\s*([A-Za-z]*)$`)

var sizeUnits = map[string]float64{
	"":    1,
	"b":   1,
	"k":   1e3,
	"kb":  1e3,
	"m":   1e6,
	"mb":  1e6,
	"g":   1e9,
	"gb":  1e9,
	"kib": 1 << 10,
	"mib": 1 << 20,
	"gib": 1 << 30,
}

// ParseSize parses a byte count such as "10485760", "10MiB" or "1MB".
func ParseSize(s string) (int64, error) {
	m := sizeRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, fmt.Errorf("%q is not a byte size", s)
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a byte size", s)
	}
	unit, ok := sizeUnits[strings.ToLower(m[2])]
	if !ok {
		return 0, fmt.Errorf("%q has an unknown size unit", s)
	}
	v := int64(n * unit)
	if v <= 0 {
		return 0, fmt.Errorf("%q is not a positive byte size", s)
	}
	return v, nil
}
