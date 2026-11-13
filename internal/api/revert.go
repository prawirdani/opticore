package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/prawirdani/opticore/internal/acs"
	"github.com/prawirdani/opticore/internal/store"
)

// revertTask queues one revert set for a done set task. The response shape and
// the Idempotency-Key handling match createTasks.
func (s *Server) revertTask(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("taskId"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION", "task id must be an integer")
		return
	}
	var idem *string
	if k := strings.TrimSpace(r.Header.Get("Idempotency-Key")); k != "" {
		idem = &k
	}
	out, err := s.svc.RevertTask(r.Context(), r.PathValue("id"), id, idem)
	if err != nil {
		s.revertError(w, err)
		return
	}
	views := make([]taskJSON, 0, len(out))
	for _, t := range out {
		views = append(views, s.taskView(t))
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"data": views})
}

// revertJob creates one revert set per done set task of a job, as a new job
// with the original max_parallel cap.
func (s *Server) revertJob(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION", "job id must be an integer")
		return
	}
	job, _, err := s.svc.RevertJob(r.Context(), id)
	if err != nil {
		s.revertJobError(w, err)
		return
	}
	// Like createJob: wake only up to the new job's max_parallel.
	if err := s.svc.PumpJobs(r.Context()); err != nil {
		s.log.Printf("ERROR pump jobs after revert err=%v", err)
	}
	counts, err := s.st.JobCounts(r.Context(), job.ID)
	if err != nil {
		s.internal(w, "job counts", err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"data": jobView(job, counts, nil)})
}

func (s *Server) revertError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrDeviceNotFound):
		writeError(w, http.StatusNotFound, "DEVICE_NOT_FOUND", "no such device")
	case errors.Is(err, store.ErrTaskNotFound):
		writeError(w, http.StatusNotFound, "TASK_NOT_FOUND", "no such task")
	case errors.Is(err, acs.ErrNotASetTask), errors.Is(err, acs.ErrTaskNotDone),
		errors.Is(err, acs.ErrNoPrev):
		writeError(w, http.StatusConflict, "CONFLICT", err.Error())
	case strings.HasPrefix(err.Error(), "store:"):
		s.internal(w, "revert task", err)
	default:
		writeError(w, http.StatusBadRequest, "VALIDATION", err.Error())
	}
}

func (s *Server) revertJobError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrJobNotFound):
		writeError(w, http.StatusNotFound, "JOB_NOT_FOUND", "no such job")
	case errors.Is(err, acs.ErrNothingToRevert):
		writeError(w, http.StatusConflict, "CONFLICT", err.Error())
	case strings.HasPrefix(err.Error(), "store:"):
		s.internal(w, "revert job", err)
	default:
		writeError(w, http.StatusBadRequest, "VALIDATION", err.Error())
	}
}
