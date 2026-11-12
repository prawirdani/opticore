package acs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/prawirdani/opticore/internal/cwmp"
	"github.com/prawirdani/opticore/internal/store"
)

// Sessions implements cwmp.Handler on top of the task queue.
type Sessions struct {
	Store       *store.Store
	Key         []byte
	Logger      *slog.Logger
	GPVBatch    int // exact names per GetParameterValues
	MaxAttempts int
}

// state is the per-session data kept in cwmp.Session.Data.
type state struct {
	dev      store.Device
	root     string // "InternetGatewayDevice." or "Device."
	queue    []store.Task
	reloaded bool

	crWanted bool
	crUser   string
	crPass   string

	stop     bool // end this session (retryable fault: retry on the next wake)
	cur      *store.Task
	steps    []cwmp.Request // remaining RPCs of cur
	prev     map[string][2]string
	instance map[string]int // batch id -> last add_object instance
	dead     map[string]bool
}

type tag struct {
	kind string // "cr", "task"
	step string // "prev", "get", "set", "obj", "reboot", "reset"
}

func (h *Sessions) st(s *cwmp.Session) *state { return s.Data.(*state) }

func (h *Sessions) Inform(ctx context.Context, s *cwmp.Session, in *cwmp.Inform) error {
	rec := store.InformRecord{
		ID:           s.DeviceID,
		Serial:       in.DeviceID.SerialNumber,
		OUI:          in.DeviceID.OUI,
		ProductClass: in.DeviceID.ProductClass,
		LastAddr:     s.RemoteAddr,
		At:           time.Now().UTC(),
		Params:       make(map[string]store.Param, len(in.Params)),
	}
	if t, err := time.Parse(time.RFC3339, in.CurrentTime); err == nil {
		rec.CurrentTime = t
	}
	root := ""
	now := time.Now().UTC()
	for _, p := range in.Params {
		rec.Params[p.Name] = store.Param{Value: p.Value, Type: p.Type, UpdatedAt: &now}
		switch {
		case strings.HasSuffix(p.Name, ".ManagementServer.ConnectionRequestURL"):
			rec.CRURL = p.Value
			root = p.Name[:strings.Index(p.Name, ".")+1]
		case strings.HasSuffix(p.Name, ".DeviceInfo.SoftwareVersion"):
			rec.SoftwareVersion = p.Value
		case strings.HasSuffix(p.Name, ".ManagementServer.PeriodicInformInterval"):
			rec.InformInterval, _ = strconv.Atoi(p.Value)
		}
		if root == "" {
			if i := strings.Index(p.Name, "."); i > 0 {
				root = p.Name[:i+1]
			}
		}
	}
	bootstrap := false
	for _, e := range in.Events {
		rec.Events = append(rec.Events, e.Code)
		if strings.HasPrefix(e.Code, "0 ") {
			bootstrap = true
		}
	}
	rec.Bootstrap = bootstrap
	dev, tasks, err := h.Store.RecordInform(ctx, rec)
	if err != nil {
		return err
	}
	s.Data = &state{
		dev:      dev,
		root:     root,
		queue:    tasks,
		crWanted: root != "" && (bootstrap || dev.CRSetAt == nil || dev.CRPassword == ""),
		instance: map[string]int{},
		dead:     map[string]bool{},
	}
	h.Logger.Info("inform", "device_id", s.DeviceID, "events", rec.Events,
		"retry", in.RetryCount, "pending_tasks", len(tasks), "version", s.Version)
	return nil
}

func (h *Sessions) TransferComplete(ctx context.Context, s *cwmp.Session, tc *cwmp.TransferComplete) error {
	h.Logger.Info("transfer complete", "device_id", s.DeviceID, "command_key", tc.CommandKey, "fault_code", tc.FaultCode)
	return nil
}

func (h *Sessions) Next(ctx context.Context, s *cwmp.Session) (cwmp.Request, bool, error) {
	st := h.st(s)
	if st.stop {
		return cwmp.Request{}, false, nil
	}
	if st.crWanted {
		st.crWanted = false
		st.crUser, st.crPass = randHex(6), randHex(12)
		ms := st.root + "ManagementServer."
		return cwmp.Request{
			Kind: cwmp.ReqSetParameterValues,
			Values: []cwmp.ParamValue{
				{Name: ms + "ConnectionRequestUsername", Value: st.crUser, Type: "xsd:string"},
				{Name: ms + "ConnectionRequestPassword", Value: st.crPass, Type: "xsd:string"},
			},
			Key: "opticore-cr",
			Tag: tag{kind: "cr"},
		}, true, nil
	}
	for {
		if len(st.steps) > 0 {
			req := st.steps[0]
			st.steps = st.steps[1:]
			return req, true, nil
		}
		if len(st.queue) == 0 {
			if st.reloaded {
				return cwmp.Request{}, false, nil
			}
			st.reloaded = true // pick up tasks enqueued during this session, once
			tasks, err := h.Store.PendingTasks(ctx, s.DeviceID)
			if err != nil {
				return cwmp.Request{}, false, err
			}
			st.queue = tasks
			continue
		}
		t := st.queue[0]
		st.queue = st.queue[1:]
		if t.BatchID != nil && st.dead[*t.BatchID] {
			continue
		}
		if err := h.Store.StartTask(ctx, t.ID); errors.Is(err, store.ErrTaskNotPending) {
			continue // cancelled or taken meanwhile
		} else if err != nil {
			return cwmp.Request{}, false, err
		}
		t.Attempts++
		steps, err := h.plan(st, &t)
		if err != nil {
			if err := h.fail(ctx, s, &t, 0, err.Error()); err != nil {
				return cwmp.Request{}, false, err
			}
			continue
		}
		st.cur, st.steps, st.prev = &t, steps, nil
	}
}

// plan turns a task into its RPC steps.
func (h *Sessions) plan(st *state, t *store.Task) ([]cwmp.Request, error) {
	key := "t" + strconv.FormatInt(t.ID, 10)
	switch t.Name {
	case "get":
		var a GetArgs
		if err := json.Unmarshal(t.Args, &a); err != nil {
			return nil, err
		}
		paths, err := h.subst(st, t, a.Paths)
		if err != nil {
			return nil, err
		}
		var steps []cwmp.Request
		for _, chunk := range chunkNames(paths, h.batch()) {
			steps = append(steps, cwmp.Request{Kind: cwmp.ReqGetParameterValues, Names: chunk, Tag: tag{"task", "get"}})
		}
		return steps, nil
	case "set":
		var a SetArgs
		if err := json.Unmarshal(t.Args, &a); err != nil {
			return nil, err
		}
		vals := make([]cwmp.ParamValue, len(a.Values))
		names := make([]string, len(a.Values))
		for i, v := range a.Values {
			p, err := h.subst(st, t, []string{v[0]})
			if err != nil {
				return nil, err
			}
			val := v[1]
			if strings.HasPrefix(val, encPrefix) {
				b, err := store.OpenSecret(h.Key, strings.TrimPrefix(val, encPrefix))
				if err != nil {
					return nil, fmt.Errorf("decrypt value %d: %w", i+1, err)
				}
				val = string(b)
			}
			vals[i] = cwmp.ParamValue{Name: p[0], Value: val, Type: v[2]}
			names[i] = p[0]
		}
		return []cwmp.Request{
			{Kind: cwmp.ReqGetParameterValues, Names: names, Tag: tag{"task", "prev"}},
			{Kind: cwmp.ReqSetParameterValues, Values: vals, Key: key, Tag: tag{"task", "set"}},
		}, nil
	case "add_object", "delete_object":
		var a ObjectArgs
		if err := json.Unmarshal(t.Args, &a); err != nil {
			return nil, err
		}
		p, err := h.subst(st, t, []string{a.Path})
		if err != nil {
			return nil, err
		}
		kind := cwmp.ReqAddObject
		if t.Name == "delete_object" {
			kind = cwmp.ReqDeleteObject
		}
		return []cwmp.Request{{Kind: kind, Path: p[0], Key: key, Tag: tag{"task", "obj"}}}, nil
	case "reboot":
		return []cwmp.Request{{Kind: cwmp.ReqReboot, Key: key, Tag: tag{"task", "reboot"}}}, nil
	case "reset":
		return []cwmp.Request{{Kind: cwmp.ReqFactoryReset, Tag: tag{"task", "reset"}}}, nil
	}
	return nil, fmt.Errorf("task %q is not supported yet", t.Name)
}

// subst replaces {instance} with the batch's last add_object result.
func (h *Sessions) subst(st *state, t *store.Task, paths []string) ([]string, error) {
	out := make([]string, len(paths))
	for i, p := range paths {
		if !strings.Contains(p, "{instance}") {
			out[i] = p
			continue
		}
		n, ok := 0, false
		if t.BatchID != nil {
			n, ok = st.instance[*t.BatchID]
		}
		if !ok {
			return nil, errors.New("{instance} used without an earlier add_object in the batch")
		}
		out[i] = strings.ReplaceAll(p, "{instance}", strconv.Itoa(n))
	}
	return out, nil
}

func (h *Sessions) Response(ctx context.Context, s *cwmp.Session, req cwmp.Request, m *cwmp.Message) error {
	st := h.st(s)
	tg, _ := req.Tag.(tag)

	if tg.kind == "cr" {
		if m.Kind == cwmp.KindFault {
			h.Logger.Warn("set connection-request credentials failed", "device_id", s.DeviceID,
				"fault_code", m.Fault.FaultCode, "fault", m.Fault.FaultString)
			return h.Store.SetLastFault(ctx, s.DeviceID, faultJSON(m.Fault.FaultCode, m.Fault.FaultString, 0))
		}
		enc, err := store.SealSecret(h.Key, []byte(st.crPass))
		if err != nil {
			return err
		}
		h.Logger.Info("connection-request credentials set", "device_id", s.DeviceID)
		return h.Store.SetCRCredentials(ctx, s.DeviceID, st.crUser, enc)
	}

	t := st.cur
	if t == nil {
		return nil
	}
	if m.Kind == cwmp.KindFault {
		st.steps = nil
		st.cur = nil
		return h.fault(ctx, s, t, m.Fault)
	}

	now := time.Now().UTC()
	switch tg.step {
	case "prev":
		st.prev = map[string][2]string{}
		for _, p := range m.GetParameterValuesResponse.Params {
			v := p.Value
			if store.IsSecretPath(p.Name) && v != "" {
				sealed, err := store.SealSecret(h.Key, []byte(v))
				if err != nil {
					return err
				}
				v = encPrefix + sealed
			}
			st.prev[p.Name] = [2]string{v, p.Type}
		}
		b, _ := json.Marshal(st.prev)
		return h.Store.SetTaskPrev(ctx, t.ID, b)
	case "get":
		cache := make(map[string]store.Param, len(m.GetParameterValuesResponse.Params))
		for _, p := range m.GetParameterValuesResponse.Params {
			cache[p.Name] = store.Param{Value: p.Value, Type: p.Type, UpdatedAt: &now}
		}
		if err := h.Store.MergeParams(ctx, s.DeviceID, cache); err != nil {
			return err
		}
		if len(st.steps) > 0 {
			return nil // more chunks of the same get
		}
		return h.done(ctx, s, t, map[string]any{"count": len(cache)})
	case "set":
		status := m.SetParameterValuesResponse.Status
		cache := map[string]store.Param{}
		for _, v := range req.Values {
			cache[v.Name] = store.Param{Value: v.Value, Type: v.Type, UpdatedAt: &now}
		}
		if err := h.Store.MergeParams(ctx, s.DeviceID, cache); err != nil {
			return err
		}
		if status == 1 {
			if err := h.Store.SetPendingReboot(ctx, s.DeviceID, true); err != nil {
				return err
			}
		}
		return h.done(ctx, s, t, map[string]any{"status": status})
	case "obj":
		res := map[string]any{}
		if m.Kind == cwmp.KindAddObjectResponse {
			n := m.AddObjectResponse.InstanceNumber
			res["instance"], res["status"] = n, m.AddObjectResponse.Status
			if t.BatchID != nil {
				st.instance[*t.BatchID] = n
			}
		} else {
			res["status"] = m.DeleteObjectResponse.Status
		}
		return h.done(ctx, s, t, res)
	case "reboot", "reset":
		return h.done(ctx, s, t, map[string]any{})
	}
	return nil
}

func (h *Sessions) done(ctx context.Context, s *cwmp.Session, t *store.Task, result map[string]any) error {
	h.st(s).cur = nil
	b, _ := json.Marshal(result)
	h.Logger.Info("task done", "device_id", s.DeviceID, "task", t.ID, "name", t.Name)
	return h.Store.FinishTask(ctx, t.ID, "done", b, nil, "")
}

// fault applies the retry policy of §6.
func (h *Sessions) fault(ctx context.Context, s *cwmp.Session, t *store.Task, f cwmp.Fault) error {
	retryable := f.FaultCode == 9002 || f.FaultCode == 9004
	if retryable && t.Attempts < h.maxAttempts() && idempotent(t.Name) {
		h.Logger.Warn("task fault, will retry", "device_id", s.DeviceID, "task", t.ID, "fault_code", f.FaultCode)
		h.st(s).stop = true // retry on the next wake, not in this session
		return h.Store.RequeueTask(ctx, t.ID)
	}
	msg := f.FaultString
	for _, pf := range f.SetParameterValuesFault {
		msg += fmt.Sprintf("; %s: %d %s", pf.ParameterName, pf.FaultCode, pf.FaultString)
	}
	return h.fail(ctx, s, t, f.FaultCode, msg)
}

func (h *Sessions) fail(ctx context.Context, s *cwmp.Session, t *store.Task, code int, msg string) error {
	h.Logger.Warn("task failed", "device_id", s.DeviceID, "task", t.ID, "name", t.Name, "fault_code", code, "fault", msg)
	var fc *int
	if code != 0 {
		fc = &code
	}
	if err := h.Store.FinishTask(ctx, t.ID, "failed", nil, fc, msg); err != nil {
		return err
	}
	if err := h.Store.SetLastFault(ctx, s.DeviceID, faultJSON(code, msg, t.ID)); err != nil {
		return err
	}
	if t.BatchID != nil {
		h.skipBatch(s, t)
		return h.Store.CancelBatchRest(ctx, *t.BatchID, t.BatchSeq)
	}
	return nil
}

func (h *Sessions) skipBatch(s *cwmp.Session, t *store.Task) {
	if t.BatchID != nil {
		h.st(s).dead[*t.BatchID] = true
	}
}

func (h *Sessions) Close(s *cwmp.Session, inflight *cwmp.Request) {
	st, ok := s.Data.(*state)
	if !ok || st.cur == nil {
		return
	}
	t := st.cur
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var tg tag
	if inflight != nil {
		tg, _ = inflight.Tag.(tag)
	}
	var err error
	if inflight == nil || tg.step == "prev" || idempotent(t.Name) {
		// Nothing irreversible was sent (or it is safe to resend): run again later.
		err = h.Store.RequeueTask(ctx, t.ID)
	} else {
		h.Logger.Warn("session lost mid-RPC; task outcome unknown", "device_id", s.DeviceID, "task", t.ID, "name", t.Name)
		err = h.Store.FinishTask(ctx, t.ID, "unknown", nil, nil, "session ended before the device answered")
		if err == nil && t.BatchID != nil {
			err = h.Store.CancelBatchRest(ctx, *t.BatchID, t.BatchSeq)
		}
	}
	if err != nil {
		h.Logger.Error("close session", "device_id", s.DeviceID, "task", t.ID, "err", err)
	}
}

// idempotent tasks are safe to resend after a lost response (§5).
func idempotent(name string) bool {
	return slices.Contains([]string{"get", "set", "delete_object"}, name)
}

func (h *Sessions) batch() int {
	if h.GPVBatch > 0 {
		return h.GPVBatch
	}
	return 32
}

func (h *Sessions) maxAttempts() int {
	if h.MaxAttempts > 0 {
		return h.MaxAttempts
	}
	return 5
}

// chunkNames batches exact names; a partial path (trailing ".") goes alone,
// since the device expands it.
func chunkNames(paths []string, n int) [][]string {
	var out [][]string
	var cur []string
	for _, p := range paths {
		if strings.HasSuffix(p, ".") {
			out = append(out, []string{p})
			continue
		}
		cur = append(cur, p)
		if len(cur) == n {
			out, cur = append(out, cur), nil
		}
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

func faultJSON(code int, msg string, taskID int64) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"code": code, "string": msg, "task_id": taskID, "at": time.Now().UTC()})
	return b
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
