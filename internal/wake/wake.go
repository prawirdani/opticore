// Package wake sends TR-069 Connection Requests to devices so they open a CWMP
// session now instead of waiting for the next periodic inform.
package wake

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

var (
	// ErrUnauthorized means the device still answered 401 after an auth attempt.
	ErrUnauthorized = errors.New("wake: unauthorized")
	// ErrUnreachable wraps network errors and timeouts after the retry.
	ErrUnreachable = errors.New("wake: unreachable")
)

// StatusError is returned when the device answers with a non-2xx status.
type StatusError struct {
	Code int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("wake: unexpected status %d", e.Code)
}

// Target is one device's connection request endpoint and credentials.
type Target struct {
	DeviceID string
	URL      string
	Username string
	Password string
	// AllowBasic permits falling back to Basic auth when the device offers it.
	AllowBasic bool
}

// Waker sends connection requests over one pooled HTTP client.
type Waker struct {
	client  *http.Client
	timeout time.Duration
	logger  *slog.Logger

	// Window is the coalescing delay for Request. Default 500 ms.
	Window time.Duration

	mu         sync.Mutex
	challenges map[string]*digestChallenge
	pending    map[string]Target
	timers     map[string]*time.Timer
	inflight   map[string]bool
	closed     bool
}

// New builds a Waker. A non-positive timeout defaults to 2 s. A nil logger is
// discarded.
func New(timeout time.Duration, logger *slog.Logger) *Waker {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Waker{
		client: &http.Client{
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				DialContext:         (&net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}).DialContext,
				MaxIdleConns:        16,
				MaxIdleConnsPerHost: 2,
				IdleConnTimeout:     90 * time.Second,
				DisableCompression:  true,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		timeout:    timeout,
		logger:     logger,
		Window:     500 * time.Millisecond,
		challenges: make(map[string]*digestChallenge),
		pending:    make(map[string]Target),
		timers:     make(map[string]*time.Timer),
		inflight:   make(map[string]bool),
	}
}

// Wake performs one connection request. A cached Digest challenge is pre-sent;
// a 401 is retried once with the fresh challenge. Network errors get one retry.
func (w *Waker) Wake(ctx context.Context, t Target) error {
	uri := requestURI(t.URL)
	auth := w.nextAuth(t, uri)

	code, hdr, err := w.attempt(ctx, t, auth)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnreachable, err)
	}

	if code == http.StatusUnauthorized {
		digest, basic := parseChallenges(hdr.Values("Www-Authenticate"))
		switch {
		case digest != nil:
			w.saveChallenge(t.DeviceID, digest)
			auth = w.nextAuth(t, uri)
		case basic && t.AllowBasic:
			auth = basicAuth(t.Username, t.Password)
		default:
			return ErrUnauthorized
		}

		code, hdr, err = w.attempt(ctx, t, auth)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrUnreachable, err)
		}
		if code == http.StatusUnauthorized {
			return ErrUnauthorized
		}
	}

	if code >= 200 && code < 300 {
		return nil
	}
	return &StatusError{Code: code}
}

// Request schedules a coalesced wake. Calls for the same device within Window,
// or while a wake is in flight, collapse into one.
func (w *Waker) Request(t Target) {
	w.mu.Lock()
	if w.closed || w.inflight[t.DeviceID] {
		w.mu.Unlock()
		return
	}
	w.pending[t.DeviceID] = t // latest target wins
	if _, ok := w.timers[t.DeviceID]; !ok {
		id := t.DeviceID
		w.timers[id] = time.AfterFunc(w.Window, func() { w.fire(id) })
	}
	w.mu.Unlock()
}

// Close stops pending timers and closes idle connections.
func (w *Waker) Close() {
	w.mu.Lock()
	w.closed = true
	for id, timer := range w.timers {
		timer.Stop()
		delete(w.timers, id)
	}
	w.mu.Unlock()
	w.client.CloseIdleConnections()
}

func (w *Waker) fire(id string) {
	w.mu.Lock()
	delete(w.timers, id)
	if w.closed {
		w.mu.Unlock()
		return
	}
	t, ok := w.pending[id]
	delete(w.pending, id)
	if !ok {
		w.mu.Unlock()
		return
	}
	w.inflight[id] = true
	w.mu.Unlock()

	go w.run(t)
}

func (w *Waker) run(t Target) {
	defer func() {
		w.mu.Lock()
		delete(w.inflight, t.DeviceID)
		w.mu.Unlock()
	}()

	start := time.Now()
	// Bounded overall context: one attempt plus one retry.
	ctx, cancel := context.WithTimeout(context.Background(), 3*w.timeout)
	defer cancel()

	err := w.Wake(ctx, t)
	attrs := []any{
		"device_id", t.DeviceID,
		"url", t.URL,
		"err", err,
		"duration", time.Since(start),
	}
	if err != nil {
		w.logger.Warn("wake failed", attrs...)
		return
	}
	w.logger.Info("wake ok", attrs...)
}

// attempt sends the request and retries once on a network error.
func (w *Waker) attempt(ctx context.Context, t Target, auth string) (int, http.Header, error) {
	code, hdr, err := w.do(ctx, t, auth)
	if err == nil || ctx.Err() != nil {
		return code, hdr, err
	}
	return w.do(ctx, t, auth)
}

func (w *Waker) do(ctx context.Context, t Target, auth string) (int, http.Header, error) {
	actx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(actx, http.MethodGet, t.URL, nil)
	if err != nil {
		return 0, nil, err
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}

	resp, err := w.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, resp.Header.Clone(), nil
}

// nextAuth returns the pre-sent Authorization value for the device, if a
// Digest challenge is cached. It increments the nonce count.
func (w *Waker) nextAuth(t Target, uri string) string {
	w.mu.Lock()
	defer w.mu.Unlock()

	c, ok := w.challenges[t.DeviceID]
	if !ok {
		return ""
	}
	c.NC++
	cp := *c
	return cp.authorization(http.MethodGet, uri, t.Username, t.Password)
}

func (w *Waker) saveChallenge(id string, c *digestChallenge) {
	w.mu.Lock()
	c.NC = 0
	w.challenges[id] = c
	w.mu.Unlock()
}

func basicAuth(username, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password))
}

func requestURI(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "/"
	}
	if r := u.RequestURI(); r != "" {
		return r
	}
	return "/"
}
