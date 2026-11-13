package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// WatchFilter narrows an SSE stream.
type WatchFilter struct {
	// Types limits the stream to Inform event types (for example "bootstrap")
	// and/or the "task" and "job" frames. Empty means everything.
	Types []string
	// DeviceID limits event and task frames to one device. Job frames are
	// aggregate and are not sent when this is set, matching the server.
	DeviceID string
	// LastEventID resumes from an SSE id such as "e42", "t7:<unixnano>" or
	// "j3:<unixnano>". Empty starts at the current end of the stream.
	LastEventID string
}

// Event is one SSE frame.
//
// Kind is the SSE event name: an Inform event type (for example "bootstrap"),
// "task", or "job". For Inform events Type carries the same type and Detail
// carries the Inform detail keys. Task and Job frames decode into Task and
// Job. Raw always holds the payload.
type Event struct {
	ID       string
	Kind     string
	Type     string
	DeviceID string
	At       time.Time
	Detail   map[string]any
	Task     *Task
	Job      *Job
	Raw      json.RawMessage
}

const (
	watchInitialBackoff = 100 * time.Millisecond
	watchMaxBackoff     = 5 * time.Second
)

// Watch opens the SSE stream and returns channels of events and of the
// terminal error, if any. It reconnects on EOF, network errors, 5xx and 429,
// resuming with Last-Event-ID set to the last frame it saw, and backs off from
// 100ms to 5s. A 4xx (other than 429) ends the stream with that *Error. Cancel
// ctx to stop; both channels then close.
func (c *Client) Watch(ctx context.Context, f WatchFilter) (<-chan Event, <-chan error) {
	events := make(chan Event)
	errs := make(chan error, 1)
	go func() {
		defer close(events)
		defer close(errs)
		last := f.LastEventID
		backoff := watchInitialBackoff
		for {
			if ctx.Err() != nil {
				return
			}
			next, n, err := c.watchOnce(ctx, f, last, events)
			if next != "" {
				last = next
			}
			if ctx.Err() != nil {
				return
			}
			var apiErr *Error
			if errors.As(err, &apiErr) && terminalStatus(apiErr.Status) {
				select {
				case errs <- err:
				case <-ctx.Done():
				}
				return
			}
			if n > 0 {
				backoff = watchInitialBackoff
			} else if backoff < watchMaxBackoff {
				backoff *= 2
				if backoff > watchMaxBackoff {
					backoff = watchMaxBackoff
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
		}
	}()
	return events, errs
}

func (c *Client) watchOnce(ctx context.Context, f WatchFilter, lastID string, out chan<- Event) (string, int, error) {
	q := url.Values{}
	if len(f.Types) > 0 {
		q.Set("type", strings.Join(f.Types, ","))
	}
	if f.DeviceID != "" {
		q.Set("device_id", f.DeviceID)
	}
	req, err := c.request(ctx, http.MethodGet, []string{"events"}, q, nil, nil)
	if err != nil {
		return lastID, 0, err
	}
	req.Header.Set("Accept", "text/event-stream")
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return lastID, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return lastID, 0, decodeError(resp)
	}
	return readSSE(ctx, resp.Body, lastID, out)
}

func readSSE(ctx context.Context, r io.Reader, lastID string, out chan<- Event) (string, int, error) {
	br := bufio.NewReader(r)
	var id, name string
	var data []string
	n := 0
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
			switch {
			case line == "":
				if id != "" && len(data) > 0 {
					if ev, perr := parseEvent(id, name, []byte(strings.Join(data, "\n"))); perr == nil {
						select {
						case out <- ev:
							n++
						case <-ctx.Done():
							return lastID, n, ctx.Err()
						}
					}
					lastID = id
				}
				id, name, data = "", "", nil
			case strings.HasPrefix(line, ":"):
				// Comment, including the server heartbeat.
			default:
				field, value, _ := strings.Cut(line, ":")
				value = strings.TrimPrefix(value, " ")
				switch field {
				case "id":
					id = value
				case "event":
					name = value
				case "data":
					data = append(data, value)
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return lastID, n, nil
			}
			return lastID, n, err
		}
	}
}

func parseEvent(id, kind string, data []byte) (Event, error) {
	ev := Event{ID: id, Kind: kind, Raw: append(json.RawMessage(nil), data...)}
	switch kind {
	case "task":
		var t Task
		if err := json.Unmarshal(data, &t); err != nil {
			return ev, err
		}
		ev.Task = &t
	case "job":
		var j Job
		if err := json.Unmarshal(data, &j); err != nil {
			return ev, err
		}
		ev.Job = &j
	default:
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			return ev, err
		}
		if raw, ok := fields["type"]; ok {
			_ = json.Unmarshal(raw, &ev.Type)
			delete(fields, "type")
		}
		if raw, ok := fields["device_id"]; ok {
			_ = json.Unmarshal(raw, &ev.DeviceID)
			delete(fields, "device_id")
		}
		if raw, ok := fields["at"]; ok {
			_ = json.Unmarshal(raw, &ev.At)
			delete(fields, "at")
		}
		ev.Detail = make(map[string]any, len(fields))
		for k, raw := range fields {
			var v any
			if json.Unmarshal(raw, &v) == nil {
				ev.Detail[k] = v
			}
		}
	}
	return ev, nil
}

func terminalStatus(status int) bool {
	if status < 400 || status >= 500 {
		return false
	}
	return status != http.StatusRequestTimeout && status != http.StatusTooManyRequests
}
