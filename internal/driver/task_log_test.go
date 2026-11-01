package driver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"github.com/hashicorp/nomad/plugins/drivers"
	"io"
	"nomad-driver-lima/internal/lima"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func realLogFIFO(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nomad.fifo")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func TestTaskLogFIFOAbsentReaderBoundedAndUnchanged(t *testing.T) {
	path := realLogFIFO(t)
	before, _ := os.Lstat(path)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	start := time.Now()
	w := newTaskLog(ctx, path)
	defer w.Close()
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("constructor blocked on missing reader")
	}
	start = time.Now()
	n, err := w.Write([]byte("durable guest output"))
	if n != 0 || err == nil || time.Since(start) > time.Second {
		t.Fatalf("missing reader not bounded: n=%d err=%v", n, err)
	}
	after, _ := os.Lstat(path)
	if !os.SameFile(before, after) || after.Mode()&os.ModeNamedPipe == 0 {
		t.Fatal("FIFO replaced")
	}
}
func TestTaskLogFIFOReaderArrivalAndCancellation(t *testing.T) {
	path := realLogFIFO(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	w := newTaskLog(ctx, path)
	defer w.Close()
	payload := []byte("native stdout after reader arrives\n")
	done := make(chan error, 1)
	go func() { _, err := w.Write(payload); done <- err }()
	time.Sleep(20 * time.Millisecond)
	r, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	actual := make([]byte, len(payload))
	_, err = io.ReadFull(r, actual)
	if err != nil || !bytes.Equal(actual, payload) {
		t.Fatal("late reader output", err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	r.Close()
	go func() { _, err := w.Write([]byte("no reader now")); done <- err }()
	time.Sleep(20 * time.Millisecond)
	start := time.Now()
	cancel()
	select {
	case err = <-done:
		if err == nil {
			t.Fatal("cancel ignored")
		}
	case <-time.After(time.Second):
		t.Fatal("cancel blocked on FIFO")
	}
	if time.Since(start) > time.Second {
		t.Fatal("slow cancellation")
	}
}
func TestTaskLogFIFOBackpressureReportsExactPrefix(t *testing.T) {
	path := realLogFIFO(t)
	readers := make(chan *os.File, 1)
	go func() { r, _ := os.OpenFile(path, os.O_RDONLY, 0); readers <- r }()
	w := newTaskLog(t.Context(), path)
	defer w.Close()
	payload := bytes.Repeat([]byte("0123456789abcdef"), 65536)
	start := time.Now()
	n, err := w.Write(payload)
	if err == nil || n <= 0 || n >= len(payload) || time.Since(start) > time.Second {
		t.Fatalf("backpressure not bounded: %d %v", n, err)
	}
	r := <-readers
	if r == nil {
		t.Fatal("real reader failed")
	}
	defer r.Close()
	first := make([]byte, n)
	if _, err = io.ReadFull(r, first); err != nil {
		t.Fatal(err)
	}
	rest := make([]byte, len(payload)-n)
	read := make(chan error, 1)
	go func() { _, err := io.ReadFull(r, rest); read <- err }()
	// Drain concurrently: resume from the exact accepted byte offset, not zero.
	offset := n
	for offset < len(payload) {
		written, e := w.Write(payload[offset:])
		offset += written
		if e != nil && written == 0 {
			t.Fatal(e)
		}
	}
	if err = <-read; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(append(first, rest...), payload) {
		t.Fatal("FIFO bytes duplicated or lost")
	}
}
func TestColdRecoverRealFIFOAbsentReaderThenArrivalNoReplay(t *testing.T) {
	f := newFake()
	root := t.TempDir()
	d := setup(t, f, root)
	cfg := fixture(t)
	h, _, err := d.StartTask(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Cold persisted Config deliberately lacks the SDK private raw configuration.
	raw, _ := json.Marshal(h.Config)
	h.Config = &drivers.TaskConfig{}
	json.Unmarshal(raw, h.Config)
	h.Config.StdoutPath = realLogFIFO(t)
	h.Config.StderrPath = realLogFIFO(t)
	state, err := d.store.read(lima.Hash([]byte(cfg.ID)))
	if err != nil {
		t.Fatal(err)
	}
	state.OutOffset = 0
	state.ErrOffset = 0
	if err = d.store.write(state); err != nil {
		t.Fatal(err)
	}
	cold := setup(t, f, root)
	start := time.Now()
	if err = cold.RecoverTask(h); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Fatal("RecoverTask waited for log reader")
	}
	output := make(chan []byte, 1)
	go func() {
		r, e := os.OpenFile(h.Config.StdoutPath, os.O_RDONLY, 0)
		if e != nil {
			output <- nil
			return
		}
		defer r.Close()
		b := make([]byte, len("guest-output\n"))
		io.ReadFull(r, b)
		output <- b
	}()
	select {
	case actual := <-output:
		if string(actual) != "guest-output\n" {
			t.Fatalf("output=%q", actual)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("late reader never received durable output")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err = cold.Shutdown(ctx); err != nil {
		t.Fatal("FIFO blocked shutdown", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.creates != 1 || f.starts != 1 || f.launches != 1 || f.stops != 0 {
		t.Fatal("FIFO recovery replayed or stopped VM")
	}
}

type spoolLogRunner struct {
	base *fakeCLI
	data []byte
}

func (r *spoolLogRunner) Run(ctx context.Context, in []byte, args ...string) ([]byte, error) {
	raw, err := r.base.Run(ctx, in, args...)
	var request map[string]any
	if err != nil || json.Unmarshal(in, &request) != nil || request["action"] != "status" {
		return raw, err
	}
	var response map[string]any
	if err = json.Unmarshal(raw, &response); err != nil {
		return nil, err
	}
	offset := int(request["stdout_offset"].(float64))
	end := offset + 65536
	if end > len(r.data) {
		end = len(r.data)
	}
	response["stdout"] = base64.StdEncoding.EncodeToString(r.data[offset:end])
	response["stdout_offset"] = end
	response["stdout_eof"] = end == len(r.data)
	return json.Marshal(response)
}
func TestColdFIFOBackpressureCommitsDeliveredPrefixWithoutDuplicateLogs(t *testing.T) {
	f := newFake()
	root := t.TempDir()
	d := setup(t, f, root)
	cfg := fixture(t)
	h, _, err := d.StartTask(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	h.Config.StdoutPath = realLogFIFO(t)
	h.Config.StderrPath = realLogFIFO(t)
	state, err := d.store.read(lima.Hash([]byte(cfg.ID)))
	if err != nil {
		t.Fatal(err)
	}
	state.OutOffset = 0
	state.ErrOffset = 0
	d.store.write(state)
	payload := bytes.Repeat([]byte("0123456789abcdef"), 65536)
	cold := setup(t, f, root)
	cold.client.Runner = &spoolLogRunner{base: f, data: payload}
	if err = cold.RecoverTask(h); err != nil {
		t.Fatal(err)
	}
	r, err := os.OpenFile(h.Config.StdoutPath, os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	task, err := cold.get(cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	var offset int64
	for time.Now().Before(deadline) {
		task.mu.Lock()
		offset = task.state.OutOffset
		task.mu.Unlock()
		if offset > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if offset <= 0 || offset >= int64(len(payload)) {
		t.Fatalf("no durable partial progress: %d", offset)
	}
	actual := make([]byte, len(payload))
	read := make(chan error, 1)
	go func() { _, e := io.ReadFull(r, actual); read <- e }()
	select {
	case err = <-read:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("backpressure never recovered")
	}
	if !bytes.Equal(actual, payload) {
		t.Fatal("monitor replayed or dropped a delivered stream prefix")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err = cold.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.creates != 1 || f.starts != 1 || f.launches != 1 || f.stops != 0 {
		t.Fatal("logging replayed/stopped VM")
	}
}
