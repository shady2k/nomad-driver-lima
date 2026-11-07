package driver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/nomad/plugins/drivers"
	"github.com/hashicorp/nomad/plugins/shared/structs"

	"github.com/shady2k/nomad-driver-lima/internal/box"
)

// The two tasks that make a project image: a bake, which turns the disk of a
// sealed box into an image in the node's cache, and a publish, which puts such an
// image into the operator's store.
//
// Neither runs a VM. They are tasks of this driver because the disk and the image
// cache are on the node, and because a task is what Nomad schedules, reports and
// stops: the handle runs and exits like any other task, with code 0 or an error in
// the task event and in the exit.

// imageWorkStopTimeout is how long a stop waits for a bake or a publish to end
// after its context was cancelled. A conversion of a large disk finishes the
// chunk it is on and removes its temporary file.
const imageWorkStopTimeout = 60 * time.Second

// startImageWork starts a bake or a publish task and returns its handle.
func (d *Driver) startImageWork(tc *drivers.TaskConfig, cfg TaskConfig) (*drivers.TaskHandle, error) {
	h := newTaskHandle(tc, cfg)
	if cfg.Bake != nil {
		// The bake reads the disk of the box it was asked for, which the volume
		// of this task holds.
		volumeDir, err := diskVolumePath(tc)
		if err != nil {
			return nil, err
		}
		h.setBoxDetails(boxState{VolumeDir: volumeDir, DiskPath: box.VolumeDiskPath(volumeDir, cfg.BoxID)})
	}
	h.markRunning(time.Now())
	d.tasks.Add(h)
	d.startImageWorkGoroutine(h)
	state, err := h.encodeState()
	if err != nil {
		return nil, err
	}
	return &drivers.TaskHandle{Version: taskHandleVersion, Config: tc, State: drivers.TaskStateRunning, DriverState: state}, nil
}

// startImageWorkGoroutine runs the work of a bake or a publish and finishes the
// handle with its result.
func (d *Driver) startImageWorkGoroutine(h *taskHandle) {
	ctx, cancel := context.WithCancel(d.ctx)
	h.setWorkCancel(cancel)
	go func() {
		defer cancel()
		res := d.doImageWork(ctx, h)
		h.finish(res)
	}()
}

// doImageWork runs the one step the task is.
func (d *Driver) doImageWork(ctx context.Context, h *taskHandle) *drivers.ExitResult {
	switch {
	case h.cfg.Bake != nil:
		return d.bakeImage(ctx, h)
	case h.cfg.Reap != nil:
		return d.reapOnce(ctx, h)
	}
	return d.publishImage(ctx, h)
}

// publishImage uploads an image baked on this node into the operator's store.
func (d *Driver) publishImage(ctx context.Context, h *taskHandle) *drivers.ExitResult {
	p := h.cfg.Publish
	if d.boxm == nil {
		return imageWorkFailure(h, d, "the box driver is not configured on this node")
	}
	store, err := readStore(h.tc.TaskDir().SecretsDir)
	if err != nil {
		return imageWorkFailure(h, d, err.Error())
	}
	rec, err := d.bakedRecord(p)
	if err != nil {
		return imageWorkFailure(h, d, err.Error())
	}
	pub := newPublisher(store, func(kind, message string) { d.emit(h.tc, kind, message) })
	if err := pub.publishImage(ctx, rec, d.boxm.BakedImagePath(rec)); err != nil {
		if ctx.Err() != nil {
			return imageWorkFailure(h, d, fmt.Sprintf("the publish of %s %s was stopped", rec.Version, rec.Arch))
		}
		return imageWorkFailure(h, d, fmt.Sprintf("publishing %s %s failed: %v", rec.Version, rec.Arch, err))
	}
	d.emitData(h.tc, "publish", fmt.Sprintf("Published the image %s %s: sha256 %s, %s; %s/box-base/index.json names it",
		rec.Version, rec.Arch, rec.SHA256, humanSize(rec.Size), store.URL),
		map[string]string{
			"version": rec.Version,
			"arch":    rec.Arch,
			"sha256":  rec.SHA256,
			"index":   store.URL + "/box-base/index.json",
		})
	return &drivers.ExitResult{}
}

// imageWorkFailure records why a bake or a publish failed, in the task event and in
// the task's exit, so `nomad alloc status` and `box images` both say it.
func imageWorkFailure(h *taskHandle, d *Driver, message string) *drivers.ExitResult {
	d.emit(h.tc, "exit", message)
	return &drivers.ExitResult{ExitCode: 1, Err: errors.New(message)}
}

// bakedRecord finds the record of an image this node baked, and checks that the
// image itself is still in the cache.
func (d *Driver) bakedRecord(p *PublishConfig) (box.BakedRecord, error) {
	records, err := d.boxm.BakedRecords()
	if err != nil {
		return box.BakedRecord{}, fmt.Errorf("cannot read the images baked on this node: %v", err)
	}
	for _, rec := range records {
		if rec.Version != p.Version || rec.Arch != p.Arch {
			continue
		}
		if p.SHA256 != "" && !strings.EqualFold(rec.SHA256, p.SHA256) {
			return box.BakedRecord{}, fmt.Errorf("this node baked %s %s with sha256 %s, and the job names %s: "+
				"the image is not the one that was asked for", rec.Version, rec.Arch, rec.SHA256, p.SHA256)
		}
		if _, err := os.Stat(d.boxm.BakedImagePath(rec)); err != nil {
			return box.BakedRecord{}, fmt.Errorf("this node baked %s %s, but its image %s is no longer in the cache: "+
				"bake it again", rec.Version, rec.Arch, d.boxm.BakedImagePath(rec))
		}
		return rec, nil
	}
	return box.BakedRecord{}, fmt.Errorf("this node has no image %s %s in its cache: the image is baked on the node whose "+
		"volume holds the box, and `box images` lists the drafts of every node", p.Version, p.Arch)
}

// stopImageWork stops a bake or a publish and waits for it to end, so that what it
// wrote temporarily is gone before the task is called stopped.
func (d *Driver) stopImageWork(h *taskHandle) {
	if cancel := h.takeWorkCancel(); cancel != nil {
		cancel()
	}
	select {
	case <-h.done:
	case <-time.After(imageWorkStopTimeout):
		d.emit(h.tc, "exit", fmt.Sprintf("The %s did not stop within %s; what it wrote temporarily is in the image cache",
			imageWorkName(h), imageWorkStopTimeout))
	}
}

// imageWorkName names what a task does, for a message.
func imageWorkName(h *taskHandle) string {
	switch {
	case h.cfg.Bake != nil:
		return "bake of " + h.cfg.Bake.Version
	case h.cfg.Publish != nil:
		return "publish of " + h.cfg.Publish.Version
	case h.cfg.Reap != nil:
		return "reap pass"
	}
	return "box"
}

// setWorkCancel records how to stop the work of a bake or a publish.
func (h *taskHandle) setWorkCancel(cancel context.CancelFunc) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.workCancel = cancel
}

// takeWorkCancel takes the cancel of the work, once.
func (h *taskHandle) takeWorkCancel() context.CancelFunc {
	h.mu.Lock()
	defer h.mu.Unlock()
	cancel := h.workCancel
	h.workCancel = nil
	return cancel
}

// bakedAttributePrefix names the per-image attributes of a node: the images it
// baked, so that a coordinator can offer them without reading the node's disk.
const bakedAttributePrefix = "driver.box.baked."

// setBakedImages publishes what this node baked: one attribute per field of the
// record, under driver.box.baked.<version>.<field>. It is how `box images` shows
// a draft that exists on one node, and how `box create --image <version>` pins a
// box to that node.
func setBakedImages(fp *drivers.Fingerprint, m *box.Manager) {
	if m == nil {
		return
	}
	records, err := m.BakedRecords()
	if err != nil {
		return
	}
	// One attribute per record and field, and nothing at all when the cache holds
	// no baked image: clearGoneKeys removes the ones that are no longer here.
	for _, rec := range records {
		prefix := bakedAttributePrefix + rec.Version + "."
		setAttribute(fp, prefix+"arch", rec.Arch)
		setAttribute(fp, prefix+"name", rec.Name)
		setAttribute(fp, prefix+"sha256", rec.SHA256)
		setAttribute(fp, prefix+"size", fmt.Sprintf("%d", rec.Size))
		setAttribute(fp, prefix+"virtual_bytes", fmt.Sprintf("%d", rec.VirtualBytes))
		setAttribute(fp, prefix+"parent", rec.Parent)
		setAttribute(fp, prefix+"built_at", rec.BuiltAt)
		setAttribute(fp, prefix+"platform", rec.Platform)
		setAttribute(fp, prefix+"recipe", rec.Recipe)
	}
}

// setAttribute adds one string attribute to a fingerprint.
func setAttribute(fp *drivers.Fingerprint, key, value string) {
	if fp.Attributes == nil {
		fp.Attributes = map[string]*structs.Attribute{}
	}
	fp.Attributes[key] = structs.NewStringAttribute(value)
}

// workLog is the task's own log, where a bake writes its steps.
//
// Nomad keeps the last ten task events of a task, and a seal prints more lines
// than that: the events are the short version in `nomad alloc status`, and this
// is the whole of it, which `nomad alloc logs` and `box bake` read.
type workLog struct {
	f  *os.File
	mu sync.Mutex
}

// line writes one line, whole.
func (l *workLog) line(text string) {
	if l == nil || l.f == nil || text == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = fmt.Fprintln(l.f, text)
}

// Write writes one chunk of a command's output to the task log, so a reap pass
// can be handed the log as its ErrOut.
func (l *workLog) Write(p []byte) (int, error) {
	if l == nil || l.f == nil {
		return len(p), nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Write(p)
}

// close closes the log file.
func (l *workLog) close() {
	if l == nil || l.f == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = l.f.Close()
	l.f = nil
}

// workLog opens the task's stdout for the work of a bake. A task without a
// stdout path (a test) writes nowhere.
func (h *taskHandle) workLog() *workLog {
	if h.tc == nil || h.tc.StdoutPath == "" {
		return &workLog{}
	}
	f, err := os.OpenFile(h.tc.StdoutPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return &workLog{}
	}
	return &workLog{f: f}
}
