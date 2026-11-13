// Package hook calls an operator's provisioner over HTTP during a live CWMP
// session (README §7). Opticore only relays: the provisioner decides what to
// send, Opticore executes it on the device.
package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/prawirdani/opticore/client"
)

// The provisioner-side wire types live in the public client package so a Go
// provisioner and this caller share one definition. These aliases keep the
// names internal/acs already uses.
type (
	Task   = client.Task
	Param  = client.Param
	Device = client.Device
	Result = client.Result
	Step   = client.Step
	Reply  = client.Reply
)

// Client calls one provisioner endpoint.
type Client struct {
	url  string
	step time.Duration
	hc   *http.Client
}

// New builds a client. u is http://, https:// or unix:/path/to.sock.
func New(u string, stepTimeout time.Duration) (*Client, error) {
	if u == "" {
		return nil, errors.New("hook: no URL")
	}
	if stepTimeout <= 0 {
		stepTimeout = 2 * time.Second
	}
	c := &Client{url: u, step: stepTimeout}
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
