package driver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hashicorp/nomad/plugins/drivers"

	"github.com/shady2k/nomad-driver-lima/internal/box"
	"github.com/shady2k/nomad-driver-lima/internal/limactl"
)

// What qemu itself says about a VM.
//
// A host filesystem that fills up does not stop a VM: qemu keeps the guest's
// writes in its own buffer, and when the first one cannot be written to the disk
// it stops the VM and puts it into the io-error run state. Nomad and Lima both go
// on reporting the box as running, and the guest stops answering, so every agent
// only sees "the guest does not answer". qemu says exactly what happened, on the
// QMP socket Lima gives it, and the driver asks it there.
//
// A box disk is a qcow2 file on the node's own filesystem with no backing file
// (measured), so the room of the Lima home is the room the guest writes into.
const (
	// qmpSocketName is the QMP socket in an instance directory: Lima 2.2.0 starts
	// qemu with a character device of its own at <instance dir>/qmp.sock.
	qmpSocketName = "qmp.sock"

	// qmpProbeTimeout bounds one conversation with qemu. QEMU serves one QMP
	// client at a time and Lima's hostagent may hold the socket, so a probe that
	// cannot get in says "unknown" and nothing else: it must never wait, and it
	// must never fail a task.
	qmpProbeTimeout = 2 * time.Second

	// qemuStateRunning is the only run state in which the guest runs.
	qemuStateRunning = "running"
	// qemuStateIOError is qemu's run state for a VM it stopped because it could
	// not write the guest's data to its disk.
	qemuStateIOError = "io-error"
)

// qemuStatusOf asks a qemu VM over its QMP socket what it is doing, and returns
// qemu's own run state: "running" for a healthy VM, and "io-error", "paused" or
// "postmigrate" for one that is not.
//
// ok is false whenever the answer is not known, and an unknown status is never
// read as a paused VM: no socket (a vz box, a VM that is not up), a refused
// connection, a socket that does not answer, and an answer that is not a QMP
// reply all say nothing. The conversation is bounded by timeout on the socket
// itself, so a qemu that accepts and never answers costs its deadline and no
// more.
func qemuStatusOf(instanceDir string, timeout time.Duration) (status string, ok bool) {
	if instanceDir == "" {
		return "", false
	}
	if timeout <= 0 {
		timeout = qmpProbeTimeout
	}
	path := filepath.Join(instanceDir, qmpSocketName)
	if _, err := os.Stat(path); err != nil {
		return "", false
	}
	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.Dial("unix", path)
	if err != nil {
		return "", false
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return "", false
	}
	c := &qmpConn{conn: conn, r: bufio.NewReader(conn)}
	// qemu greets every client before it takes a command; the greeting is not
	// needed, but a socket that sends nothing is not qemu's.
	if _, err := c.readLine(); err != nil {
		return "", false
	}
	if _, err := c.command("qmp_capabilities"); err != nil {
		return "", false
	}
	reply, err := c.command("query-status")
	if err != nil {
		return "", false
	}
	return qemuStatusFromReply(reply)
}

// qmpConn is one QMP conversation: JSON objects, one per line, with the greeting
// first and asynchronous events mixed into the replies.
type qmpConn struct {
	conn net.Conn
	r    *bufio.Reader
}

// qmpReply is one line qemu sends: the answer to a command, or an event.
type qmpReply struct {
	Return json.RawMessage `json:"return"`
	Error  json.RawMessage `json:"error"`
	Event  string          `json:"event"`
}

// readLine reads one line of the conversation.
func (c *qmpConn) readLine() ([]byte, error) {
	line, err := c.r.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	return line, nil
}

// command sends one QMP command and returns its "return" document, skipping the
// events qemu may interleave with it.
func (c *qmpConn) command(execute string) (json.RawMessage, error) {
	req, err := json.Marshal(map[string]string{"execute": execute})
	if err != nil {
		return nil, err
	}
	if _, err := c.conn.Write(append(req, '\n')); err != nil {
		return nil, err
	}
	for {
		line, err := c.readLine()
		if err != nil {
			return nil, err
		}
		var reply qmpReply
		if err := json.Unmarshal(line, &reply); err != nil {
			return nil, fmt.Errorf("qmp: %w", err)
		}
		if reply.Event != "" {
			continue
		}
		if len(reply.Error) > 0 {
			return nil, fmt.Errorf("qmp: %s", oneJSONLine(reply.Error))
		}
		if len(reply.Return) == 0 {
			return nil, errors.New("qmp: a reply without a return document")
		}
		return reply.Return, nil
	}
}

// qemuStatusFromReply reads the run state out of a query-status reply.
func qemuStatusFromReply(reply []byte) (string, bool) {
	var doc struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(reply, &doc); err != nil {
		return "", false
	}
	if doc.Status == "" {
		return "", false
	}
	return doc.Status, true
}

// oneJSONLine renders a JSON document on one line for a message.
func oneJSONLine(raw json.RawMessage) string {
	out := make([]byte, 0, len(raw))
	for _, b := range raw {
		if b == '\n' || b == '\r' || b == '\t' {
			b = ' '
		}
		out = append(out, b)
	}
	return string(out)
}

// vmStatePeriod is how often the driver asks qemu what each of its VMs is doing.
// It is shorter than the fingerprint period so that the attribute the CLI reads is
// usually there when the fingerprint is built.
const vmStatePeriod = 10 * time.Second

// vmStates is the driver's cache of qemu run states, and the io-error state it
// has already told each task about.
//
// The cache exists so that the fingerprint never waits on a VM: asking qemu is a
// bounded conversation with a process that may be busy, and a node's fingerprint
// must not be held up by it. A goroutine of the driver's own refreshes the cache.
type vmStates struct {
	mu    sync.Mutex
	byBox map[string]string
	// saidIOError is the io-error state each box's task was last told about, so a
	// VM that stays paused is one event and not one per refresh.
	saidIOError map[string]bool
}

// watchVMStates refreshes the qemu statuses of the node's VMs until the driver
// shuts down.
func (d *Driver) watchVMStates() {
	ticker := time.NewTicker(vmStatePeriod)
	defer ticker.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			d.probeVMStates()
		}
	}
}

// probeVMStates asks every running qemu VM its status and records the answers.
//
// The probes run side by side, each bounded by qmpProbeTimeout, so one VM that
// does not answer costs its deadline once and not the others.
func (d *Driver) probeVMStates() {
	if d.boxm == nil || d.config.VMType != box.VMTypeQEMU {
		return
	}
	ctx, cancel := context.WithTimeout(d.ctx, 4*qmpProbeTimeout)
	instances, err := d.boxm.List(ctx)
	cancel()
	if err != nil {
		return
	}
	targets := make([]*limactl.Instance, 0, len(instances))
	for _, inst := range instances {
		if inst != nil && inst.Name != "" && inst.Dir != "" && inst.IsRunning() {
			targets = append(targets, inst)
		}
	}
	states := make(map[string]string, len(targets))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, inst := range targets {
		wg.Add(1)
		go func(inst *limactl.Instance) {
			defer wg.Done()
			status, ok := qemuStatusOf(inst.Dir, qmpProbeTimeout)
			if !ok {
				return
			}
			mu.Lock()
			states[inst.Name] = status
			mu.Unlock()
		}(inst)
	}
	wg.Wait()
	d.applyVMStates(states)
}

// applyVMStates records what qemu said and tells every task whose VM is paused by
// an I/O error, once per transition into it, and once when it runs again.
//
// The event names what qemu reported and nothing it did not: io-error is an I/O
// error, a full host disk is the usual reason for one, and the free space of the
// node is the number a person acts on.
func (d *Driver) applyVMStates(states map[string]string) {
	d.vmState.mu.Lock()
	d.vmState.byBox = states
	d.vmState.mu.Unlock()

	free, freeErr := box.FreeBytes(d.config.LimaHome)
	attached := d.tasks.BoxIDs()
	var events []vmEvent
	d.vmState.mu.Lock()
	for boxID, taskID := range attached {
		h := d.tasks.Get(taskID)
		if h == nil || h.tc == nil {
			continue
		}
		broken := states[boxID] == qemuStateIOError
		said := d.vmState.saidIOError[boxID]
		if broken == said {
			continue
		}
		if broken {
			d.vmState.saidIOError[boxID] = true
			events = append(events, vmEvent{tc: h.tc, message: ioErrorMessage(boxID, free, freeErr)})
			continue
		}
		delete(d.vmState.saidIOError, boxID)
		events = append(events, vmEvent{tc: h.tc, message: fmt.Sprintf(
			"The VM of box %s runs again: qemu reports it %s, not paused by an I/O error", boxID, dashWord(states[boxID]))})
	}
	// A box no task owns keeps no memory of a transition: its next task hears of
	// the state it finds.
	for boxID := range d.vmState.saidIOError {
		if _, ok := attached[boxID]; !ok {
			delete(d.vmState.saidIOError, boxID)
		}
	}
	d.vmState.mu.Unlock()
	for _, event := range events {
		d.emit(event.tc, "disk", event.message)
	}
}

// vmEvent is one task event an answer from qemu raised, held until the lock of
// the cache is released: emitting under it would put a task's consumer in the
// driver's lock.
type vmEvent struct {
	tc      *drivers.TaskConfig
	message string
}

// ioErrorMessage is the sentence of a VM that qemu stopped on an I/O error.
func ioErrorMessage(boxID string, free int64, freeErr error) string {
	msg := fmt.Sprintf("The VM of box %s is paused by qemu (io-error): the host could not write its disk.", boxID)
	if freeErr != nil {
		return msg + " A host disk that is full is the usual reason."
	}
	return fmt.Sprintf("%s %s is free on the node; a full host disk is the usual reason.", msg, gibString(free))
}

// qemuStatuses returns the qemu status the driver last read, per box id. A VM
// whose status is not known is absent from the map.
func (d *Driver) qemuStatuses() map[string]string {
	d.vmState.mu.Lock()
	defer d.vmState.mu.Unlock()
	out := make(map[string]string, len(d.vmState.byBox))
	for boxID, status := range d.vmState.byBox {
		out[boxID] = status
	}
	return out
}

// gibString renders a byte count in GiB, the unit a person reads free space in.
func gibString(n int64) string {
	return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
}

// dashWord renders an empty qemu status in a sentence.
func dashWord(status string) string {
	if status == "" {
		return "unknown"
	}
	return status
}
