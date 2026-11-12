// Package api serves the /v1 HTTP control and read planes from README §10 and
// Appendix A, plus the SSE stream.
//
// SSE resume semantics: every streamed row carries an opaque id. Event rows
// use "e<events.id>"; task transitions use "t<tasks.id>:<updated_at unix
// nanos>". A client reconnects with Last-Event-ID (or ?last_event_id=):
//
//   - "eN" replays events with id > N and tasks updated at or after event N.
//   - "tI:NS" replays tasks after (NS, I) and events newer than the newest
//     event at NS.
//
// A stream without a cursor starts at the current end of both tables. Replay
// is bounded by the events/tasks retention windows.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/prawirdani/opticore/internal/acs"
	"github.com/prawirdani/opticore/internal/store"
)

// Options tunes the API for the deployment. ListenAddr is the address the
// caller will bind; New uses it to refuse AllowInsecureLocal off loopback.
type Options struct {
	AllowInsecureLocal bool
	RequireRevealScope bool
	ListenAddr         string
}

// Server is the /v1 HTTP handler. It is safe for concurrent use.
type Server struct {
	st   *store.Store
	svc  *acs.Service
	key  []byte
	log  *slog.Logger
	opts Options

	fatal    error
	shutdown chan struct{}
	once     sync.Once
}

type ctxKey int

const scopesKey ctxKey = 0

// New builds the API server. The caller owns st and svc.
func New(st *store.Store, svc *acs.Service, key []byte, log *slog.Logger, opts Options) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{
		st:       st,
		svc:      svc,
		key:      key,
		log:      log,
		opts:     opts,
		shutdown: make(chan struct{}),
	}
	if opts.AllowInsecureLocal {
		if err := CheckInsecureLocal(opts.ListenAddr); err != nil {
			s.fatal = err
		}
	}
	return s
}

// Close ends every live SSE stream. The caller should use it during shutdown.
func (s *Server) Close() {
	s.once.Do(func() { close(s.shutdown) })
}

// Err reports a startup-time configuration problem, such as -insecure-local
// on a non-loopback listener. The daemon must refuse to start when non-nil.
func (s *Server) Err() error { return s.fatal }

// CheckInsecureLocal accepts an address only when it binds a loopback host.
// An empty host (all interfaces) is refused.
func CheckInsecureLocal(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return errors.New("insecure-local: listener address is not host:port")
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("insecure-local: listener is not loopback")
	}
	return nil
}

// Handler returns the routed /v1 mux wrapped in authentication.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/devices/{id}/tasks", s.createTasks)
	mux.HandleFunc("DELETE /v1/devices/{id}/tasks/{taskId}", s.cancelTask)
	mux.HandleFunc("POST /v1/devices/{id}/wake", s.wakeDevice)
	mux.HandleFunc("DELETE /v1/devices/{id}", s.deleteDevice)
	mux.HandleFunc("GET /v1/devices", s.listDevices)
	mux.HandleFunc("GET /v1/devices/{id}", s.getDevice)
	mux.HandleFunc("GET /v1/devices/{id}/params", s.listParams)
	mux.HandleFunc("GET /v1/devices/{id}/tasks", s.listTasks)
	mux.HandleFunc("GET /v1/devices/{id}/events", s.listEvents)
	mux.HandleFunc("GET /v1/tasks/{taskId}", s.getTask)
	mux.HandleFunc("GET /v1/events", s.streamEvents)

	var h http.Handler = mux
	h = s.auth(h)
	if s.fatal != nil {
		fatal := s.fatal
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s.log.Error("api refuses to start", "err", fatal)
			writeError(w, http.StatusServiceUnavailable, "CONFLICT", fatal.Error())
		})
	}
	return h
}

func (s *Server) auth(next http.Handler) http.Handler {
	if s.opts.AllowInsecureLocal {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing bearer token")
			return
		}
		scopes, err := s.st.LookupToken(r.Context(), token)
		if errors.Is(err, store.ErrTokenNotFound) {
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "invalid token")
			return
		}
		if err != nil {
			s.internal(w, "lookup token", err)
			return
		}
		need := "read"
		if r.Method != http.MethodGet {
			need = "write"
		}
		if !slices.Contains(scopes, need) {
			writeError(w, http.StatusForbidden, "FORBIDDEN", "token lacks the "+need+" scope")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), scopesKey, scopes)))
	})
}

func bearerToken(r *http.Request) (string, bool) {
	scheme, rest, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token := strings.TrimSpace(rest)
	return token, token != ""
}

func scopesFrom(ctx context.Context) []string {
	scopes, _ := ctx.Value(scopesKey).([]string)
	return scopes
}

// revealAllowed reports whether ?reveal=1 may be honoured. Without auth on a
// loopback listener it is allowed; otherwise it needs the reveal scope and
// RequireRevealScope enabled, so a read-only token can never read secrets.
func (s *Server) revealAllowed(r *http.Request) bool {
	if r.URL.Query().Get("reveal") != "1" {
		return false
	}
	if s.opts.AllowInsecureLocal {
		return true
	}
	if !s.opts.RequireRevealScope {
		return false
	}
	return slices.Contains(scopesFrom(r.Context()), "reveal")
}

// requireReveal rejects ?reveal=1 unless the caller may use it, and returns
// whether reveal is active.
func (s *Server) requireReveal(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Query().Get("reveal") != "1" {
		return false
	}
	if s.revealAllowed(r) {
		return true
	}
	writeError(w, http.StatusForbidden, "FORBIDDEN", "token lacks the reveal scope")
	return false
}

type errorBody struct {
	Error struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	var body errorBody
	body.Error.Code = code
	body.Error.Message = message
	body.Error.Details = map[string]any{}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
