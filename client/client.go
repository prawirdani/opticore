// Package client is the Go SDK for the Opticore /v1 HTTP API.
//
// It is a thin, standard-library-only transport over the same JSON the daemon
// serves: typed task builders, typed read models, a CWMP fault helper, an SSE
// Watch stream with automatic resume, and the provisioner-side hook handler.
// It holds no provisioning or CWMP logic of its own.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Error is a non-2xx answer from the API. Test it with errors.As.
type Error struct {
	Status  int
	Code    string
	Message string
	Details map[string]any
}

func (e *Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("opticore: %s: %s (HTTP %d)", e.Code, e.Message, e.Status)
	}
	return fmt.Sprintf("opticore: HTTP %d: %s", e.Status, e.Message)
}

// Client calls one Opticore daemon. It is safe for concurrent use.
type Client struct {
	base *url.URL
	hc   *http.Client
}

// Option tunes a Client.
type Option func(*Client)

// WithHTTPClient replaces the HTTP client. The default has no timeout so Watch
// can stay open; pass context deadlines for ordinary calls.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) { c.hc = hc }
}

// New builds a client for the daemon root, for example
// http://127.0.0.1:8081.
func New(baseURL string, opts ...Option) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	if err != nil {
		return nil, fmt.Errorf("opticore: base URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("opticore: base URL %q must be http:// or https://", baseURL)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("opticore: base URL %q has no host", baseURL)
	}
	c := &Client{base: u, hc: &http.Client{}}
	for _, o := range opts {
		o(c)
	}
	if c.hc == nil {
		c.hc = &http.Client{}
	}
	return c, nil
}

// request builds one /v1 request. parts are escaped path segments below /v1.
func (c *Client) request(ctx context.Context, method string, parts []string, q url.Values, headers map[string]string, body any) (*http.Request, error) {
	u := c.base.JoinPath(append([]string{"v1"}, parts...)...)
	if q != nil {
		u.RawQuery = q.Encode()
	}
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("opticore: encode request: %w", err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), r)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req, nil
}

// do performs a JSON request. out may be nil for responses with no body.
func (c *Client) do(ctx context.Context, method string, parts []string, q url.Values, headers map[string]string, body, out any) error {
	req, err := c.request(ctx, method, parts, q, headers, body)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return decodeError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("opticore: decode response: %w", err)
	}
	return nil
}

func decodeError(resp *http.Response) *Error {
	e := &Error{Status: resp.StatusCode}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var body struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &body) == nil && (body.Error.Code != "" || body.Error.Message != "") {
		e.Code = body.Error.Code
		e.Message = body.Error.Message
		e.Details = body.Error.Details
	} else if len(b) > 0 {
		e.Message = strings.TrimSpace(string(b))
	}
	if e.Message == "" {
		e.Message = http.StatusText(resp.StatusCode)
	}
	return e
}
