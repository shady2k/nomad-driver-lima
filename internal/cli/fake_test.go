package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/stretchr/testify/require"
)

// maxTestWaits is how many times one command of a test may ask to sleep before
// the test fails. No command of this repo comes near it; a wait that asks more
// often is a wait that cannot end (see the sleep in newTestApp).
const maxTestWaits = 100000

// testWaitSpinPanic is what such a wait panics with.
const testWaitSpinPanic = "a wait loop does not end: the test clock does not move (fakeClock.step = 0) and the test sleep is immediate. " +
	"Give the command a clock that moves (newTestApp), or make it not wait (--wait=false); " +
	"every poll of the fake cluster is kept, and this test once took a test binary to 11 GB"

// fakeClock is a clock that moves one step per read, so a wait loop finishes
// deterministically instead of sleeping.
//
// A command that follows a task log reads it from a goroutine of its own, so the
// clock is asked for the time from two goroutines: the lock is what makes the
// same fake usable by both (the real clock is time.Now, which is safe).
type fakeClock struct {
	mu   sync.Mutex
	now  time.Time
	step time.Duration
}

// advance moves the fake clock forward, which a test does where a real wait
// would sleep.
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now
	c.now = c.now.Add(c.step)
	return now
}

// fakeCluster is an in-memory Nomad cluster.
type fakeCluster struct {
	mu      sync.Mutex
	nodes   map[string]*api.Node
	allocs  map[string]*api.Allocation
	jobs    map[string]*api.Job
	volumes map[string]*api.HostVolume
	// pendingVolumes are deleted volumes Nomad still lists, with the number of
	// listings left before they disappear.
	pendingVolumes map[string]int
	// deletedVolumes holds the records of those volumes.
	deletedVolumes map[string]*api.HostVolume
	variables      map[string]*api.Variable
	evals          map[string]*api.Evaluation
	allocSeq       int
	volumeSeq      int
	variableSeq    uint64
	registerNo     int

	// registerErr makes RegisterJob fail.
	registerErr error
	// registerLostErr makes RegisterJob fail *after* the job was registered:
	// it is a lost response, not a rejected job.
	registerLostErr error
	// registerErrAfter makes RegisterJob fail and every later Job read fail
	// too: the server took the request and then stopped answering.
	registerErrAfter bool
	// jobErr makes every Job read fail, which is how "Nomad cannot be asked"
	// looks to the command.
	jobErr error
	// jobAllocsErr makes every JobAllocations call fail, so a pass that has to
	// re-read the allocations of one box cannot.
	jobAllocsErr error
	// allocsErr makes the cluster-wide Allocations call fail.
	allocsErr error
	// nodeErr makes the read of one node fail, by node id: a node that is away.
	nodeErr map[string]error
	// volumeListErr makes HostVolumes fail, which is a cluster whose disks cannot
	// be listed.
	volumeListErr error
	// variableReadErr makes every VariableRead fail.
	variableReadErr error
	// variableReadErrPath makes the read of one variable path fail, which is how
	// a token that cannot read the cluster's stopped-box term looks while the
	// clocks of the boxes are still readable.
	variableReadErrPath map[string]error
	// variableCreateErr makes every VariableCreateChecked fail.
	variableCreateErr error
	// onVariableDeleteChecked runs before a checked delete is decided; a test
	// uses it to be the `box start` that cleared a box's clock first. It runs
	// with the cluster's lock held, so it must not take it.
	onVariableDeleteChecked func(c *fakeCluster, path string)
	// onVariableUpdateChecked runs before a checked update is decided; a test
	// uses it to be the `box start` that cleared a box's clock first. It runs
	// with the cluster's lock held, so it must not take it.
	onVariableUpdateChecked func(c *fakeCluster, path string)
	// onJobRead runs at the start of every Job read, with the number of the read
	// of that job; a test uses it to be a `box start` that registers the job
	// again between two reads. It runs with the cluster's lock held.
	onJobRead func(c *fakeCluster, id string, call int)
	// onDeleteHostVolume runs at the start of every DeleteHostVolume call and, when
	// it returns an error, that is what the call answers. It runs with the
	// cluster's lock held.
	onDeleteHostVolume func(c *fakeCluster, id string) error
	// stopErr makes StopJob fail.
	stopErr error
	// stopLeavesAllocFailed makes StopJob leave the allocation's client status
	// where it was when the VM failed: Nomad keeps a failed status for good, and
	// that is what a stop of a failed box waits on.
	stopLeavesAllocFailed bool
	// volumeDeleteLag is how many HostVolumes calls still list a volume after
	// it was deleted: Nomad's volume list can trail the delete.
	volumeDeleteLag int
	// createVolumeErr makes CreateHostVolume fail.
	createVolumeErr error
	// onRegister runs after a job was registered; a test uses it to fail a
	// placement or to leave the job without an allocation.
	onRegister func(c *fakeCluster, job *api.Job)
	// exec is the guest transport every allocation shares.
	exec ExecTransport
	// logData is what the log stream returns, per log file.
	logData map[string]string
	// logSteps scripts the calls of a log stream: the nth call serves the nth
	// step, and the last step repeats. A step with an error is a stream that
	// dies after its frames, which is what a client restart looks like.
	logSteps map[string][]logStep
	// logCalls counts the Logs calls per log type.
	logCalls map[string]int
	// calls records what the command asked Nomad for.
	calls []string
	// jobReads counts the reads of each job, so a test can act between two of
	// them.
	jobReads map[string]int
	// volumeRegisterErr fails the update of an existing volume.
	volumeRegisterErr error
	// variableWriteErr makes every VariableWrite fail, as a token without the
	// variables capability sees it.
	variableWriteErr error
	// variableDeleteErr makes every VariableDelete fail.
	variableDeleteErr error
	// callsMu guards calls. The streaming calls (Exec, Logs) run in a goroutine
	// of their own and must not take mu, so record has a lock of its own.
	callsMu sync.Mutex
}

func newFakeCluster() *fakeCluster {
	return &fakeCluster{
		nodes:          map[string]*api.Node{},
		allocs:         map[string]*api.Allocation{},
		jobs:           map[string]*api.Job{},
		volumes:        map[string]*api.HostVolume{},
		pendingVolumes: map[string]int{},
		variables:      map[string]*api.Variable{},
		evals:          map[string]*api.Evaluation{},
		logData:        map[string]string{},
	}
}

func (c *fakeCluster) record(what string) {
	c.callsMu.Lock()
	defer c.callsMu.Unlock()
	c.calls = append(c.calls, what)
}

func (c *fakeCluster) NodeList(ctx context.Context) ([]*api.NodeListStub, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("NodeList")
	out := make([]*api.NodeListStub, 0, len(c.nodes))
	for _, node := range c.nodes {
		out = append(out, &api.NodeListStub{ID: node.ID, Name: node.Name, Status: node.Status,
			SchedulingEligibility: node.SchedulingEligibility})
	}
	return out, nil
}

func (c *fakeCluster) NodeInfo(ctx context.Context, id string) (*api.Node, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("NodeInfo " + id)
	if err := c.nodeErr[id]; err != nil {
		return nil, err
	}
	node, ok := c.nodes[id]
	if !ok {
		return nil, notFoundError()
	}
	return node, nil
}

func (c *fakeCluster) NodeAllocations(ctx context.Context, nodeID string) ([]*api.Allocation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("NodeAllocations " + nodeID)
	var out []*api.Allocation
	for _, alloc := range c.allocs {
		if alloc.NodeID == nodeID {
			out = append(out, alloc)
		}
	}
	return out, nil
}

func (c *fakeCluster) JobList(ctx context.Context) ([]*api.JobListStub, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("JobList")
	out := make([]*api.JobListStub, 0, len(c.jobs))
	for _, job := range c.jobs {
		stub := &api.JobListStub{ID: derefStr(job.ID, ""), Name: derefStr(job.Name, ""),
			Status: derefStr(job.Status, ""), Meta: job.Meta}
		if job.Stop != nil && *job.Stop {
			stub.Stop = true
		}
		out = append(out, stub)
	}
	return out, nil
}

func (c *fakeCluster) Job(ctx context.Context, id string) (*api.Job, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("Job " + id)
	if c.jobReads == nil {
		c.jobReads = map[string]int{}
	}
	c.jobReads[id]++
	if c.onJobRead != nil {
		c.onJobRead(c, id, c.jobReads[id])
	}
	if c.jobErr != nil {
		return nil, c.jobErr
	}
	job, ok := c.jobs[id]
	if !ok {
		return nil, notFoundError()
	}
	return job, nil
}

func (c *fakeCluster) JobAllocations(ctx context.Context, jobID string) ([]*api.AllocationListStub, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("JobAllocations " + jobID)
	if c.jobAllocsErr != nil {
		return nil, c.jobAllocsErr
	}
	out := make([]*api.AllocationListStub, 0, len(c.allocs))
	for _, alloc := range c.allocs {
		if alloc.JobID == jobID {
			out = append(out, allocStub(alloc))
		}
	}
	return out, nil
}

func (c *fakeCluster) RegisterJob(ctx context.Context, job *api.Job) (*api.JobRegisterResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := derefStr(job.ID, "")
	c.record("RegisterJob " + id)
	if c.registerErr != nil {
		if c.registerErrAfter {
			c.jobErr = c.registerErr
		}
		return nil, c.registerErr
	}
	c.registerNo++
	evalID := fmt.Sprintf("eval-%d", c.registerNo)
	job.Status = strPtr("running")
	job.Stop = boolPtr(false)
	c.jobs[id] = job
	c.evals[evalID] = &api.Evaluation{ID: evalID, JobID: id, Status: "complete"}
	if c.onRegister != nil {
		c.onRegister(c, job)
	}
	if c.registerLostErr != nil {
		// The server took the job; the answer never reached the caller.
		return nil, c.registerLostErr
	}
	return &api.JobRegisterResponse{EvalID: evalID}, nil
}

func (c *fakeCluster) StopJob(ctx context.Context, id string, purge bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record(fmt.Sprintf("StopJob %s purge=%v", id, purge))
	if c.stopErr != nil {
		return c.stopErr
	}
	job, ok := c.jobs[id]
	if !ok {
		return notFoundError()
	}
	if purge {
		delete(c.jobs, id)
		for allocID, alloc := range c.allocs {
			if alloc.JobID == id {
				delete(c.allocs, allocID)
			}
		}
		return nil
	}
	job.Stop = boolPtr(true)
	job.Status = strPtr("dead")
	for _, alloc := range c.allocs {
		if alloc.JobID == id {
			if c.stopLeavesAllocFailed {
				alloc.ClientStatus = "failed"
			} else {
				alloc.ClientStatus = "complete"
			}
			alloc.DesiredStatus = "stop"
			if task := alloc.TaskStates[boxTaskName]; task != nil {
				task.State = "dead"
			}
		}
	}
	return nil
}

func (c *fakeCluster) Allocations(ctx context.Context) ([]*api.AllocationListStub, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("Allocations")
	if c.allocsErr != nil {
		return nil, c.allocsErr
	}
	out := make([]*api.AllocationListStub, 0, len(c.allocs))
	for _, alloc := range c.allocs {
		out = append(out, allocStub(alloc))
	}
	return out, nil
}

func (c *fakeCluster) Allocation(ctx context.Context, id string) (*api.Allocation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("Allocation " + id)
	alloc, ok := c.allocs[id]
	if !ok {
		return nil, notFoundError()
	}
	return alloc, nil
}

func (c *fakeCluster) Evaluation(ctx context.Context, id string) (*api.Evaluation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("Evaluation " + id)
	eval, ok := c.evals[id]
	if !ok {
		return nil, notFoundError()
	}
	return eval, nil
}

func (c *fakeCluster) HostVolumes(ctx context.Context) ([]*api.HostVolumeStub, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("HostVolumes")
	if c.volumeListErr != nil {
		return nil, c.volumeListErr
	}
	out := make([]*api.HostVolumeStub, 0, len(c.volumes)+len(c.pendingVolumes))
	for id, left := range c.pendingVolumes {
		if left <= 1 {
			delete(c.pendingVolumes, id)
		} else {
			c.pendingVolumes[id] = left - 1
		}
		if volume := c.deletedVolumes[id]; volume != nil {
			out = append(out, &api.HostVolumeStub{
				ID: volume.ID, Name: volume.Name, PluginID: volume.PluginID, NodeID: volume.NodeID,
				CapacityBytes: volume.CapacityBytes, State: volume.State,
			})
		}
	}
	for _, volume := range c.volumes {
		out = append(out, &api.HostVolumeStub{
			ID: volume.ID, Name: volume.Name, PluginID: volume.PluginID, NodeID: volume.NodeID,
			CapacityBytes: volume.CapacityBytes, State: volume.State,
		})
	}
	return out, nil
}

func (c *fakeCluster) HostVolume(ctx context.Context, id string) (*api.HostVolume, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("HostVolume " + id)
	volume, ok := c.volumes[id]
	if !ok {
		return nil, notFoundError()
	}
	return volume, nil
}

func (c *fakeCluster) CreateHostVolume(ctx context.Context, volume *api.HostVolume) (*api.HostVolume, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("CreateHostVolume " + volume.Name)
	if c.createVolumeErr != nil {
		return nil, c.createVolumeErr
	}
	c.volumeSeq++
	volume.ID = fmt.Sprintf("vol-%d", c.volumeSeq)
	volume.CapacityBytes = volume.RequestedCapacityMaxBytes
	volume.State = api.HostVolumeStateReady
	if volume.NodeID == "" {
		volume.NodeID = "node-1"
	}
	c.volumes[volume.ID] = volume
	return volume, nil
}

func (c *fakeCluster) RegisterHostVolume(ctx context.Context, volume *api.HostVolume) (*api.HostVolume, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("RegisterHostVolume " + volume.Name)
	if _, exists := c.volumes[volume.ID]; exists && c.volumeRegisterErr != nil {
		return nil, c.volumeRegisterErr
	}
	if _, exists := c.volumes[volume.ID]; exists {
		for _, alloc := range c.allocs {
			if !isTerminalAlloc(alloc.ClientStatus) {
				return nil, fmt.Errorf("cannot update a volume in use")
			}
		}
	}
	volume.CapacityBytes = volume.RequestedCapacityMaxBytes
	c.volumes[volume.ID] = volume
	return volume, nil
}

func (c *fakeCluster) DeleteHostVolume(ctx context.Context, id string, force bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record(fmt.Sprintf("DeleteHostVolume %s force=%v", id, force))
	if c.onDeleteHostVolume != nil {
		if err := c.onDeleteHostVolume(c, id); err != nil {
			return err
		}
	}
	volume, ok := c.volumes[id]
	if !ok {
		return notFoundError()
	}
	for _, alloc := range c.allocs {
		if !isTerminalAlloc(alloc.ClientStatus) {
			if !force {
				return fmt.Errorf("volume %s is in use by allocation %s", id, alloc.ID)
			}
		}
	}
	delete(c.volumes, id)
	if c.volumeDeleteLag > 0 {
		if c.deletedVolumes == nil {
			c.deletedVolumes = map[string]*api.HostVolume{}
		}
		c.deletedVolumes[id] = volume
		c.pendingVolumes[id] = c.volumeDeleteLag
	}
	return nil
}

func (c *fakeCluster) VariableRead(ctx context.Context, path string) (*api.Variable, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("VariableRead " + path)
	if c.variableReadErr != nil {
		return nil, c.variableReadErr
	}
	if err := c.variableReadErrPath[path]; err != nil {
		return nil, err
	}
	v, ok := c.variables[path]
	if !ok {
		return nil, notFoundError()
	}
	return v, nil
}

func (c *fakeCluster) VariableWrite(ctx context.Context, v *api.Variable) (*api.Variable, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("VariableWrite " + v.Path)
	if c.variableWriteErr != nil {
		return nil, c.variableWriteErr
	}
	c.variables[v.Path] = v
	return v, nil
}

func (c *fakeCluster) VariableCreateChecked(ctx context.Context, v *api.Variable) (*api.Variable, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("VariableCreateChecked " + v.Path)
	if c.variableCreateErr != nil {
		return nil, c.variableCreateErr
	}
	if existing, ok := c.variables[v.Path]; ok {
		return nil, api.ErrCASConflict{Conflict: existing, CheckIndex: 0}
	}
	c.variableSeq++
	v.CreateIndex = c.variableSeq
	v.ModifyIndex = c.variableSeq
	c.variables[v.Path] = v
	return v, nil
}

func (c *fakeCluster) VariableUpdateChecked(ctx context.Context, v *api.Variable) (*api.Variable, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("VariableUpdateChecked " + v.Path)
	if c.onVariableUpdateChecked != nil {
		c.onVariableUpdateChecked(c, v.Path)
	}
	if c.variableWriteErr != nil {
		return nil, c.variableWriteErr
	}
	existing, ok := c.variables[v.Path]
	if !ok {
		return nil, notFoundError()
	}
	if existing.ModifyIndex != v.ModifyIndex {
		return nil, api.ErrCASConflict{Conflict: existing, CheckIndex: v.ModifyIndex}
	}
	c.variableSeq++
	v.CreateIndex = existing.CreateIndex
	v.ModifyIndex = c.variableSeq
	c.variables[v.Path] = v
	return v, nil
}

func (c *fakeCluster) VariableDelete(ctx context.Context, path string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("VariableDelete " + path)
	if c.variableDeleteErr != nil {
		return c.variableDeleteErr
	}
	if _, ok := c.variables[path]; !ok {
		return notFoundError()
	}
	delete(c.variables, path)
	return nil
}

func (c *fakeCluster) VariableDeleteChecked(ctx context.Context, path string, modifyIndex uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record("VariableDeleteChecked " + path)
	if c.onVariableDeleteChecked != nil {
		c.onVariableDeleteChecked(c, path)
	}
	if c.variableDeleteErr != nil {
		return c.variableDeleteErr
	}
	existing, ok := c.variables[path]
	if !ok {
		return notFoundError()
	}
	if existing.ModifyIndex != modifyIndex {
		return api.ErrCASConflict{Conflict: existing, CheckIndex: modifyIndex}
	}
	delete(c.variables, path)
	return nil
}

// nomadGC does what `nomad system gc` does to a dead job: the job and its history
// are deleted. The volume and the variable are Nomad's, not the job's, so they
// stay -- which is the whole point of a box's stored description.
func (c *fakeCluster) nomadGC() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, job := range c.jobs {
		if job.Stop == nil || !*job.Stop {
			continue
		}
		c.record("GC " + id)
		delete(c.jobs, id)
		for allocID, alloc := range c.allocs {
			if alloc.JobID == id {
				delete(c.allocs, allocID)
			}
		}
	}
}

// setExecutors puts the given items into the executors variable.
func (c *fakeCluster) setExecutors(items map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.variables[boxExecutorsVariable] = &api.Variable{Path: boxExecutorsVariable, Items: items}
}

func (c *fakeCluster) Exec(ctx context.Context, alloc *api.Allocation, task string, cmd []string,
	stdin io.Reader, stdout, stderr io.Writer, tty bool, termSize <-chan api.TerminalSize) (int, error) {
	c.record("Exec " + strings.Join(cmd, " "))
	if c.exec == nil {
		return 0, fmt.Errorf("no guest is attached to this fake cluster")
	}
	return c.exec.Exec(ctx, ExecOpts{Command: cmd, Stdin: stdin, Stdout: stdout, Stderr: stderr, TTY: tty, TermSize: termSize})
}

func (c *fakeCluster) Logs(ctx context.Context, alloc *api.Allocation, task, logType string, follow bool, offset int64) (<-chan *api.StreamFrame, <-chan error) {
	c.record(fmt.Sprintf("Logs %s %s %d", alloc.ID, logType, offset))
	c.mu.Lock()
	if c.logCalls == nil {
		c.logCalls = map[string]int{}
	}
	c.logCalls[logType]++
	n := c.logCalls[logType]
	steps := c.logSteps[logType]
	data, hasData := c.logData[logType]
	c.mu.Unlock()

	frames := make(chan *api.StreamFrame, 4)
	errCh := make(chan error, 1)
	if len(steps) > 0 {
		if n > len(steps) {
			n = len(steps)
		}
		step := steps[n-1]
		for _, frame := range step.frames {
			frames <- &api.StreamFrame{Data: []byte(frame)}
		}
		if step.err != nil {
			// A lost stream does not close its frames: Nomad reports it on the
			// error channel and leaves the stream open, which is what a client
			// that restarted does.
			errCh <- step.err
			return frames, errCh
		}
		close(frames)
		errCh <- io.EOF
		return frames, errCh
	}
	if hasData {
		frames <- &api.StreamFrame{Data: []byte(data)}
	}
	close(frames)
	errCh <- io.EOF
	return frames, errCh
}

// logStep is one call of a scripted log stream: its frames, then its error.
type logStep struct {
	frames []string
	err    error
}

// addNode registers a node with the box driver and the box-volume plugin.
func (c *fakeCluster) addNode(id, name, arch string, cpuShares, memoryMB, diskMB int64) *api.Node {
	node := &api.Node{
		ID:                    id,
		Name:                  name,
		Status:                api.NodeStatusReady,
		SchedulingEligibility: api.NodeSchedulingEligible,
		Attributes: map[string]string{
			"driver.box.version":                     "0.2.0",
			"driver.box.arch":                        LimaArch(arch),
			"driver.box.os":                          "linux",
			"driver.box.vm_type":                     "qemu",
			"driver.box.lima_version":                "2.2.0",
			"driver.box.instances":                   "0",
			"plugins.host_volume.box-volume.version": "0.1.0",
			"plugins.host_volume.mkdir.version":      "0.1.0",
			"os.name":                                "ubuntu",
		},
		NodeResources: &api.NodeResources{
			Cpu:    api.NodeCpuResources{CpuShares: cpuShares},
			Memory: api.NodeMemoryResources{MemoryMB: memoryMB},
			Disk:   api.NodeDiskResources{DiskMB: diskMB},
		},
		Drivers: map[string]*api.DriverInfo{
			"box": {Attributes: map[string]string{}, Healthy: true},
		},
	}
	c.nodes[id] = node
	return node
}

// addVolume registers the disk of a box, as `box create` leaves it: the volume
// exists on a node, and nothing else of the box has to.
func (c *fakeCluster) addVolume(boxID, nodeID string, capacityBytes int64) *api.HostVolume {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.volumeSeq++
	volume := &api.HostVolume{
		ID:            fmt.Sprintf("vol-%d", c.volumeSeq),
		Name:          BoxVolumeName(boxID),
		PluginID:      boxVolumePluginID,
		NodeID:        nodeID,
		CapacityBytes: capacityBytes,
		State:         api.HostVolumeStateReady,
	}
	c.volumes[volume.ID] = volume
	return volume
}

// setNodeStatus changes what a node reports, which is what a node that went down
// looks like to a pass.
func (c *fakeCluster) setNodeStatus(nodeID, status string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if node := c.nodes[nodeID]; node != nil {
		node.Status = status
	}
}

// setNodeDiskRoom publishes what the driver reports about the filesystem a node
// writes its box disks to: the space free, the size of the filesystem and the part
// of it the node keeps for itself. free < 0 removes the free space, which is a
// driver that did not measure it.
func (c *fakeCluster) setNodeDiskRoom(nodeID string, free, total, reserve int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	node := c.nodes[nodeID]
	if node == nil {
		return
	}
	if node.Attributes == nil {
		node.Attributes = map[string]string{}
	}
	if free < 0 {
		delete(node.Attributes, boxDiskAttrPrefix+"free_bytes")
	} else {
		node.Attributes[boxDiskAttrPrefix+"free_bytes"] = strconv.FormatInt(free, 10)
	}
	if total > 0 {
		node.Attributes[boxDiskAttrPrefix+"total_bytes"] = strconv.FormatInt(total, 10)
	}
	node.Attributes[boxDiskAttrPrefix+"reserve_bytes"] = strconv.FormatInt(reserve, 10)
}

// addNodeDiskLocked publishes the driver's disk usage attributes for one box
// on a node, and keeps the node totals right. The caller holds the mutex.
func (c *fakeCluster) addNodeDiskLocked(nodeID, boxID string, used, virtual int64) {
	node := c.nodes[nodeID]
	if node == nil {
		return
	}
	if node.Attributes == nil {
		node.Attributes = map[string]string{}
	}
	const prefix = "driver.box.disk."
	node.Attributes[prefix+boxID+".used_bytes"] = strconv.FormatInt(used, 10)
	node.Attributes[prefix+boxID+".virtual_bytes"] = strconv.FormatInt(virtual, 10)
	var totalUsed, totalVirtual int64
	for key, value := range node.Attributes {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		rest := strings.TrimPrefix(key, prefix)
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			continue
		}
		switch {
		case rest == "used_bytes":
			totalUsed = n
		case rest == "virtual_bytes":
			totalVirtual = n
		case rest == "free_bytes", rest == "image_cache_free_bytes":
			// The room left on a filesystem, not the footprint of a disk.
		case strings.HasSuffix(rest, ".used_bytes"):
			totalUsed += n
		case strings.HasSuffix(rest, ".virtual_bytes"):
			totalVirtual += n
		}
	}
	node.Attributes[prefix+"used_bytes"] = strconv.FormatInt(totalUsed, 10)
	node.Attributes[prefix+"virtual_bytes"] = strconv.FormatInt(totalVirtual, 10)
}

// runBox makes an allocation of a job run; it is the normal outcome of a
// registration.
func (c *fakeCluster) runBox(job *api.Job) *api.Allocation {
	c.allocSeq++
	jobID := derefStr(job.ID, "")
	alloc := &api.Allocation{
		ID:            fmt.Sprintf("alloc-%d", c.allocSeq),
		JobID:         jobID,
		Job:           job,
		NodeID:        "node-1",
		NodeName:      "node-1",
		TaskGroup:     boxGroupName,
		ClientStatus:  "running",
		DesiredStatus: "run",
		CreateIndex:   uint64(c.allocSeq),
		CreateTime:    time.Now().UnixNano(),
		TaskStates: map[string]*api.TaskState{
			boxTaskName: {State: "running", StartedAt: time.Now().Add(-time.Minute)},
		},
	}
	c.allocs[alloc.ID] = alloc
	return alloc
}

func allocStub(alloc *api.Allocation) *api.AllocationListStub {
	return &api.AllocationListStub{
		ID:            alloc.ID,
		JobID:         alloc.JobID,
		NodeID:        alloc.NodeID,
		NodeName:      alloc.NodeName,
		ClientStatus:  alloc.ClientStatus,
		DesiredStatus: alloc.DesiredStatus,
		CreateIndex:   alloc.CreateIndex,
		CreateTime:    alloc.CreateTime,
		TaskStates:    alloc.TaskStates,
	}
}

// notFoundError mimics the 404 the Nomad API returns.
func notFoundError() error {
	return &httpError{status: 404, msg: "Unexpected response code: 404 (job not found)"}
}

type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string       { return e.msg }
func (e *httpError) StatusCode() int     { return e.status }
func (e *httpError) HasStatusCode() bool { return true }

// fakeGuest is an in-memory guest: a filesystem under root, the boxctl
// contract as canned answers, and tar via the same helpers the command uses.
type fakeGuest struct {
	root string
	// responses maps "boxctl <args joined by space>" to the JSON answer.
	responses map[string]string
	// errors maps the same key to an error document and an exit code.
	errors map[string]boxctlAnswer
	// runExit is the exit code of any command that is not boxctl, tar, mkdir.
	runExit int
	// block makes a `box run` command run until its context ends, as a
	// silent long command does.
	block bool
	// silent holds the boxctl calls this guest never answers ("info",
	// "session ls"): each one blocks until its context ends, which is what a
	// guest out of memory does.
	silent map[string]bool
	// lost is how many upcoming calls fail the way a lost connection does,
	// before the guest answers again; lostErr is what they report.
	lost    int
	lostErr error
	// loseWhen, when set, decides per call whether it is lost (it gets the
	// command and the number of the call), so a test can lose exactly one.
	loseWhen func(command []string, call int) error
	// onCall runs for every command the guest runs, on the caller's goroutine.
	onCall func(command []string)
	// deaf holds the boxctl calls never answer at all, not even when their
	// context ends: what a transport does while its server looks for a node that
	// is restarting, and what no bound of the caller's context can stop.
	deaf map[string]bool
	// deafCh keeps the deaf calls of a test from returning.
	deafCh chan struct{}
	// tty records whether a command asked for a terminal.
	tty bool
	// mu guards calls and stdin: a command that gave up on a call leaves its
	// goroutine running, so two calls can be in flight at once.
	mu sync.Mutex
	// calls records every command that ran.
	calls [][]string
	// stdin records what was sent on stdin.
	stdin []string
}

type boxctlAnswer struct {
	json string
	code int
}

func newFakeGuest(t *testing.T) *fakeGuest {
	t.Helper()
	return &fakeGuest{
		root:      t.TempDir(),
		responses: map[string]string{},
		errors:    map[string]boxctlAnswer{},
		silent:    map[string]bool{},
		deaf:      map[string]bool{},
		deafCh:    make(chan struct{}),
	}
}

// path maps a guest path onto the fake filesystem.
func (g *fakeGuest) path(guestPath string) string {
	clean := strings.TrimPrefix(filepath.Clean("/"+guestPath), "/")
	return filepath.Join(g.root, filepath.FromSlash(clean))
}

// write puts a file into the fake guest.
func (g *fakeGuest) write(t *testing.T, guestPath, content string) {
	t.Helper()
	full := g.path(guestPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
}

// read returns a file of the fake guest.
func (g *fakeGuest) read(t *testing.T, guestPath string) string {
	t.Helper()
	data, err := os.ReadFile(g.path(guestPath))
	require.NoError(t, err)
	return string(data)
}

func (g *fakeGuest) Exec(ctx context.Context, opts ExecOpts) (int, error) {
	g.mu.Lock()
	g.calls = append(g.calls, opts.Command)
	call := len(g.calls)
	g.mu.Unlock()
	if g.onCall != nil {
		g.onCall(opts.Command)
	}
	if g.loseWhen != nil {
		if err := g.loseWhen(opts.Command, call); err != nil {
			return -2, err
		}
	}
	if g.lost > 0 {
		g.lost--
		if g.lostErr != nil {
			return -2, g.lostErr
		}
		return -2, fmt.Errorf("cannot run boxctl in the box: unexpected EOF")
	}
	if opts.TTY {
		g.tty = true
	}
	switch {
	case len(opts.Command) == 0:
		return 0, fmt.Errorf("empty command")
	case opts.Command[0] == boxctlPath:
		return g.boxctl(ctx, opts)
	case opts.Command[0] == "tar":
		return g.tar(opts)
	case opts.Command[0] == "mkdir":
		dir := opts.Command[len(opts.Command)-1]
		return 0, os.MkdirAll(g.path(dir), 0o755)
	case g.block && opts.Command[0] == "setsid":
		g.recordStdin(opts)
		<-ctx.Done()
		return -1, ctx.Err()
	default:
		// The stdin of a command that is not boxctl or tar is what the command
		// would have read: a recipe, for one.
		g.recordStdin(opts)
		return g.runExit, nil
	}
}

// callLines is every command the fake guest ran, as one line each.
func (g *fakeGuest) callLines() []string {
	g.mu.Lock()
	calls := append([][]string(nil), g.calls...)
	g.mu.Unlock()
	out := make([]string, 0, len(calls))
	for _, call := range calls {
		out = append(out, strings.Join(call, " "))
	}
	return out
}

// recordStdin keeps what a command was given on stdin, so a test can assert on it.
func (g *fakeGuest) recordStdin(opts ExecOpts) {
	if opts.Stdin == nil {
		return
	}
	data, _ := io.ReadAll(opts.Stdin)
	g.mu.Lock()
	g.stdin = append(g.stdin, string(data))
	g.mu.Unlock()
}

func (g *fakeGuest) boxctl(ctx context.Context, opts ExecOpts) (int, error) {
	args := opts.Command[1:]
	key := strings.Join(args, " ")
	if g.silent[key] {
		<-ctx.Done()
		// The real transport returns Nomad's -2 and the context's error; the
		// command has to notice the bound, not the number.
		return -2, ctx.Err()
	}
	var stdin string
	if opts.Stdin != nil {
		data, _ := io.ReadAll(opts.Stdin)
		stdin = string(data)
	}
	g.mu.Lock()
	g.stdin = append(g.stdin, stdin)
	g.mu.Unlock()
	for prefix := range g.deaf {
		if strings.HasPrefix(key, prefix) {
			<-g.deafCh
			return -2, fmt.Errorf("the box did not answer")
		}
	}
	if answer, ok := lookupAnswer(g.errors, key); ok {
		if opts.Stdout != nil {
			_, _ = io.WriteString(opts.Stdout, answer.json)
		}
		return answer.code, nil
	}
	if answer, ok := lookupResponse(g.responses, key); ok {
		if opts.Stdout != nil {
			_, _ = io.WriteString(opts.Stdout, answer)
		}
		return 0, nil
	}
	if opts.Stderr != nil {
		fmt.Fprintf(opts.Stderr, "fake boxctl: unknown command %q", key)
	}
	return 1, nil
}

// lookupResponse finds the answer of a call. An answer registered for a prefix of
// the call answers it, so a test does not have to know a timeout that a clock
// computed; an exact answer always wins.
func lookupResponse(m map[string]string, key string) (string, bool) {
	if v, ok := m[key]; ok {
		return v, true
	}
	best, bestLen := "", -1
	for k, v := range m {
		if strings.HasPrefix(key, k) && len(k) > bestLen {
			best, bestLen = v, len(k)
		}
	}
	return best, bestLen >= 0
}

// lookupAnswer is lookupResponse for the error documents.
func lookupAnswer(m map[string]boxctlAnswer, key string) (boxctlAnswer, bool) {
	if v, ok := m[key]; ok {
		return v, true
	}
	best, bestLen, found := boxctlAnswer{}, -1, false
	for k, v := range m {
		if strings.HasPrefix(key, k) && len(k) > bestLen {
			best, bestLen, found = v, len(k), true
		}
	}
	return best, found
}

func (g *fakeGuest) tar(opts ExecOpts) (int, error) {
	args := opts.Command[1:]
	if len(args) < 3 {
		return 1, fmt.Errorf("fake tar: unsupported arguments %v", args)
	}
	var dir, names []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-C":
			i++
			dir = append(dir, args[i])
		case "-xf", "-cf", "-":
			// flags and the stream marker
		default:
			names = append(names, args[i])
		}
	}
	base := "."
	if len(dir) > 0 {
		base = dir[0]
	}
	root := g.path(base)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return 1, err
	}
	switch args[0] {
	case "-xf":
		if _, err := UnpackTar(opts.Stdin, root); err != nil {
			return 1, err
		}
		return 0, nil
	case "-cf":
		if len(names) == 0 {
			return 1, fmt.Errorf("fake tar: -cf without a name")
		}
		name := names[0]
		if err := PackTar(opts.Stdout, filepath.Join(root, filepath.FromSlash(name)), filepath.Base(name)); err != nil {
			if opts.Stderr != nil {
				fmt.Fprintf(opts.Stderr, "tar: %s: Cannot stat: No such file or directory\n", name)
			}
			return 2, nil
		}
		return 0, nil
	}
	return 1, fmt.Errorf("fake tar: unsupported command %q", args[0])
}

// fakeImages serves an index document without a network.
type fakeImages struct {
	body   string
	status int
	err    error
	calls  int
}

func (f *fakeImages) Fetch(ctx context.Context, url string) ([]byte, int, error) {
	f.calls++
	if f.err != nil {
		return nil, 0, f.err
	}
	return []byte(f.body), f.status, nil
}

// fakeTerminal is a terminal that is, or is not, interactive.
type fakeTerminal struct {
	interactive bool
	raw         bool
	restored    bool
}

func (t *fakeTerminal) IsInteractive() bool { return t.interactive }
func (t *fakeTerminal) MakeRaw() (func(), error) {
	t.raw = true
	return func() { t.restored = true }, nil
}
func (t *fakeTerminal) Sizes() <-chan api.TerminalSize {
	ch := make(chan api.TerminalSize, 1)
	ch <- api.TerminalSize{Width: 100, Height: 40}
	return ch
}

// testApp wires an App to the fakes.
type testApp struct {
	*App
	cluster *fakeCluster
	guest   *fakeGuest
	images  *fakeImages
	term    *fakeTerminal
	out     *bytes.Buffer
	errOut  *bytes.Buffer
	in      *bytes.Buffer
	clock   *fakeClock
	// workJob is the last bake or publish job the command registered. It is kept
	// here because the command purges it when it is done: the fake cluster holds
	// no record of it any more.
	workJob *api.Job
}

// newTestApp returns an App with a node, an image index and a guest.
func newTestApp(t *testing.T, indexBody string) *testApp {
	t.Helper()
	cluster := newFakeCluster()
	// 32 GiB, so the default box (4 GiB of RAM plus the VM overhead) fits in the
	// free memory of the node.
	cluster.addNode("node-1", "node-1", "amd64", 12000, 32768, 150000)
	guest := newFakeGuest(t)
	cluster.exec = guest
	images := &fakeImages{body: indexBody, status: 200}
	term := &fakeTerminal{}
	app := New(cluster, images, nil)
	app.Guest = func(ctx context.Context, alloc *api.Allocation) (ExecTransport, error) { return guest, nil }
	app.Terminal = term
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	in := &bytes.Buffer{}
	app.Out = out
	app.ErrOut = errOut
	app.In = in
	clock := &fakeClock{now: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC), step: time.Second}
	app.Now = clock.Now
	// A sleep that notices the caller stopping is what a real wait does; a
	// command that follows a stream would otherwise run on after its context
	// ended.
	//
	// It counts, because it does not wait. A wait loop's deadline comes from the
	// test clock, and a test that freezes that clock (fakeClock.step = 0) gives
	// the loop a deadline that never passes: with an immediate sleep the loop
	// then spins, and every poll is a call the fake cluster records, so the test
	// binary grows without bound. That is how one run reached 11 GB and had the
	// kernel kill other people's processes. maxTestWaits turns it into a failed
	// test with a stack.
	var waitMu sync.Mutex
	waits := 0
	app.Sleep = func(ctx context.Context, d time.Duration) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		// A command's wait may run in a goroutine of its own (a bake follows the
		// task log while it waits for it), so the count is guarded.
		waitMu.Lock()
		defer waitMu.Unlock()
		// A wait whose deadline comes from this clock cannot end while the clock
		// stands still, and while it spins it makes the fake cluster record every
		// call.
		if clock.step == 0 || waits > maxTestWaits {
			panic(testWaitSpinPanic)
		}
		waits++
		return nil
	}
	app.SetImageIndexURL("http://images.test/box-base/index.json")
	// A box's project is resolved from the environment and from the git
	// repository of the working directory; a test that reads either would get the
	// machine's, so both answer nothing here and a test that wants one sets it.
	app.Getenv = func(string) string { return "" }
	app.repoName = func(string) string { return "" }
	return &testApp{App: app, cluster: cluster, guest: guest, images: images, term: term, out: out, errOut: errOut, in: in, clock: clock}
}

// indexJSON is a valid index document with one image per architecture.
const indexJSON = `{
  "schema": 1,
  "updated_at": "2026-11-01T10:00:00Z",
  "images": {
    "1.0.0": {
      "amd64": {
        "url": "https://images.test/box-base/1.0.0/amd64/box-base-1.0.0-amd64.qcow2",
        "sha256": "3f9a2b7c5d1e0f3a4b6c8d9e0f1a2b3c4d5e6f708192a3b4c5d6e7f809132435",
        "size": 1056047104,
        "built_at": "2026-11-01T09:00:00Z"
      },
      "arm64": {
        "url": "https://images.test/box-base/1.0.0/arm64/box-base-1.0.0-arm64.qcow2",
        "sha256": "aa9a2b7c5d1e0f3a4b6c8d9e0f1a2b3c4d5e6f708192a3b4c5d6e7f809132435",
        "size": 1056047104,
        "built_at": "2026-11-01T09:10:00Z"
      }
    }
  }
}`

// platformIndexJSON is an index of a store that publishes platform bundles:
// one image that takes a bundle ("platform": "mount"), one that runs the boxd it
// carries, two bundle versions for amd64 and one for arm64.
const platformIndexJSON = `{
  "schema": 1,
  "updated_at": "2026-11-02T10:00:00Z",
  "images": {
    "1.0.0": {
      "amd64": {
        "url": "https://images.test/box-base/1.0.0/amd64/box-base-1.0.0-amd64.qcow2",
        "sha256": "5dbac55d46a0cae6368a4a820dd2cf610371873e506f8230ab5c961b7cc0ffc1",
        "size": 1056047104,
        "built_at": "2026-11-01T09:00:00Z",
        "platform": "mount"
      },
      "arm64": {
        "url": "https://images.test/box-base/1.0.0/arm64/box-base-1.0.0-arm64.qcow2",
        "sha256": "a6eb4379fa5d3a86fac99b7712cbddd0e0b9d2f8e6aa7d88672a94e6633fd712",
        "size": 1056047104,
        "built_at": "2026-11-01T09:10:00Z",
        "platform": "mount"
      }
    },
    "2.0.0": {
      "amd64": {
        "url": "https://images.test/box-base/2.0.0/amd64/box-base-2.0.0-amd64.qcow2",
        "sha256": "2b587ed93272cf2a1887989e16913bdee213e0ec0b44539bd37f5bb6549e2b3f",
        "size": 1056047104,
        "built_at": "2026-11-02T09:00:00Z"
      }
    }
  },
  "platforms": {
    "1.9.0": {
      "amd64": {
        "url": "https://images.test/box-platform/1.9.0/box-platform-1.9.0-linux-amd64.tar.gz",
        "sha256": "425210d28bc8b720946a9f17403d27bece85b3a131d958538bed3dea2edea986",
        "size": 48234496,
        "built_at": "2026-11-01T12:00:00Z",
        "boxd": "0.4.0",
        "tools": {"opencode": "1.2.0"}
      }
    },
    "1.10.0": {
      "amd64": {
        "url": "https://images.test/box-platform/1.10.0/box-platform-1.10.0-linux-amd64.tar.gz",
        "sha256": "502629c71242657dda4a82b69be1916831afdf959ae184073d290a278c11e6b0",
        "size": 50331648,
        "built_at": "2026-11-02T12:00:00Z",
        "boxd": "0.5.0",
        "tools": {"editor": "3.0.1", "opencode": "1.3.0"}
      },
      "arm64": {
        "url": "https://images.test/box-platform/1.10.0/box-platform-1.10.0-linux-arm64.tar.gz",
        "sha256": "8533c58866c54e271e52257f0e28dbc231e7c3e59291d687a5e040e29c7f6af7",
        "size": 50331648,
        "built_at": "2026-11-02T12:10:00Z",
        "boxd": "0.5.0",
        "tools": {"opencode": "1.3.0"}
      }
    }
  }
}`

// boxTaskConfig returns the task config of a registered box job, which is what
// the driver reads and what the platform tests assert on.
func boxTaskConfig(t *testing.T, ta *testApp, jobID string) map[string]any {
	t.Helper()
	ta.cluster.mu.Lock()
	job, ok := ta.cluster.jobs[jobID]
	ta.cluster.mu.Unlock()
	require.True(t, ok, "the job %s is registered", jobID)
	task, err := boxTask(job)
	require.NoError(t, err)
	return task.Config
}

// callCount counts the recorded Nomad calls that start with prefix.
func callCount(ta *testApp, prefix string) int {
	ta.cluster.callsMu.Lock()
	defer ta.cluster.callsMu.Unlock()
	n := 0
	for _, call := range ta.cluster.calls {
		if strings.HasPrefix(call, prefix) {
			n++
		}
	}
	return n
}

// run executes one command line the way main does.
func (ta *testApp) run(t *testing.T, args ...string) int {
	t.Helper()
	return ta.Run(context.Background(), args)
}
