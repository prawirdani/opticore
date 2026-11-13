package client

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// ListDevicesOptions filters GET /v1/devices.
type ListDevicesOptions struct {
	SerialPrefix string
	ProductClass string
	Cursor       string
	Limit        int
	Reveal       bool
}

// DevicePage is one page of GET /v1/devices.
type DevicePage struct {
	Devices    []Device
	NextCursor *string
	Total      int
}

// ListDevices returns one page of the device registry.
func (c *Client) ListDevices(ctx context.Context, opt ListDevicesOptions) (DevicePage, error) {
	q := url.Values{}
	if opt.SerialPrefix != "" {
		q.Set("serial_prefix", opt.SerialPrefix)
	}
	if opt.ProductClass != "" {
		q.Set("product_class", opt.ProductClass)
	}
	if opt.Cursor != "" {
		q.Set("cursor", opt.Cursor)
	}
	if opt.Limit > 0 {
		q.Set("limit", strconv.Itoa(opt.Limit))
	}
	if opt.Reveal {
		q.Set("reveal", "1")
	}
	var out struct {
		Data []Device `json:"data"`
		Meta struct {
			NextCursor *string `json:"next_cursor"`
			Total      int     `json:"total"`
		} `json:"meta"`
	}
	if err := c.do(ctx, http.MethodGet, []string{"devices"}, q, nil, nil, &out); err != nil {
		return DevicePage{}, err
	}
	return DevicePage{Devices: out.Data, NextCursor: out.Meta.NextCursor, Total: out.Meta.Total}, nil
}

// GetDevice returns one device, including its parameter cache and task counts.
func (c *Client) GetDevice(ctx context.Context, id string, reveal bool) (*Device, error) {
	var out struct {
		Data Device `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, []string{"devices", id}, revealQuery(reveal), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out.Data, nil
}

// ListParams returns the cached parameters of a device, optionally under a
// prefix.
func (c *Client) ListParams(ctx context.Context, id, prefix string, reveal bool) ([]Param, error) {
	q := revealQuery(reveal)
	if prefix != "" {
		q.Set("prefix", prefix)
	}
	var out struct {
		Data []Param `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, []string{"devices", id, "params"}, q, nil, nil, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

// ListTasksOptions filters GET /v1/devices/{id}/tasks.
type ListTasksOptions struct {
	Status string
	Limit  int
	Reveal bool
}

// ListTasks returns the tasks of one device, newest first.
func (c *Client) ListTasks(ctx context.Context, deviceID string, opt ListTasksOptions) ([]Task, error) {
	q := revealQuery(opt.Reveal)
	if opt.Status != "" {
		q.Set("status", opt.Status)
	}
	if opt.Limit > 0 {
		q.Set("limit", strconv.Itoa(opt.Limit))
	}
	var out struct {
		Data []Task `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, []string{"devices", deviceID, "tasks"}, q, nil, nil, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

// ListEventsOptions filters GET /v1/devices/{id}/events.
type ListEventsOptions struct {
	Before time.Time
	Limit  int
}

// ListEvents returns the Inform events of one device.
func (c *Client) ListEvents(ctx context.Context, deviceID string, opt ListEventsOptions) ([]EventRecord, error) {
	q := url.Values{}
	if !opt.Before.IsZero() {
		q.Set("before", opt.Before.UTC().Format(time.RFC3339))
	}
	if opt.Limit > 0 {
		q.Set("limit", strconv.Itoa(opt.Limit))
	}
	var out struct {
		Data []EventRecord `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, []string{"devices", deviceID, "events"}, q, nil, nil, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

// DeleteDevice forgets a device. It registers again on its next Inform.
func (c *Client) DeleteDevice(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, []string{"devices", id}, nil, nil, nil, nil)
}

func revealQuery(reveal bool) url.Values {
	q := url.Values{}
	if reveal {
		q.Set("reveal", "1")
	}
	return q
}
