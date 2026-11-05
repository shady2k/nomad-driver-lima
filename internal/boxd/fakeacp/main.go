// Command fakeacp is a stand-in agent for boxd tests: it speaks just enough of
// the Agent Client Protocol to exercise every contract command without a model
// provider.
//
// It is started the way boxd starts prime-agent:
//
//	fakeacp --mode acp --session-dir DIR --provider P --model M [-r ROOT]
//
// Behaviour is chosen by the prompt text:
//
//	"pong"          stream "pong" in three chunks, end_turn
//	"tool"          a tool call with a result, then text
//	"slow:<ms>"     stream some text, wait <ms>, finish (default 3000)
//	"hang"          never finish until cancelled
//	"crash"         emit a tool call, then exit 3 mid-turn
//	"fail"          answer the prompt with a JSON-RPC error
//	"leave-bg"      start "sh -c 'sleep 300 & wait'" in the background, answer,
//	                and leave it running: the tool that outlives its turn
//	"say:<text>"    answer with <text>
//	anything else   echo the prompt back
//
// Every start appends its argv to <session-dir>/fake-argv.log, so a test can
// prove that a resumed session was really started with "-r <root id>".
package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcErr         `json:"error,omitempty"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type agent struct {
	mu        sync.Mutex
	out       *bufio.Writer
	sessionID string
	rootID    string
	dir       string
	resumed   string
	cancelled bool
	deaf      bool
	booted    bool
}

func main() {
	var (
		mode       string
		sessionDir string
		provider   string
		model      string
		resume     string
		showVer    bool
	)
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() string {
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch a {
		case "--mode":
			mode = next()
		case "--session-dir":
			sessionDir = next()
		case "--provider":
			provider = next()
		case "--model":
			model = next()
		case "-r", "--resume":
			resume = next()
		case "--version":
			showVer = true
		}
	}
	if showVer {
		fmt.Println("fake-acp 1.0")
		return
	}
	if mode != "acp" {
		fmt.Fprintln(os.Stderr, "fakeacp: only --mode acp is supported")
		os.Exit(2)
	}
	if sessionDir == "" {
		fmt.Fprintln(os.Stderr, "fakeacp: --session-dir is required")
		os.Exit(2)
	}
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "fakeacp:", err)
		os.Exit(2)
	}
	if resume != "" {
		// A resumed session appends to the existing transcript, exactly like the
		// real agent does.
	} else {
		resume = newID()
	}
	a := &agent{
		out:     bufio.NewWriter(os.Stdout),
		rootID:  resume,
		dir:     sessionDir,
		resumed: resume,
	}
	a.logArgv(os.Args[1:], provider, model)
	a.writeTranscript(fmt.Sprintf(`{"type":"session_start","root":%q,"provider":%q,"model":%q}`, resume, provider, model))

	in := bufio.NewReader(os.Stdin)
	for {
		line, err := in.ReadBytes('\n')
		if len(line) > 0 {
			var m rpcMsg
			if json.Unmarshal(line, &m) == nil && m.Method != "" {
				a.handle(m)
			}
		}
		if err != nil {
			return
		}
	}
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:4]) + "-" + hex.EncodeToString(b[4:])
}

func (a *agent) logArgv(argv []string, provider, model string) {
	f, err := os.OpenFile(filepath.Join(a.dir, "fake-argv.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	rec := map[string]any{"argv": argv, "provider": provider, "model": model, "pid": os.Getpid(), "time": time.Now().UTC()}
	b, _ := json.Marshal(rec)
	_, _ = f.Write(append(b, '\n'))
}

func (a *agent) writeTranscript(line string) {
	f, err := os.OpenFile(filepath.Join(a.dir, a.rootID+".jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append([]byte(line), '\n'))
}

func (a *agent) send(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	a.mu.Lock()
	_, _ = a.out.Write(append(b, '\n'))
	_ = a.out.Flush()
	a.mu.Unlock()
}

func (a *agent) reply(id json.RawMessage, result any) {
	a.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (a *agent) replyErr(id json.RawMessage, code int, msg string) {
	a.send(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}})
}

func (a *agent) update(u map[string]any) {
	if a.sessionID == "" {
		return
	}
	a.send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
		"sessionId": a.sessionID,
		"update":    u,
	}})
}

func (a *agent) handle(m rpcMsg) {
	switch m.Method {
	case "initialize":
		a.reply(m.ID, map[string]any{
			"protocolVersion": 1,
			"agentCapabilities": map[string]any{
				"loadSession":        false,
				"promptCapabilities": map[string]any{"image": false},
			},
			"agentInfo": map[string]any{"name": "fakeacp", "version": "1.0"},
		})
	case "session/new":
		if a.booted {
			a.replyErr(m.ID, -32603, "fakeacp hosts one session per connection")
			return
		}
		a.booted = true
		a.sessionID = newID()
		a.writeTranscript(fmt.Sprintf(`{"type":"session_new","acp":%q,"root":%q}`, a.sessionID, a.rootID))
		a.reply(m.ID, map[string]any{
			"sessionId": a.sessionID,
			"configOptions": []any{map[string]any{
				"id": "model", "name": "Model", "category": "model", "type": "select",
				"currentValue": `["fake","model-1"]`,
			}},
		})
	case "session/set_config_option":
		a.reply(m.ID, map[string]any{})
	case "session/prompt":
		go a.turn(m)
	case "session/cancel":
		a.mu.Lock()
		if !a.deaf {
			a.cancelled = true
		}
		a.mu.Unlock()
	case "session/close":
		a.reply(m.ID, map[string]any{})
	default:
		if m.ID != nil {
			a.replyErr(m.ID, -32601, fmt.Sprintf("Method not found: %s", m.Method))
		}
	}
}

func (a *agent) isCancelled() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cancelled
}

func (a *agent) turn(m rpcMsg) {
	var p struct {
		SessionID string `json:"sessionId"`
		Prompt    []struct {
			Text string `json:"text"`
		} `json:"prompt"`
	}
	_ = json.Unmarshal(m.Params, &p)
	var text string
	for _, part := range p.Prompt {
		text += part.Text
	}
	if p.SessionID != a.sessionID {
		a.replyErr(m.ID, -32603, "Unknown ACP session: "+p.SessionID)
		return
	}
	a.writeTranscript(fmt.Sprintf(`{"type":"prompt","text":%q}`, text))
	a.mu.Lock()
	// "deaf" makes the agent ignore session/cancel, the way prime-agent 0.9.8
	// does (it answers -32601 to the method and ignores the notification).
	a.deaf = strings.Contains(text, "deaf")
	a.cancelled = false
	a.mu.Unlock()
	a.update(map[string]any{"sessionUpdate": "user_message_chunk", "content": map[string]any{"type": "text", "text": text}})

	switch {
	case strings.Contains(text, "fail"):
		a.replyErr(m.ID, -32000, "fake failure for testing")
		return
	case strings.Contains(text, "crash"):
		a.update(map[string]any{
			"sessionUpdate": "tool_call", "toolCallId": "tc-crash",
			"title": "Bash: sleep 1000", "kind": "execute", "status": "in_progress",
		})
		time.Sleep(100 * time.Millisecond)
		os.Exit(3)
	case strings.Contains(text, "hang"):
		a.stream("thinking about it")
		for i := 0; i < 600; i++ {
			if a.isCancelled() {
				a.reply(m.ID, map[string]any{"stopReason": "cancelled"})
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		a.reply(m.ID, map[string]any{"stopReason": "end_turn"})
		return
	case strings.Contains(text, "slow:"):
		ms := 3000
		if _, err := fmt.Sscanf(text[strings.Index(text, "slow:")+5:], "%d", &ms); err != nil {
			// "slow:" without a number keeps the default delay.
			ms = 3000
		}
		a.stream("working")
		deadline := time.Now().Add(time.Duration(ms) * time.Millisecond)
		for time.Now().Before(deadline) {
			if a.isCancelled() {
				a.reply(m.ID, map[string]any{"stopReason": "cancelled"})
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		a.stream("done")
		a.reply(m.ID, map[string]any{"stopReason": "end_turn"})
		return
	case strings.Contains(text, "leave-bg"):
		// A tool call that starts something which outlives the answer: the
		// command runs here as the executor's child, as a tool's command does,
		// and nothing waits for it after the turn.
		if err := a.leaveRunning(); err != nil {
			a.replyErr(m.ID, -32603, "fakeacp: "+err.Error())
			return
		}
		a.stream("started it in the background")
		a.reply(m.ID, map[string]any{"stopReason": "end_turn"})
		return
	case strings.Contains(text, "tool"):
		a.stream("let me look")
		a.update(map[string]any{
			"sessionUpdate": "tool_call", "toolCallId": "tc-1",
			"title": "Read: /etc/hostname", "kind": "read", "status": "in_progress",
		})
		time.Sleep(100 * time.Millisecond)
		a.update(map[string]any{
			"sessionUpdate": "tool_call_update", "toolCallId": "tc-1", "status": "completed",
			"content": []any{map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": "box-guest"}}},
		})
		a.stream("the hostname is box-guest")
		a.reply(m.ID, map[string]any{"stopReason": "end_turn"})
		return
	}

	if i := strings.Index(text, "say:"); i >= 0 {
		a.stream(strings.TrimSpace(text[i+4:]))
		a.reply(m.ID, map[string]any{"stopReason": "end_turn"})
		return
	}
	if strings.Contains(text, "pong") {
		a.stream("po")
		a.stream("ng")
		a.reply(m.ID, map[string]any{"stopReason": "end_turn"})
		return
	}
	a.stream("echo: " + text)
	a.reply(m.ID, map[string]any{"stopReason": "end_turn"})
}

// leaveRunning starts the background command the test looks for: a shell that
// keeps a sleep alive for five minutes under it.
func (a *agent) leaveRunning() error {
	cmd := exec.Command("/bin/sh", "-c", "sleep 300 & wait")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	fmt.Fprintf(os.Stderr, "fakeacp: left a background command running, pid %d\n", cmd.Process.Pid)
	return nil
}

// stream emits one assistant message chunk.
func (a *agent) stream(text string) {
	a.update(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": text}})
	time.Sleep(20 * time.Millisecond)
}
