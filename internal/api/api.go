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
//   - "jJ:NS" (job snapshots, jobs.updated_at) replays jobs after (NS, J) and
//     events and tasks from NS on.
//
// A stream without a cursor starts at the current end of every table. Replay
// is bounded by the events/tasks retention windows.
package api

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"

	"github.com/prawirdani/opticore/internal/acs"
	"github.com/prawirdani/opticore/internal/store"
)

// Server is the /v1 HTTP handler. It is safe for concurrent use.
type Server struct {
	st  *store.Store
	svc *acs.Service
	log *log.Logger

	shutdown chan struct{}
	once     sync.Once
}

// New builds the API server. The caller owns st and svc.
func New(st *store.Store, svc *acs.Service, logger *log.Logger) *Server {
	if logger == nil {
		logger = log.Default()
	}
	return &Server{
		st:       st,
		svc:      svc,
		log:      logger,
		shutdown: make(chan struct{}),
	}
}

// Close ends every live SSE stream. The caller should use it during shutdown.
func (s *Server) Close() {
	s.once.Do(func() { close(s.shutdown) })
}

// Handler returns the routed /v1 mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/devices/{id}/tasks", s.createTasks)
	mux.HandleFunc("DELETE /v1/devices/{id}/tasks/{taskId}", s.cancelTask)
	mux.HandleFunc("POST /v1/devices/{id}/tasks/{taskId}/revert", s.revertTask)
	mux.HandleFunc("POST /v1/devices/{id}/wake", s.wakeDevice)
	mux.HandleFunc("DELETE /v1/devices/{id}", s.deleteDevice)
	mux.HandleFunc("GET /v1/devices", s.listDevices)
	mux.HandleFunc("GET /v1/devices/{id}", s.getDevice)
	mux.HandleFunc("GET /v1/devices/{id}/params", s.listParams)
	mux.HandleFunc("GET /v1/devices/{id}/tasks", s.listTasks)
	mux.HandleFunc("GET /v1/devices/{id}/events", s.listEvents)
	mux.HandleFunc("GET /v1/tasks/{taskId}", s.getTask)
	mux.HandleFunc("POST /v1/jobs", s.createJob)
	mux.HandleFunc("GET /v1/jobs", s.listJobs)
	mux.HandleFunc("GET /v1/jobs/{id}", s.getJob)
	mux.HandleFunc("DELETE /v1/jobs/{id}", s.cancelJob)
	mux.HandleFunc("POST /v1/jobs/{id}/revert", s.revertJob)
	mux.HandleFunc("GET /v1/events", s.streamEvents)

	return mux
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
