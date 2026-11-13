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

	"github.com/prawirdani/opticore/internal/store"
)

type jobInput struct {
	Name        string            `json:"name"`
	Task        taskInput         `json:"task"`
	Select      store.JobSelector `json:"select"`
	MaxParallel int               `json:"max_parallel"`
	RunAt       *time.Time        `json:"run_at"`
}

type jobJSON struct {
	ID          int64              `json:"id"`
	Name        string             `json:"name"`
	Status      string             `json:"status"`
	MaxParallel int                `json:"max_parallel"`
	CreatedAt   time.Time          `json:"created_at"`
	Counts      map[string]int     `json:"counts"`
	Failures    []store.JobFailure `json:"failures,omitempty"`
}

func jobView(j store.Job, counts map[string]int, failures []store.JobFailure) jobJSON {
	total := 0
	for _, n := range counts {
		total += n
	}
	counts["total"] = total
	return jobJSON{
		ID:          j.ID,
		Name:        j.Name,
		Status:      j.Status,
		MaxParallel: j.MaxParallel,
		CreatedAt:   j.CreatedAt,
		Counts:      counts,
		Failures:    failures,
	}
}

func (s *Server) createJob(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION", "request body too large")
		return
	}
	var in jobInput
	if err := json.Unmarshal(bytes.TrimSpace(body), &in); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION", "invalid job object")
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		writeError(w, http.StatusBadRequest, "VALIDATION", "job name is required")
		return
	}
	if in.Task.Name == "" {
		writeError(w, http.StatusBadRequest, "VALIDATION", "task name is required")
		return
	}
	args := in.Task.Args
	if len(args) == 0 || bytes.Equal(args, []byte("null")) {
		args = json.RawMessage(`{}`)
	}
	spec, err := json.Marshal(in)
	if err != nil {
		s.internal(w, "encode job spec", err)
		return
	}
	j := store.NewJob{
		Name:        in.Name,
		Spec:        spec,
		Selector:    in.Select,
		Task:        store.NewTask{Name: in.Task.Name, Args: args},
		MaxParallel: in.MaxParallel,
	}
	if in.RunAt != nil {
		j.RunAt = *in.RunAt
	}

	job, _, err := s.svc.CreateJob(r.Context(), j)
	if err != nil {
		s.jobError(w, "create job", err)
		return
	}
	// Creation already expanded to tasks; wake only the first max_parallel.
	if err := s.svc.PumpJobs(r.Context()); err != nil {
		s.log.Error("pump jobs after create", "err", err)
	}
	counts, err := s.st.JobCounts(r.Context(), job.ID)
	if err != nil {
		s.internal(w, "job counts", err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"data": jobView(job, counts, nil)})
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.st.ListJobs(r.Context(), clampLimit(r.URL.Query().Get("limit"), 100))
	if err != nil {
		s.internal(w, "list jobs", err)
		return
	}
	out := make([]jobJSON, 0, len(jobs))
	for _, j := range jobs {
		counts, err := s.st.JobCounts(r.Context(), j.ID)
		if err != nil {
			s.internal(w, "job counts", err)
			return
		}
		out = append(out, jobView(j, counts, nil))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION", "job id must be an integer")
		return
	}
	job, err := s.st.GetJob(r.Context(), id)
	if err != nil {
		s.jobError(w, "get job", err)
		return
	}
	counts, err := s.st.JobCounts(r.Context(), job.ID)
	if err != nil {
		s.internal(w, "job counts", err)
		return
	}
	failures, err := s.st.JobFailures(r.Context(), job.ID)
	if err != nil {
		s.internal(w, "job failures", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": jobView(job, counts, failures)})
}

func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION", "job id must be an integer")
		return
	}
	if err := s.st.CancelJob(r.Context(), id); err != nil {
		s.jobError(w, "cancel job", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) jobError(w http.ResponseWriter, what string, err error) {
	var unknown *store.UnknownDevicesError
	switch {
	case errors.As(err, &unknown):
		writeError(w, http.StatusBadRequest, "VALIDATION", unknown.Error())
	case errors.Is(err, store.ErrJobNotFound):
		writeError(w, http.StatusNotFound, "JOB_NOT_FOUND", "no such job")
	case errors.Is(err, store.ErrJobSelectorEmpty), errors.Is(err, store.ErrJobNoDevices):
		writeError(w, http.StatusBadRequest, "VALIDATION", err.Error())
	case strings.HasPrefix(err.Error(), "store:"):
		s.internal(w, what, err)
	default:
		writeError(w, http.StatusBadRequest, "VALIDATION", err.Error())
	}
}
