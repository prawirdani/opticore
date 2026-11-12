package api

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prawirdani/opticore/internal/store"
)

const (
	ssePoll      = 250 * time.Millisecond
	sseHeartbeat = 15 * time.Second
	sseBatch     = 500
)

func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "streaming unsupported")
		return
	}
	reveal := s.requireReveal(w, r)
	if r.URL.Query().Get("reveal") == "1" && !reveal {
		return
	}
	types := typeFilter(r.URL.Query().Get("type"))
	device := r.URL.Query().Get("device_id")
	cursor := r.Header.Get("Last-Event-ID")
	if cursor == "" {
		cursor = r.URL.Query().Get("last_event_id")
	}
	lastEvent, lastTaskAt, lastTaskID := s.sseStart(r.Context(), cursor)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	poll := time.NewTicker(ssePoll)
	defer poll.Stop()
	heartbeat := time.NewTicker(sseHeartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.shutdown:
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-poll.C:
			s.ssePump(r.Context(), w, flusher, types, device, reveal,
				&lastEvent, &lastTaskAt, &lastTaskID)
		}
	}
}

// sseStart resolves a resume cursor into an events.id, a task updated_at and
// a task id. Without a cursor it starts at the current end of both tables.
func (s *Server) sseStart(ctx context.Context, cursor string) (int64, time.Time, int64) {
	if cursor == "" {
		now, err := s.st.Now(ctx)
		if err != nil {
			s.log.Error("sse: start cursor", "err", err)
			now = time.Now()
		}
		lastEvent, err := s.st.MaxEventID(ctx)
		if err != nil {
			s.log.Error("sse: max event id", "err", err)
		}
		return lastEvent, now, math.MaxInt64
	}
	if rest, ok := strings.CutPrefix(cursor, "e"); ok {
		id, err := strconv.ParseInt(rest, 10, 64)
		if err != nil {
			return s.sseStart(ctx, "")
		}
		at, err := s.st.EventAt(ctx, id)
		if err != nil {
			s.log.Error("sse: event at", "err", err)
		}
		if at.IsZero() {
			at, _ = s.st.Now(ctx)
			at = at.Add(-time.Second)
		}
		return id, at, -1
	}
	if rest, ok := strings.CutPrefix(cursor, "t"); ok {
		idPart, nsPart, ok := strings.Cut(rest, ":")
		if !ok {
			return s.sseStart(ctx, "")
		}
		id, err1 := strconv.ParseInt(idPart, 10, 64)
		ns, err2 := strconv.ParseInt(nsPart, 10, 64)
		if err1 != nil || err2 != nil {
			return s.sseStart(ctx, "")
		}
		at := time.Unix(0, ns).UTC()
		lastEvent, err := s.st.MaxEventIDAt(ctx, at)
		if err != nil {
			s.log.Error("sse: max event id at", "err", err)
		}
		return lastEvent, at, id
	}
	if id, err := strconv.ParseInt(cursor, 10, 64); err == nil {
		return s.sseStart(ctx, "e"+strconv.FormatInt(id, 10))
	}
	return s.sseStart(ctx, "")
}

func (s *Server) ssePump(ctx context.Context, w http.ResponseWriter, flusher http.Flusher,
	types map[string]bool, device string, reveal bool,
	lastEvent *int64, lastTaskAt *time.Time, lastTaskID *int64) {
	for {
		events, err := s.st.EventsSinceID(ctx, *lastEvent, sseBatch)
		if err != nil {
			s.log.Error("sse: events", "err", err)
			return
		}
		for _, e := range events {
			*lastEvent = e.ID
			if device != "" && e.DeviceID != device {
				continue
			}
			if len(types) > 0 && !types[e.Type] {
				continue
			}
			if !writeSSE(w, flusher, "e"+strconv.FormatInt(e.ID, 10), e.Type, eventData(e)) {
				return
			}
		}
		if len(events) < sseBatch {
			break
		}
	}
	for {
		tasks, err := s.st.TasksUpdatedSince(ctx, *lastTaskAt, *lastTaskID, sseBatch)
		if err != nil {
			s.log.Error("sse: tasks", "err", err)
			return
		}
		for _, t := range tasks {
			*lastTaskAt = t.UpdatedAt
			*lastTaskID = t.ID
			if device != "" && t.DeviceID != device {
				continue
			}
			if len(types) > 0 && !types["task"] {
				continue
			}
			id := "t" + strconv.FormatInt(t.ID, 10) + ":" +
				strconv.FormatInt(t.UpdatedAt.UnixNano(), 10)
			if !writeSSE(w, flusher, id, "task", s.taskView(t, reveal)) {
				return
			}
		}
		if len(tasks) < sseBatch {
			return
		}
	}
}

func writeSSE(w http.ResponseWriter, flusher http.Flusher, id, name string, data any) bool {
	b, err := json.Marshal(data)
	if err != nil {
		return true
	}
	if _, err := fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", id, name, b); err != nil {
		return false
	}
	flusher.Flush()
	return true
}

func eventData(e store.Event) map[string]any {
	out := map[string]any{}
	if len(e.Detail) > 0 {
		var detail map[string]any
		if json.Unmarshal(e.Detail, &detail) == nil && detail != nil {
			for k, v := range detail {
				out[k] = v
			}
		}
	}
	out["device_id"] = e.DeviceID
	out["at"] = e.At
	out["type"] = e.Type
	return out
}

func typeFilter(raw string) map[string]bool {
	if raw == "" {
		return nil
	}
	out := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out[p] = true
		}
	}
	return out
}
