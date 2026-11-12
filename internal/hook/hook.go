// Package hook calls an operator's provisioner over HTTP during a live CWMP
// session (README §7). Opticore only relays: the provisioner decides what to
// send, Opticore executes it on the device.
package hook

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Task is one RPC the provisioner wants sent, in the same shape as the API.
type Task struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

// Param is a parameter value, as carried in the Inform.
type Param struct {
	Value string `json:"value"`
	Type  string `json:"type,omitempty"`
}

// Device is the identity the provisioner needs to look the subscriber up.
type Device struct {
	ID              string `json:"id"`
	Serial          string `json:"serial"`
	OUI             string `json:"oui"`
	ProductClass    string `json:"product_class"`
	SoftwareVersion string `json:"software_version"`
}

// Result is the outcome of one task from a previous step.
type Result struct {
	Name      string          `json:"name"`
	Status    string          `json:"status"` // done | failed | unknown
	Result    json.RawMessage `json:"result,omitempty"`
	FaultCode *int            `json:"fault_code,omitempty"`
	Fault     string          `json:"fault,omitempty"`
}

// Step is one request to the provisioner.
type Step struct {
	Step     int              `json:"step"`
	Device   Device           `json:"device"`
	Events   []string         `json:"events"`
	Params   map[string]Param `json:"params,omitempty"`
	Prefetch map[string]Param `json:"prefetch,omitempty"`
	Results  []Result         `json:"results,omitempty"`
	State    string           `json:"state,omitempty"`
}

// Reply is the provisioner's answer. Empty Tasks ends the hook for the session.
type Reply struct {
	Tasks []Task `json:"tasks"`
	State string `json:"state,omitempty"`
}

// Client calls one provisioner endpoint.
type Client struct {
	url    string
	secret string
	step   time.Duration
	hc     *http.Client
}

// New builds a client. u is http://, https:// or unix:/path/to.sock.
func New(u, secret string, stepTimeout time.Duration) (*Client, error) {
	if u == "" {
		return nil, errors.New("hook: no URL")
	}
	if secret == "" {
		return nil, errors.New("hook: no secret")
	}
	if stepTimeout <= 0 {
		stepTimeout = 2 * time.Second
	}
	c := &Client{url: u, secret: secret, step: stepTimeout}
	switch {
	case strings.HasPrefix(u, "unix:"):
		path := strings.TrimPrefix(u, "unix:")
		if path == "" {
			return nil, errors.New("hook: unix URL without a path")
		}
		c.url = "http://unix/"
		c.hc = &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", path)
			},
			MaxIdleConns:    4,
			IdleConnTimeout: 90 * time.Second,
		}}
	case strings.HasPrefix(u, "http://"), strings.HasPrefix(u, "https://"):
		c.hc = &http.Client{Transport: &http.Transport{
			MaxIdleConns:        8,
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     90 * time.Second,
		}}
	default:
		return nil, fmt.Errorf("hook: %q must be http://, https:// or unix:", u)
	}
	return c, nil
}

// Step posts one step and decodes the reply.
func (c *Client) Step(ctx context.Context, s Step) (Reply, error) {
	body, err := json.Marshal(s)
	if err != nil {
		return Reply{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.step)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return Reply{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.secret)
	resp, err := c.hc.Do(req)
	if err != nil {
		return Reply{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return Reply{}, fmt.Errorf("hook: status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var r Reply
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&r); err != nil {
		return Reply{}, fmt.Errorf("hook: decode reply: %w", err)
	}
	return r, nil
}

// Handler is the provisioner side. It verifies the bearer secret and calls fn.
// A non-2xx answer from fn is not possible: return an error to send 500, which
// Opticore treats as "hook failed, session continues".
func Handler(secret string, fn func(context.Context, Step) ([]Task, error)) http.Handler {
	return HandlerFunc(secret, func(ctx context.Context, s Step) (Reply, error) {
		tasks, err := fn(ctx, s)
		return Reply{Tasks: tasks}, err
	})
}

// HandlerFunc is Handler with access to the opaque state field.
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
		json.NewEncoder(w).Encode(reply)
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
