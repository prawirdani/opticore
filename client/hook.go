package client

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// Step is one request to a provisioner during a live CWMP session.
type Step struct {
	Step     int              `json:"step"`
	Device   Device           `json:"device"`
	Events   []string         `json:"events"`
	Params   map[string]Param `json:"params,omitempty"`
	Prefetch map[string]Param `json:"prefetch,omitempty"`
	Results  []Result         `json:"results,omitempty"`
	State    string           `json:"state,omitempty"`
}

// Reply is the provisioner's answer. Empty Tasks ends the hook for the
// session.
type Reply struct {
	Tasks []Task `json:"tasks"`
	State string `json:"state,omitempty"`
}

// Result is the outcome of one task from a previous step.
type Result struct {
	Name      string          `json:"name"`
	Status    string          `json:"status"` // done | failed | unknown
	Result    json.RawMessage `json:"result,omitempty"`
	FaultCode *int            `json:"fault_code,omitempty"`
	Fault     string          `json:"fault,omitempty"`
}

// HookHandler is the provisioner side. It verifies the bearer secret and calls
// fn. Return an error to send 500, which Opticore treats as "hook failed,
// session continues".
func HookHandler(secret string, fn func(context.Context, Step) ([]Task, error)) http.Handler {
	return HandlerFunc(secret, func(ctx context.Context, s Step) (Reply, error) {
		tasks, err := fn(ctx, s)
		return Reply{Tasks: tasks}, err
	})
}

// HandlerFunc is HookHandler with access to the opaque state field.
func HandlerFunc(secret string, fn func(context.Context, Step) (Reply, error)) http.Handler {
	want := "Bearer " + secret
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(want)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var s Step
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&s); err != nil {
			http.Error(w, "bad step", http.StatusBadRequest)
			return
		}
		reply, err := fn(r.Context(), s)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(reply)
	})
}

// Matches reports whether any Inform event code is on the trigger list. An
// entry matches a code that starts with it, so "0" matches "0 BOOTSTRAP".
func Matches(events, triggers []string) bool {
	for _, e := range events {
		for _, t := range triggers {
			if t != "" && strings.HasPrefix(e, t) {
				return true
			}
		}
	}
	return false
}
