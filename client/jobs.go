package client

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
)

// CreateJob expands a job spec into per-device tasks.
func (c *Client) CreateJob(ctx context.Context, spec JobSpec) (*Job, error) {
	var out struct {
		Data Job `json:"data"`
	}
	if err := c.do(ctx, http.MethodPost, []string{"jobs"}, nil, nil, spec, &out); err != nil {
		return nil, err
	}
	return &out.Data, nil
}

// ListJobs returns recent jobs.
func (c *Client) ListJobs(ctx context.Context, limit int) ([]Job, error) {
	var q url.Values
	if limit > 0 {
		q = url.Values{"limit": []string{strconv.Itoa(limit)}}
	}
	var out struct {
		Data []Job `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, []string{"jobs"}, q, nil, nil, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

// GetJob returns one job with its counts and failures.
func (c *Client) GetJob(ctx context.Context, id int64) (*Job, error) {
	var out struct {
		Data Job `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, []string{"jobs", strconv.FormatInt(id, 10)}, nil, nil, nil, &out); err != nil {
		return nil, err
	}
	return &out.Data, nil
}

// CancelJob cancels the pending tasks of a job; running ones finish.
func (c *Client) CancelJob(ctx context.Context, id int64) error {
	return c.do(ctx, http.MethodDelete, []string{"jobs", strconv.FormatInt(id, 10)}, nil, nil, nil, nil)
}

// RevertJob creates a new job with one revert set per done set task.
func (c *Client) RevertJob(ctx context.Context, id int64) (*Job, error) {
	var out struct {
		Data Job `json:"data"`
	}
	if err := c.do(ctx, http.MethodPost, []string{"jobs", strconv.FormatInt(id, 10), "revert"},
		nil, nil, nil, &out); err != nil {
		return nil, err
	}
	return &out.Data, nil
}
