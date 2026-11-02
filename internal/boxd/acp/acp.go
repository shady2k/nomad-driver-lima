// Package acp is a small client for the Agent Client Protocol over stdio:
// newline-delimited JSON-RPC 2.0, one session per agent process.
//
// It keeps the parts boxd needs: initialize, session/new, session/prompt,
// session/cancel (a notification), session/close, the session/update
// notification stream, and agent-to-client permission requests.
package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"
)

// ProtocolVersion is the ACP version boxd speaks.
const ProtocolVersion = 1

// RPCError is a JSON-RPC error object.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	if len(e.Data) > 0 {
		return fmt.Sprintf("rpc error %d: %s (%s)", e.Code, e.Message, string(e.Data))
	}
	return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message)
}

// Message is one JSON-RPC message in either direction.
type Message struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *RPCError        `json:"error,omitempty"`
}

// Handler answers a request the agent sends to the client.
type Handler func(method string, params json.RawMessage) (any, error)

// Client drives one agent process over its stdin/stdout.
type Client struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	out   *bufio.Reader

	writeMu sync.Mutex

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan *Message
	handler Handler

	// updateMu guards notification handling: the update callback may be any
	// function the owner sets before the first prompt.
	updateMu sync.Mutex
	onUpdate func(params json.RawMessage)

	closeOnce sync.Once
	done      chan struct{} // closed when the read loop stops
	exitErr   error         // set before done is closed
	reaped    chan struct{} // closed when the process is reaped and exit code known
	exitCode  int
}

// NewClient starts watching a process that has already been started with
// stdin/stdout pipes. cmd's Stdin/Stdout must be pipes created by the caller;
// stderr is the caller's business.
func NewClient(cmd *exec.Cmd) (*Client, error) {
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	c := &Client{
		cmd:     cmd,
		stdin:   in,
		out:     bufio.NewReaderSize(out, 1<<20),
		pending: map[int64]chan *Message{},
		done:    make(chan struct{}),
		reaped:  make(chan struct{}),
	}
	return c, nil
}

// Start starts the process and the read loop.
func (c *Client) Start() error {
	if err := c.cmd.Start(); err != nil {
		return err
	}
	go c.readLoop()
	go func() {
		err := c.cmd.Wait()
		c.mu.Lock()
		c.exitErr = err
		if c.cmd.ProcessState != nil {
			c.exitCode = c.cmd.ProcessState.ExitCode()
		}
		c.mu.Unlock()
		close(c.reaped)
		c.closeDone(nil)
	}()
	return nil
}

// Pid is the agent process id.
func (c *Client) Pid() int {
	if c.cmd.Process == nil {
		return 0
	}
	return c.cmd.Process.Pid
}

// ExitCode is valid after Reaped returns.
func (c *Client) ExitCode() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.exitCode
}

// Reaped is closed once the process has exited and its code is known.
func (c *Client) Reaped() <-chan struct{} { return c.reaped }

// Done is closed when the connection is unusable (process exit or read error).
func (c *Client) Done() <-chan struct{} { return c.done }

// Err reports why the connection ended, if it ended with an error.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.exitErr
}

// OnUpdate sets the handler for session/update notifications.
func (c *Client) OnUpdate(fn func(params json.RawMessage)) {
	c.updateMu.Lock()
	c.onUpdate = fn
	c.updateMu.Unlock()
}

// OnRequest sets the handler for agent-to-client requests (permission prompts).
func (c *Client) OnRequest(fn Handler) {
	c.mu.Lock()
	c.handler = fn
	c.mu.Unlock()
}

func (c *Client) closeDone(err error) {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		if c.exitErr == nil && err != nil {
			c.exitErr = err
		}
		c.mu.Unlock()
		close(c.done)
	})
}

func (c *Client) readLoop() {
	defer c.closeDone(nil)
	for {
		line, err := c.out.ReadBytes('\n')
		if len(line) > 0 {
			c.dispatch(line)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				c.closeDone(err)
			}
			return
		}
	}
}

func (c *Client) dispatch(line []byte) {
	var m Message
	if err := json.Unmarshal(line, &m); err != nil {
		// Not JSON (a stray log line): the caller keeps stderr for diagnostics,
		// so dropping this keeps the session alive.
		return
	}
	switch {
	case m.Method != "" && m.ID != nil:
		go c.answer(&m)
	case m.Method != "":
		if m.Method == "session/update" {
			c.updateMu.Lock()
			fn := c.onUpdate
			c.updateMu.Unlock()
			if fn != nil {
				fn(m.Params)
			}
		}
	case m.ID != nil:
		var id int64
		if err := json.Unmarshal(*m.ID, &id); err != nil {
			return
		}
		c.mu.Lock()
		ch := c.pending[id]
		delete(c.pending, id)
		c.mu.Unlock()
		if ch != nil {
			ch <- &m
		}
	}
}

func (c *Client) answer(m *Message) {
	c.mu.Lock()
	h := c.handler
	c.mu.Unlock()
	var result any
	var err error
	if h != nil {
		result, err = h(m.Method, m.Params)
	}
	resp := map[string]any{"jsonrpc": "2.0", "id": m.ID}
	if err != nil {
		resp["error"] = map[string]any{"code": -32601, "message": err.Error()}
	} else if result == nil {
		resp["result"] = map[string]any{}
	} else {
		resp["result"] = result
	}
	_ = c.write(resp)
}

func (c *Client) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.stdin == nil {
		return errors.New("acp: connection closed")
	}
	_, err = c.stdin.Write(b)
	return err
}

// Notify sends a notification (no id, no response).
func (c *Client) Notify(method string, params any) error {
	m := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		m["params"] = params
	}
	return c.write(m)
}

// Call sends a request and waits for its response. A response with an error
// object is returned as an error. The call also fails when the connection dies
// or ctx expires.
func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan *Message, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	m := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		m["params"] = params
	}
	if err := c.write(m); err != nil {
		return nil, err
	}
	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		select {
		case resp := <-ch:
			if resp.Error != nil {
				return nil, resp.Error
			}
			return resp.Result, nil
		default:
		}
		c.mu.Lock()
		err := c.exitErr
		c.mu.Unlock()
		if err == nil {
			err = errors.New("acp: agent process exited")
		}
		return nil, err
	}
}

// Initialize performs the ACP handshake.
func (c *Client) Initialize(ctx context.Context, clientName string) (json.RawMessage, error) {
	return c.Call(ctx, "initialize", map[string]any{
		"protocolVersion": ProtocolVersion,
		"clientCapabilities": map[string]any{
			"fs": map[string]any{"readTextFile": false, "writeTextFile": false},
		},
		"clientInfo": map[string]any{"name": clientName, "version": "1"},
	})
}

// NewSessionResult is the useful half of a session/new response.
type NewSessionResult struct {
	SessionID     string `json:"sessionId"`
	ConfigOptions []struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		CurrentValue string `json:"currentValue"`
	} `json:"configOptions"`
}

// NewSession opens a session with the given working directory.
func (c *Client) NewSession(ctx context.Context, cwd string) (*NewSessionResult, error) {
	raw, err := c.Call(ctx, "session/new", map[string]any{
		"cwd":        cwd,
		"mcpServers": []any{},
	})
	if err != nil {
		return nil, err
	}
	var res NewSessionResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("session/new: %w", err)
	}
	if res.SessionID == "" {
		return nil, errors.New("session/new returned no sessionId")
	}
	return &res, nil
}

// PromptResult is the response of one session/prompt.
type PromptResult struct {
	StopReason string `json:"stopReason"`
}

// Prompt runs one turn.
func (c *Client) Prompt(ctx context.Context, sessionID, text string) (*PromptResult, error) {
	raw, err := c.Call(ctx, "session/prompt", map[string]any{
		"sessionId": sessionID,
		"prompt":    []any{map[string]any{"type": "text", "text": text}},
	})
	if err != nil {
		return nil, err
	}
	var res PromptResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("session/prompt: %w", err)
	}
	return &res, nil
}

// Cancel asks the agent to stop the running turn (an ACP notification).
func (c *Client) Cancel(sessionID string) error {
	return c.Notify("session/cancel", map[string]any{"sessionId": sessionID})
}

// CloseSession asks the agent to close the session.
func (c *Client) CloseSession(ctx context.Context, sessionID string) error {
	_, err := c.Call(ctx, "session/close", map[string]any{"sessionId": sessionID})
	return err
}

// Shutdown closes stdin, waits grace for the process to exit, then kills the
// process group.
func (c *Client) Shutdown(grace time.Duration) {
	c.writeMu.Lock()
	if c.stdin != nil {
		_ = c.stdin.Close()
		c.stdin = nil
	}
	c.writeMu.Unlock()
	select {
	case <-c.reaped:
		return
	case <-time.After(grace):
	}
	c.Kill()
	select {
	case <-c.reaped:
	case <-time.After(5 * time.Second):
	}
}

// Kill kills the process group (the agent may have children).
func (c *Client) Kill() {
	if c.cmd.Process == nil {
		return
	}
	killGroup(c.cmd.Process.Pid)
}
