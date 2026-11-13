package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prawirdani/opticore/internal/acs"
	"github.com/prawirdani/opticore/internal/store"
)

const encPrefix = "enc:"

var taskNames = map[string]bool{
	"get": true, "set": true, "add_object": true, "delete_object": true,
	"reboot": true, "reset": true, "ping": true,
}

var taskStatuses = map[string]bool{
	"pending": true, "running": true, "done": true, "failed": true,
	"unknown": true, "cancelled": true, "expired": true,
}

type taskJSON struct {
	ID        int64           `json:"id"`
	Name      string          `json:"name"`
	Args      json.RawMessage `json:"args"`
	Prev      json.RawMessage `json:"prev"`
	Result    json.RawMessage `json:"result"`
	Status    string          `json:"status"`
	Attempts  int             `json:"attempts"`
	FaultCode *int            `json:"fault_code"`
	Fault     string          `json:"fault"`
	CreatedAt time.Time       `json:"created_at"`
	RunAt     time.Time       `json:"run_at"`
	ExpiresAt time.Time       `json:"expires_at"`
	UpdatedAt time.Time       `json:"updated_at"`
	BatchID   *string         `json:"batch_id"`
	BatchSeq  int             `json:"batch_seq"`
}

func (s *Server) taskView(t store.Task, reveal bool) taskJSON {
	return taskJSON{
		ID:        t.ID,
		Name:      t.Name,
		Args:      s.scrub(t.Args, reveal),
		Prev:      s.scrub(t.Prev, reveal),
		Result:    t.Result,
		Status:    t.Status,
		Attempts:  t.Attempts,
		FaultCode: t.FaultCode,
		Fault:     t.Fault,
		CreatedAt: t.CreatedAt,
		RunAt:     t.RunAt,
		ExpiresAt: t.ExpiresAt,
		UpdatedAt: t.UpdatedAt,
		BatchID:   t.BatchID,
		BatchSeq:  t.BatchSeq,
	}
}

// scrub masks sealed ("enc:") values and, with reveal, decrypts them.
func (s *Server) scrub(raw json.RawMessage, reveal bool) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	v = s.walk(v, reveal)
	out, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return out
}

func (s *Server) walk(v any, reveal bool) any {
	switch x := v.(type) {
	case map[string]any:
		for k := range x {
			x[k] = s.walk(x[k], reveal)
		}
		return x
	case []any:
		for i := range x {
			x[i] = s.walk(x[i], reveal)
		}
		return x
	case string:
		if !strings.HasPrefix(x, encPrefix) {
			return x
		}
		if reveal {
			if plain, err := store.OpenSecret(s.key, strings.TrimPrefix(x, encPrefix)); err == nil {
				return string(plain)
			}
		}
		return "••••"
	default:
		return v
	}
}

type taskInput struct {
	Name      string          `json:"name"`
	Args      json.RawMessage `json:"args"`
	RunAt     *time.Time      `json:"run_at"`
	ExpiresAt *time.Time      `json:"expires_at"`
}

func (s *Server) createTasks(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION", "request body too large")
		return
	}
	inputs, err := decodeTaskInputs(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION", err.Error())
		return
	}
	idem := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	queued := make([]store.NewTask, 0, len(inputs))
	for i, in := range inputs {
		if !taskNames[in.Name] {
			writeError(w, http.StatusBadRequest, "VALIDATION", "unknown task name "+strconv.Quote(in.Name))
			return
		}
		args := in.Args
		if len(args) == 0 || bytes.Equal(args, []byte("null")) {
			args = json.RawMessage(`{}`)
		}
		t := store.NewTask{Name: in.Name, Args: args}
		if in.RunAt != nil {
			t.RunAt = *in.RunAt
		}
		t.ExpiresAt = in.ExpiresAt
		// The unique index is per (device, key), so a batch derives one key
		// per task and a replay of the same batch finds the same rows.
		if idem != "" {
			key := idem
			if len(inputs) > 1 {
				key = idem + "#" + strconv.Itoa(i+1)
			}
			t.IdemKey = &key
		}
		queued = append(queued, t)
	}

	out, err := s.svc.Enqueue(r.Context(), r.PathValue("id"), queued)
	if err != nil {
		s.enqueueError(w, err)
		return
	}
	views := make([]taskJSON, 0, len(out))
	for _, t := range out {
		views = append(views, s.taskView(t, false))
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"data": views})
}

func decodeTaskInputs(body []byte) ([]taskInput, error) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil, errors.New("empty request body")
	}
	if body[0] == '[' {
		var inputs []taskInput
		if err := json.Unmarshal(body, &inputs); err != nil {
			return nil, errors.New("invalid task array")
		}
		if len(inputs) == 0 {
			return nil, errors.New("task array is empty")
		}
		return inputs, nil
	}
	var one taskInput
	if err := json.Unmarshal(body, &one); err != nil {
		return nil, errors.New("invalid task object")
	}
	return []taskInput{one}, nil
}

func (s *Server) enqueueError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrDeviceNotFound):
		writeError(w, http.StatusNotFound, "DEVICE_NOT_FOUND", "no such device")
	case errors.Is(err, acs.ErrReservedPath):
		writeError(w, http.StatusBadRequest, "VALIDATION", acs.ErrReservedPath.Error())
	case strings.HasPrefix(err.Error(), "store:"):
		s.internal(w, "enqueue tasks", err)
	default:
		writeError(w, http.StatusBadRequest, "VALIDATION", err.Error())
	}
}

func (s *Server) cancelTask(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("taskId"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION", "task id must be an integer")
		return
	}
	switch err := s.st.CancelTask(r.Context(), id); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, store.ErrTaskNotFound):
		writeError(w, http.StatusNotFound, "TASK_NOT_FOUND", "no such task")
	case errors.Is(err, store.ErrTaskNotPending):
		writeError(w, http.StatusConflict, "CONFLICT", "task is not pending")
	default:
		s.internal(w, "cancel task", err)
	}
}

func (s *Server) wakeDevice(w http.ResponseWriter, r *http.Request) {
	d, err := s.st.GetDevice(r.Context(), r.PathValue("id"))
	if err != nil {
		s.deviceError(w, err)
		return
	}
	if d.CRURL == "" {
		writeError(w, http.StatusConflict, "CONFLICT", "device has no ConnectionRequestURL")
		return
	}
	if s.svc == nil {
		writeError(w, http.StatusBadGateway, "DEVICE_UNREACHABLE", "waker is not configured")
		return
	}
	if err := s.svc.Wake(r.Context(), d.ID); err != nil {
		writeError(w, http.StatusBadGateway, "DEVICE_UNREACHABLE", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"data": map[string]bool{"woken": true}})
}

func (s *Server) deleteDevice(w http.ResponseWriter, r *http.Request) {
	switch err := s.st.DeleteDevice(r.Context(), r.PathValue("id")); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, store.ErrDeviceNotFound):
		writeError(w, http.StatusNotFound, "DEVICE_NOT_FOUND", "no such device")
	default:
		s.internal(w, "delete device", err)
	}
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request) {
	reveal := s.requireReveal(w, r)
	if r.URL.Query().Get("reveal") == "1" && !reveal {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("taskId"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION", "task id must be an integer")
		return
	}
	t, err := s.st.GetTask(r.Context(), id)
	if err != nil {
		s.deviceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": s.taskView(t, reveal)})
}
