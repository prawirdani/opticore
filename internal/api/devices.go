package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/prawirdani/opticore/internal/store"
)

type paramJSON struct {
	Value     string     `json:"value"`
	Type      string     `json:"type"`
	Writable  *bool      `json:"writable"`
	UpdatedAt *time.Time `json:"updated_at"`
}

type deviceJSON struct {
	ID              string                `json:"id"`
	Serial          string                `json:"serial"`
	OUI             string                `json:"oui"`
	ProductClass    string                `json:"product_class"`
	SoftwareVersion string                `json:"software_version"`
	LastInform      time.Time             `json:"last_inform"`
	LastAddr        string                `json:"last_addr"`
	CRURL           string                `json:"cr_url"`
	CRReady         bool                  `json:"cr_ready"`
	PendingReboot   bool                  `json:"pending_reboot"`
	LastFault       json.RawMessage       `json:"last_fault"`
	Freshness       string                `json:"freshness"`
	ParamsCount     int                   `json:"params_count"`
	Params          *map[string]paramJSON `json:"params,omitempty"`
	Tasks           *map[string]int       `json:"tasks,omitempty"`
}

func freshness(last time.Time, interval int, now time.Time) string {
	if interval <= 0 {
		interval = 300
	}
	age := now.Sub(last)
	if age <= time.Duration(2*interval)*time.Second {
		return "fresh"
	}
	if age <= time.Duration(6*interval)*time.Second {
		return "stale"
	}
	return "offline"
}

func (s *Server) deviceView(d store.Device, now time.Time, reveal bool) deviceJSON {
	return deviceJSON{
		ID:              d.ID,
		Serial:          d.Serial,
		OUI:             d.OUI,
		ProductClass:    d.ProductClass,
		SoftwareVersion: d.SoftwareVersion,
		LastInform:      d.LastInform,
		LastAddr:        d.LastAddr,
		CRURL:           d.CRURL,
		CRReady:         d.CRSetAt != nil,
		PendingReboot:   d.PendingReboot,
		LastFault:       d.LastFault,
		Freshness:       freshness(d.LastInform, d.InformInterval, now),
		ParamsCount:     len(d.Params),
	}
}

func paramView(path string, p store.Param, reveal bool) paramJSON {
	v := p.Value
	if p.Secret || (!reveal && store.IsSecretPath(path)) {
		v = "••••"
	}
	return paramJSON{Value: v, Type: p.Type, Writable: p.Writable, UpdatedAt: p.UpdatedAt}
}

func (s *Server) listDevices(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := clampLimit(q.Get("limit"), 100)
	afterAt, afterID, err := decodeCursor(q.Get("cursor"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION", "invalid cursor")
		return
	}
	f := store.DevicePageFilter{
		SerialPrefix:    q.Get("serial_prefix"),
		ProductClass:    q.Get("product_class"),
		Limit:           limit,
		AfterLastInform: afterAt,
		AfterID:         afterID,
	}
	devices, err := s.st.ListDevicePage(r.Context(), f)
	if err != nil {
		s.internal(w, "list devices", err)
		return
	}
	total, err := s.st.CountDevices(r.Context(), f.SerialPrefix, f.ProductClass)
	if err != nil {
		s.internal(w, "count devices", err)
		return
	}
	now := time.Now()
	out := make([]deviceJSON, 0, len(devices))
	for _, d := range devices {
		out = append(out, s.deviceView(d, now, false))
	}
	var next *string
	if len(devices) == limit {
		c := encodeCursor(devices[len(devices)-1])
		next = &c
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data": out,
		"meta": map[string]any{"next_cursor": next, "total": total},
	})
}

func (s *Server) getDevice(w http.ResponseWriter, r *http.Request) {
	reveal := revealRequested(r)
	d, err := s.st.GetDevice(r.Context(), r.PathValue("id"))
	if err != nil {
		s.deviceError(w, err)
		return
	}
	out := s.deviceView(d, time.Now(), reveal)
	params := make(map[string]paramJSON, len(d.Params))
	for path, p := range d.Params {
		params[path] = paramView(path, p, reveal)
	}
	out.Params = &params
	summary, err := s.st.TaskSummary(r.Context(), d.ID)
	if err != nil {
		s.internal(w, "task summary", err)
		return
	}
	total := 0
	for _, n := range summary {
		total += n
	}
	summary["total"] = total
	out.Tasks = &summary
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (s *Server) listParams(w http.ResponseWriter, r *http.Request) {
	reveal := revealRequested(r)
	d, err := s.st.GetDevice(r.Context(), r.PathValue("id"))
	if err != nil {
		s.deviceError(w, err)
		return
	}
	prefix := r.URL.Query().Get("prefix")
	paths := make([]string, 0, len(d.Params))
	for path := range d.Params {
		if strings.HasPrefix(path, prefix) {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	type paramEntry struct {
		Path string `json:"path"`
		paramJSON
	}
	out := make([]paramEntry, 0, len(paths))
	for _, path := range paths {
		out = append(out, paramEntry{Path: path, paramJSON: paramView(path, d.Params[path], reveal)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	status := q.Get("status")
	if status != "" && !taskStatuses[status] {
		writeError(w, http.StatusBadRequest, "VALIDATION", "invalid task status")
		return
	}
	reveal := revealRequested(r)
	tasks, err := s.st.ListTasksFiltered(r.Context(), r.PathValue("id"), status, clampLimit(q.Get("limit"), 100))
	if err != nil {
		s.deviceError(w, err)
		return
	}
	out := make([]taskJSON, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, s.taskView(t, reveal))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var before time.Time
	if raw := q.Get("before"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "VALIDATION", "before must be RFC3339")
			return
		}
		before = t
	}
	events, err := s.st.ListEvents(r.Context(), r.PathValue("id"), clampLimit(q.Get("limit"), 100), before)
	if err != nil {
		s.deviceError(w, err)
		return
	}
	out := make([]eventJSON, 0, len(events))
	for _, e := range events {
		out = append(out, eventView(e))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

type eventJSON struct {
	ID       int64           `json:"id"`
	DeviceID string          `json:"device_id"`
	At       time.Time       `json:"at"`
	Type     string          `json:"type"`
	Detail   json.RawMessage `json:"detail"`
}

func eventView(e store.Event) eventJSON {
	return eventJSON{ID: e.ID, DeviceID: e.DeviceID, At: e.At, Type: e.Type, Detail: e.Detail}
}

func (s *Server) deviceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrDeviceNotFound):
		writeError(w, http.StatusNotFound, "DEVICE_NOT_FOUND", "no such device")
	case errors.Is(err, store.ErrTaskNotFound):
		writeError(w, http.StatusNotFound, "TASK_NOT_FOUND", "no such task")
	default:
		s.internal(w, "device request", err)
	}
}

func (s *Server) internal(w http.ResponseWriter, what string, err error) {
	s.log.Printf("ERROR %s err=%v", what, err)
	writeError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
}

func clampLimit(raw string, def int) int {
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return def
	}
	if n > 1000 {
		return 1000
	}
	return n
}

type cursorData struct {
	LastInform time.Time `json:"t"`
	ID         string    `json:"id"`
}

func encodeCursor(d store.Device) string {
	b, _ := json.Marshal(cursorData{LastInform: d.LastInform.UTC(), ID: d.ID})
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(raw string) (time.Time, string, error) {
	if raw == "" {
		return time.Time{}, "", nil
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return time.Time{}, "", err
	}
	var c cursorData
	if err := json.Unmarshal(b, &c); err != nil {
		return time.Time{}, "", err
	}
	return c.LastInform, c.ID, nil
}
