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
	reveal := revealRequested(r)
	types := typeFilter(r.URL.Query().Get("type"))
	device := r.URL.Query().Get("device_id")
	cursor := r.Header.Get("Last-Event-ID")
	if cursor == "" {
		cursor = r.URL.Query().Get("last_event_id")
	}
	cur := s.sseStart(r.Context(), cursor)

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
			s.ssePump(r.Context(), w, flusher, types, device, reveal, &cur)
		}
	}
}

// sseCursor is the resume position of one stream: the last events.id, the
// last (tasks.updated_at, tasks.id) and the last (jobs.updated_at, jobs.id).
//
// Cursor formats accepted in Last-Event-ID or ?last_event_id=:
//
//	e<events.id>       events after that id; tasks and jobs from its time.
//	t<tasks.id>:<ns>   tasks after (ns, id); events and jobs from ns.
//	j<jobs.id>:<ns>    jobs after (ns, id); events and tasks from ns.
//
// A bare integer is read as e<n>, so the oldest format keeps working.
type sseCursor struct {
	eventID int64
	taskAt  time.Time
	taskID  int64
	jobAt   time.Time
	jobID   int64
}

// sseStart resolves a resume cursor. Without one it starts at the current end
// of all three tables.
func (s *Server) sseStart(ctx context.Context, cursor string) sseCursor {
	if cursor == "" {
		now, err := s.st.Now(ctx)
		if err != nil {
			s.log.Printf("ERROR sse: start cursor err=%v", err)
			now = time.Now()
		}
		lastEvent, err := s.st.MaxEventID(ctx)
		if err != nil {
			s.log.Printf("ERROR sse: max event id err=%v", err)
		}
		return sseCursor{eventID: lastEvent, taskAt: now, taskID: math.MaxInt64,
			jobAt: now, jobID: math.MaxInt64}
	}
	if rest, ok := strings.CutPrefix(cursor, "e"); ok {
		id, err := strconv.ParseInt(rest, 10, 64)
		if err != nil {
			return s.sseStart(ctx, "")
		}
		at, err := s.st.EventAt(ctx, id)
		if err != nil {
			s.log.Printf("ERROR sse: event at err=%v", err)
		}
		if at.IsZero() {
			at, _ = s.st.Now(ctx)
			at = at.Add(-time.Second)
		}
		return sseCursor{eventID: id, taskAt: at, taskID: -1, jobAt: at, jobID: -1}
	}
	if rest, ok := strings.CutPrefix(cursor, "t"); ok {
		id, at, ok := parseRowCursor(rest)
		if !ok {
			return s.sseStart(ctx, "")
		}
		lastEvent, err := s.st.MaxEventIDAt(ctx, at)
		if err != nil {
			s.log.Printf("ERROR sse: max event id at err=%v", err)
		}
		return sseCursor{eventID: lastEvent, taskAt: at, taskID: id, jobAt: at, jobID: -1}
	}
	if rest, ok := strings.CutPrefix(cursor, "j"); ok {
		id, at, ok := parseRowCursor(rest)
		if !ok {
			return s.sseStart(ctx, "")
		}
		lastEvent, err := s.st.MaxEventIDAt(ctx, at)
		if err != nil {
			s.log.Printf("ERROR sse: max event id at err=%v", err)
		}
		return sseCursor{eventID: lastEvent, taskAt: at, taskID: -1, jobAt: at, jobID: id}
	}
	if id, err := strconv.ParseInt(cursor, 10, 64); err == nil {
		return s.sseStart(ctx, "e"+strconv.FormatInt(id, 10))
	}
	return s.sseStart(ctx, "")
}

// parseRowCursor parses "<id>:<unixnano>" from a t or j cursor.
func parseRowCursor(rest string) (int64, time.Time, bool) {
	idPart, nsPart, ok := strings.Cut(rest, ":")
	if !ok {
		return 0, time.Time{}, false
	}
	id, err1 := strconv.ParseInt(idPart, 10, 64)
	ns, err2 := strconv.ParseInt(nsPart, 10, 64)
	if err1 != nil || err2 != nil {
		return 0, time.Time{}, false
	}
	return id, time.Unix(0, ns).UTC(), true
}

func (s *Server) ssePump(ctx context.Context, w http.ResponseWriter, flusher http.Flusher,
	types map[string]bool, device string, reveal bool, cur *sseCursor) {
	for {
		events, err := s.st.EventsSinceID(ctx, cur.eventID, sseBatch)
		if err != nil {
			s.log.Printf("ERROR sse: events err=%v", err)
			return
		}
		for _, e := range events {
			cur.eventID = e.ID
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
		tasks, err := s.st.TasksUpdatedSince(ctx, cur.taskAt, cur.taskID, sseBatch)
		if err != nil {
			s.log.Printf("ERROR sse: tasks err=%v", err)
			return
		}
		for _, t := range tasks {
			cur.taskAt = t.UpdatedAt
			cur.taskID = t.ID
			if device != "" && t.DeviceID != device {
				continue
			}
			// A terminal task moves its job's counts; the trigger touched
			// jobs.updated_at, so the job loop below emits the snapshot.
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
			break
		}
	}
	// A job event is aggregate, not device-scoped: with ?device_id= set we
	// still advance the cursor but emit no job event.
	for {
		jobs, err := s.st.JobsUpdatedSince(ctx, cur.jobAt, cur.jobID, sseBatch)
		if err != nil {
			s.log.Printf("ERROR sse: jobs err=%v", err)
			return
		}
		for _, j := range jobs {
			cur.jobAt = j.UpdatedAt
			cur.jobID = j.ID
			if device != "" {
				continue
			}
			if len(types) > 0 && !types["job"] {
				continue
			}
			counts, err := s.st.JobCounts(ctx, j.ID)
			if err != nil {
				s.log.Printf("ERROR sse: job counts err=%v", err)
				return
			}
			failures, err := s.st.JobFailures(ctx, j.ID)
			if err != nil {
				s.log.Printf("ERROR sse: job failures err=%v", err)
				return
			}
			id := "j" + strconv.FormatInt(j.ID, 10) + ":" +
				strconv.FormatInt(j.UpdatedAt.UnixNano(), 10)
			if !writeSSE(w, flusher, id, "job", jobView(j, counts, failures)) {
				return
			}
		}
		if len(jobs) < sseBatch {
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
