package boxd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// Client talks to a boxd daemon over its unix socket.
type Client struct {
	socket string
	hc     *http.Client
}

// NewClient builds a client for the daemon at socket.
func NewClient(socket string) *Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socket)
		},
		MaxIdleConns:    2,
		IdleConnTimeout: 30 * time.Second,
	}
	return &Client{socket: socket, hc: &http.Client{Transport: tr}}
}

// Socket is the daemon socket this client speaks to.
func (c *Client) Socket() string { return c.socket }

func (c *Client) call(ctx context.Context, method, path string, body []byte, out any) error {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://boxd"+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "text/plain")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return Errorf(CodeBoxdUnavailable, "boxd is not reachable on %s: %s", c.socket, oneLine(err.Error(), 160))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return Errorf(CodeInternal, "read boxd response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		var er ErrorResponse
		if json.Unmarshal(data, &er) == nil && er.Error.Code != "" {
			return &Error{Code: er.Error.Code, Message: er.Error.Message}
		}
		return Errorf(CodeInternal, "boxd answered HTTP %d: %s", resp.StatusCode, oneLine(string(data), 200))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return Errorf(CodeInternal, "boxd sent invalid JSON: %v", err)
	}
	return nil
}

// Info fetches the daemon info document.
func (c *Client) Info(ctx context.Context) (Info, error) {
	var out Info
	err := c.call(ctx, http.MethodGet, "/v1/info", nil, &out)
	return out, err
}

// NewSession creates a session and returns its id.
func (c *Client) NewSession(ctx context.Context, executor, model, cwd, title string) (string, error) {
	body, _ := json.Marshal(map[string]string{"executor": executor, "model": model, "cwd": cwd, "title": title})
	var out struct {
		Session string `json:"session"`
	}
	if err := c.call(ctx, http.MethodPost, "/v1/sessions", body, &out); err != nil {
		return "", err
	}
	return out.Session, nil
}

// Sessions lists every session.
func (c *Client) Sessions(ctx context.Context) ([]Session, error) {
	var out struct {
		Sessions []Session `json:"sessions"`
	}
	err := c.call(ctx, http.MethodGet, "/v1/sessions", nil, &out)
	return out.Sessions, err
}

// Session fetches one session.
func (c *Client) Session(ctx context.Context, id string) (Session, error) {
	var out Session
	err := c.call(ctx, http.MethodGet, "/v1/session/"+id, nil, &out)
	return out, err
}

// Send starts a turn and returns its number.
func (c *Client) Send(ctx context.Context, id, prompt string) (int, error) {
	var out struct {
		Session string `json:"session"`
		Turn    int    `json:"turn"`
	}
	if err := c.call(ctx, http.MethodPost, "/v1/session/"+id+"/send", []byte(prompt), &out); err != nil {
		return 0, err
	}
	return out.Turn, nil
}

// Wait blocks for a turn (0 = the latest) up to timeout.
func (c *Client) Wait(ctx context.Context, id string, turn int, timeout time.Duration) (Turn, error) {
	path := fmt.Sprintf("/v1/session/%s/wait?turn=%d&timeout=%g", id, turn, timeout.Seconds())
	var out Turn
	err := c.call(ctx, http.MethodGet, path, nil, &out)
	return out, err
}

// Events fetches the event feed after a cursor.
func (c *Client) Events(ctx context.Context, id string, since int64, limit int) ([]Event, int64, error) {
	var out struct {
		Events []Event `json:"events"`
		Next   int64   `json:"next"`
	}
	err := c.call(ctx, http.MethodGet, fmt.Sprintf("/v1/session/%s/events?since=%d&limit=%d", id, since, limit), nil, &out)
	return out.Events, out.Next, err
}

// Cancel asks boxd to stop the running turn.
func (c *Client) Cancel(ctx context.Context, id string) (Turn, error) {
	var out Turn
	err := c.call(ctx, http.MethodPost, "/v1/session/"+id+"/cancel", nil, &out)
	return out, err
}

// Close stops the session's executor and keeps its history.
func (c *Client) Close(ctx context.Context, id string) error {
	return c.call(ctx, http.MethodPost, "/v1/session/"+id+"/close", nil, nil)
}

// Reload re-reads the executor credentials and refreshes the runtime directory.
func (c *Client) Reload(ctx context.Context) (ReloadResult, error) {
	var out ReloadResult
	err := c.call(ctx, http.MethodPost, "/v1/reload", nil, &out)
	return out, err
}

// asError unwraps a coded error.
func asError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Code: CodeInternal, Message: err.Error()}
}
