package boxd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shady2k/nomad-driver-lima/internal/version"
)

// Daemon is the boxd HTTP server on the unix socket.
type Daemon struct {
	cfg  DaemonConfig
	mgr  *Manager
	ln   net.Listener
	srv  *http.Server
	once sync.Once
}

// NewDaemon loads the persisted sessions and builds the HTTP handler.
func NewDaemon(cfg DaemonConfig) (*Daemon, error) {
	cfg = cfg.withDefaults()
	mgr, err := NewManager(cfg)
	if err != nil {
		return nil, err
	}
	d := &Daemon{cfg: cfg, mgr: mgr}
	d.srv = &http.Server{
		Handler:           d.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No write timeout: "wait" and "events" are long polls.
		IdleTimeout: 2 * time.Minute,
	}
	return d, nil
}

// Manager is the daemon's session manager.
func (d *Daemon) Manager() *Manager { return d.mgr }

// Serve listens on the unix socket and serves until ctx is cancelled.
func (d *Daemon) Serve(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(d.cfg.Socket), 0o755); err != nil {
		return err
	}
	if conn, err := net.DialTimeout("unix", d.cfg.Socket, 200*time.Millisecond); err == nil {
		conn.Close()
		return fmt.Errorf("another boxd is already listening on %s", d.cfg.Socket)
	}
	_ = os.Remove(d.cfg.Socket)
	ln, err := net.Listen("unix", d.cfg.Socket)
	if err != nil {
		return err
	}
	// The socket is world-accessible on purpose: `nomad alloc exec` runs as the
	// box user (the lima user), not as root, and it is the only way the box CLI
	// reaches boxd. The box is a single-tenant VM for one coordinator, so there
	// is no other local user to keep out.
	if err := os.Chmod(d.cfg.Socket, 0o666); err != nil {
		d.cfg.logf("chmod socket: %v", err)
	}
	d.ln = ln
	d.cfg.logf("listening on %s (state %s)", d.cfg.Socket, d.cfg.StateDir)

	errCh := make(chan error, 1)
	go func() { errCh <- d.srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		d.shutdown()
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (d *Daemon) shutdown() {
	d.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = d.srv.Shutdown(ctx)
		d.mgr.Shutdown()
		if d.ln != nil {
			_ = os.Remove(d.ln.Addr().String())
		}
		d.cfg.logf("stopped")
	})
}

// SocketPath is the listening socket.
func (d *Daemon) SocketPath() string { return d.cfg.Socket }

// Handler builds the HTTP routes.
func (d *Daemon) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "version": version.Version})
	})
	mux.HandleFunc("GET /v1/info", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, d.Info(r.Context()))
	})
	mux.HandleFunc("POST /v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Executor string `json:"executor"`
			Model    string `json:"model"`
			Cwd      string `json:"cwd"`
			Title    string `json:"title"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			fail(w, Errorf(CodeInvalid, "bad request body: %v", err))
			return
		}
		s, err := d.mgr.NewSession(req.Executor, req.Model, req.Cwd, req.Title)
		if err != nil {
			fail(w, err)
			return
		}
		d.cfg.logf("new session %s (%s %s)", s.ID, s.Executor, s.Model)
		writeJSON(w, 200, map[string]any{"session": s.ID})
	})
	mux.HandleFunc("GET /v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"sessions": d.mgr.List()})
	})
	mux.HandleFunc("GET /v1/session/{id}", func(w http.ResponseWriter, r *http.Request) {
		s, err := d.mgr.Get(r.PathValue("id"))
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, s)
	})
	mux.HandleFunc("POST /v1/session/{id}/send", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err != nil {
			fail(w, err)
			return
		}
		t, err := d.mgr.Send(r.PathValue("id"), string(body))
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"session": t.Session, "turn": t.Turn})
	})
	mux.HandleFunc("GET /v1/session/{id}/wait", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		turn, _ := strconv.Atoi(q.Get("turn"))
		timeout := 300 * time.Second
		if v := q.Get("timeout"); v != "" {
			secs, err := strconv.ParseFloat(v, 64)
			if err != nil || secs < 0 {
				fail(w, Errorf(CodeInvalid, "bad timeout %q", v))
				return
			}
			timeout = time.Duration(secs * float64(time.Second))
		}
		ctx := r.Context()
		var cancel context.CancelFunc
		if timeout > 0 {
			// The handler answers a little after the wait deadline, so the client
			// still receives the running turn.
			ctx, cancel = context.WithTimeout(ctx, timeout+5*time.Second)
			defer cancel()
		}
		type result struct {
			t   Turn
			err error
		}
		ch := make(chan result, 1)
		go func() {
			t, err := d.mgr.Wait(r.PathValue("id"), turn, timeout)
			ch <- result{t, err}
		}()
		select {
		case res := <-ch:
			if res.err != nil {
				fail(w, res.err)
				return
			}
			writeJSON(w, 200, res.t)
		case <-ctx.Done():
			// Client went away; nothing to answer.
		}
	})
	mux.HandleFunc("GET /v1/session/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var since int64
		if v := q.Get("since"); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 {
				fail(w, Errorf(CodeInvalid, "bad since %q", v))
				return
			}
			since = n
		}
		limit := 100
		if v := q.Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				fail(w, Errorf(CodeInvalid, "bad limit %q", v))
				return
			}
			limit = n
		}
		evs, next, err := d.mgr.Events(r.PathValue("id"), since, limit)
		if err != nil {
			fail(w, err)
			return
		}
		if evs == nil {
			evs = []Event{}
		}
		writeJSON(w, 200, map[string]any{"events": evs, "next": next})
	})
	mux.HandleFunc("POST /v1/session/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		t, err := d.mgr.Cancel(r.PathValue("id"))
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, t)
	})
	// POST /v1/reload re-reads the executor credentials the driver put on the
	// guest's tmpfs. It is how a running box is rolled to new credentials without
	// being recreated.
	mux.HandleFunc("POST /v1/reload", func(w http.ResponseWriter, r *http.Request) {
		res, err := d.mgr.Reload()
		if err != nil {
			fail(w, err)
			return
		}
		d.cfg.logf("reloaded the executor credentials (configured %v, restarted %v, deferred %v)",
			res.Configured, res.Restarted, res.Deferred)
		writeJSON(w, 200, res)
	})
	mux.HandleFunc("POST /v1/session/{id}/close", func(w http.ResponseWriter, r *http.Request) {
		if err := d.mgr.Close(r.PathValue("id")); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"session": r.PathValue("id"), "state": StateClosed})
	})
	return mux
}

// Info builds the contract info document.
func (d *Daemon) Info(ctx context.Context) Info {
	info := Info{
		BoxdVersion: version.Version,
		Executors:   []ExecutorInfo{},
		Sessions:    d.mgr.Count(),
		CPUs:        runtime.NumCPU(),
	}
	total, free := memoryMiB()
	info.MemTotalMiB, info.MemFreeMiB = total, free
	for _, name := range []string{"prime", "opencode", "claude"} {
		ex, err := buildExecutor(name, d.cfg)
		if err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		info.Executors = append(info.Executors, ex.Probe(ctx))
		cancel()
	}
	return info
}

// memoryMiB reads total and available memory from /proc/meminfo.
func memoryMiB() (total, free int) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		v, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			total = v / 1024
		case "MemAvailable:":
			free = v / 1024
		}
	}
	return total, free
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	_, _ = w.Write(append(b, '\n'))
}

func fail(w http.ResponseWriter, err error) {
	code := CodeOf(err)
	msg := err.Error()
	if e, ok := err.(*Error); ok {
		msg = e.Message
	}
	status := http.StatusInternalServerError
	switch code {
	case CodeNotFound:
		status = http.StatusNotFound
	case CodeInvalid:
		status = http.StatusBadRequest
	case CodeBusy, CodeClosed:
		status = http.StatusConflict
	case CodeExecutor:
		status = http.StatusBadGateway
	case CodeTimeout:
		status = http.StatusGatewayTimeout
	}
	writeJSON(w, status, ErrorResponse{Error: ErrorBody{Code: code, Message: msg}})
}
