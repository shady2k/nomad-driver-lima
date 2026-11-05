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
	"os/exec"
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
	cfg   DaemonConfig
	mgr   *Manager
	ln    net.Listener
	srv   *http.Server
	once  sync.Once
	usage usageSampler
}

// NewDaemon loads the persisted sessions and builds the HTTP handler.
func NewDaemon(cfg DaemonConfig) (*Daemon, error) {
	cfg = cfg.withDefaults()
	mgr, err := NewManager(cfg)
	if err != nil {
		return nil, err
	}
	d := &Daemon{cfg: cfg, mgr: mgr}
	// The load boxd measures reads the same process table the turn's report
	// does, so a test's tree is the one both read.
	d.usage.procRoot = cfg.procRoot()
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
	d.cfg.logf("%s", d.startLine(ctx))
	var usageLog func(string, ...any)
	if d.cfg.Diagnostics {
		usageLog = d.cfg.logf
	}
	go d.usage.run(ctx, usageLog)
	if d.cfg.Diagnostics {
		go watchKernel(ctx, d.cfg.logf)
		go watchEarlyOOM(ctx, d.cfg.logf)
		go watchResources(ctx, d.cfg.logf, []string{"/"})
		go tailDir(ctx, d.cfg.logf, "prime log", filepath.Join(d.cfg.executorsDir(), "prime", "logs"))
	}

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
	mux.HandleFunc("GET /v1/prepare", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, d.mgr.Prepare().State())
	})
	mux.HandleFunc("GET /v1/prepare/wait", func(w http.ResponseWriter, r *http.Request) {
		timeout := 30 * time.Second
		if v := r.URL.Query().Get("timeout"); v != "" {
			sec, err := strconv.Atoi(v)
			if err != nil || sec < 0 {
				fail(w, Errorf(CodeInvalid, "timeout %q is not a number of seconds", v))
				return
			}
			timeout = time.Duration(sec) * time.Second
		}
		writeJSON(w, 200, d.mgr.Prepare().Wait(r.Context(), timeout))
	})
	mux.HandleFunc("POST /v1/prepare", func(w http.ResponseWriter, r *http.Request) {
		var req PrepareRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			fail(w, Errorf(CodeInvalid, "bad request body: %v", err))
			return
		}
		st, err := d.mgr.Prepare().Start(req)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, st)
	})
	// The files come as the request body, a tar stream; the rest of the
	// request is in the query.
	mux.HandleFunc("POST /v1/prepare/files", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		limit, _ := strconv.Atoi(q.Get("limit_sec"))
		req := PrepareRequest{Dest: q.Get("dest"), Owner: q.Get("owner"), LimitSec: limit}
		st, err := d.mgr.Prepare().StartFiles(req, r.Body)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, st)
	})
	mux.HandleFunc("POST /v1/prepare/cancel", func(w http.ResponseWriter, r *http.Request) {
		st, err := d.mgr.Prepare().Cancel()
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, st)
	})
	mux.HandleFunc("POST /v1/prepare/skip", func(w http.ResponseWriter, r *http.Request) {
		st, err := d.mgr.Prepare().Skip()
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, st)
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
		// "total" is how many events there are from the cursor and "more" says
		// that this answer is a page of them: a cut feed reads as a hang to
		// whoever asked, so the answer has to say it was cut.
		evs, next, total, err := d.mgr.EventsPage(r.PathValue("id"), since, limit)
		if err != nil {
			fail(w, err)
			return
		}
		if evs == nil {
			evs = []Event{}
		}
		writeJSON(w, 200, map[string]any{"events": evs, "next": next, "total": total, "more": total > len(evs)})
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
	// POST /v1/seal seals the box into an image. The answer is a stream of JSON
	// lines -- one {"line": "..."} per step while it works, then {"seal": {...}}
	// or {"error": {...}} -- because a seal takes minutes and its progress is
	// what a person watches in the task log. The status is 200 either way: the
	// lines are on the wire before the seal can fail.
	mux.HandleFunc("POST /v1/seal", func(w http.ResponseWriter, r *http.Request) {
		var req SealRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			fail(w, Errorf(CodeInvalid, "bad request body: %v", err))
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		enc := json.NewEncoder(w)
		flusher, _ := w.(http.Flusher)
		write := func(v any) {
			if err := enc.Encode(v); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		d.cfg.logf("seal requested: image %s (%s)", req.Name, dashOr(req.Version, "no version"))
		res, err := d.mgr.Seal(req, func(line string) {
			write(map[string]string{"line": line})
		})
		if err != nil {
			e := asError(err)
			d.cfg.logf("seal failed: %s", e.Message)
			write(ErrorResponse{Error: ErrorBody{Code: e.Code, Message: e.Message}})
			return
		}
		write(map[string]any{"seal": res})
	})
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
		Platform:    readPlatform(d.cfg.platformDir()),
		Sessions:    d.mgr.Count(),
		CPUs:        runtime.NumCPU(),
	}
	total, free := memoryMiB()
	info.MemTotalMiB, info.MemFreeMiB = total, free
	st := d.mgr.Prepare().State()
	info.Prepare = &st
	if u := d.usage.Last(); !u.At.IsZero() {
		// What is running with no turn in progress is asked for here and not
		// measured by the sampler: it is a report about this moment, and a turn
		// that started since the last sample would make it wrong.
		u.Leftover = d.mgr.leftoverAt(d.cfg.now())
		info.Usage = &u
	}
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

// startLine says what this boxd runs with: its version, the executors and
// whether they have credentials, whether the task files are mounted, and which
// platform bundle the box mounts.
func (d *Daemon) startLine(ctx context.Context) string {
	var parts []string
	for _, name := range []string{"prime", "opencode", "claude"} {
		ex, err := buildExecutor(name, d.cfg)
		if err != nil {
			continue
		}
		state := "no credentials"
		if c, ok := ex.(interface{ Configured() bool }); ok && c.Configured() {
			state = "credentials"
		}
		parts = append(parts, name+" ("+state+")")
	}
	mounted := "not mounted"
	if out, err := exec.CommandContext(ctx, "findmnt", "-no", "FSTYPE,OPTIONS", "--target", d.cfg.SecretsDir).Output(); err == nil {
		if f := strings.Fields(string(out)); len(f) > 0 && f[0] != "tmpfs" && f[0] != "ext4" {
			mounted = "mounted (" + f[0] + ")"
		}
	}
	platform := readPlatform(d.cfg.platformDir())
	bundle := "no bundle mounted at " + platform.Path
	if platform.Mounted {
		bundle = "platform " + platform.Version
		if platform.Boxd != "" {
			bundle += " (boxd " + platform.Boxd + ")"
		}
	}
	return fmt.Sprintf("boxd %s: executors %s; task files at %s %s; %s",
		version.Version, strings.Join(parts, ", "), d.cfg.SecretsDir, mounted, bundle)
}
