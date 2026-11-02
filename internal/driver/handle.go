package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/hashicorp/nomad/plugins/drivers"
)

// driverState is what the driver persists in the task handle so that
// RecoverTask can find the VM again after a client or plugin restart.
type driverState struct {
	Version     int        `json:"version"`
	BoxID       string     `json:"box_id"`
	VolumeDir   string     `json:"volume_dir"`
	DiskPath    string     `json:"disk_path"`
	InstanceDir string     `json:"instance_dir"`
	CgroupMode  string     `json:"cgroup_mode"`
	Config      TaskConfig `json:"config"`
}

const driverStateVersion = 1

// taskHandle is the driver's view of one running box.
type taskHandle struct {
	tc  *drivers.TaskConfig
	cfg TaskConfig

	boxID       string
	volumeDir   string
	diskPath    string
	instanceDir string
	cgroupMode  string

	mu    sync.RWMutex
	state drivers.TaskState
	// digest is the digest of the task files the guest last received; the
	// watcher re-copies them when Nomad re-renders them (a credential rotation).
	digest      string
	startedAt   time.Time
	completedAt time.Time
	exit        *drivers.ExitResult

	// done is closed once the VM stopped (or the handle was destroyed).
	done     chan struct{}
	doneOnce sync.Once

	logCancel context.CancelFunc
	watchStop context.CancelFunc

	stats *taskStats
}

func newTaskHandle(tc *drivers.TaskConfig, cfg TaskConfig) *taskHandle {
	return &taskHandle{
		tc:    tc,
		cfg:   cfg,
		boxID: cfg.BoxID,
		state: drivers.TaskStateUnknown,
		done:  make(chan struct{}),
		stats: newTaskStats(),
	}
}

func (h *taskHandle) setBoxDetails(st boxState) {
	h.volumeDir = st.VolumeDir
	h.diskPath = st.DiskPath
	h.instanceDir = st.InstanceDir
	h.cgroupMode = st.CgroupMode
}

// boxState is the subset of box.State the handle keeps.
type boxState struct {
	VolumeDir   string
	DiskPath    string
	InstanceDir string
	CgroupMode  string
}

func (h *taskHandle) markRunning(at time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state = drivers.TaskStateRunning
	h.startedAt = at
	h.completedAt = time.Time{}
	h.exit = nil
}

// finish records the exit result exactly once and closes done.
func (h *taskHandle) finish(res *drivers.ExitResult) {
	if res == nil {
		res = &drivers.ExitResult{}
	}
	h.mu.Lock()
	h.state = drivers.TaskStateExited
	h.completedAt = time.Now()
	h.exit = res.Copy()
	h.mu.Unlock()
	h.doneOnce.Do(func() { close(h.done) })
}

// setFilesDigest records the digest of the task files the guest now has.
func (h *taskHandle) setFilesDigest(digest string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.digest = digest
}

// filesDigest returns the digest of the task files the guest last received; an
// empty string means "unknown", which makes the watcher copy them again.
func (h *taskHandle) filesDigest() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.digest
}

// currentInstanceDir returns the Lima instance directory, if known.
func (h *taskHandle) currentInstanceDir() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.instanceDir
}

func (h *taskHandle) taskStatus() *drivers.TaskStatus {
	h.mu.RLock()
	defer h.mu.RUnlock()
	attrs := map[string]string{
		"box_id":      h.boxID,
		"disk_path":   h.diskPath,
		"cgroup_mode": h.cgroupMode,
	}
	if h.instanceDir != "" {
		attrs["instance_dir"] = h.instanceDir
	}
	return &drivers.TaskStatus{
		ID:               h.tc.ID,
		Name:             h.tc.Name,
		State:            h.state,
		StartedAt:        h.startedAt,
		CompletedAt:      h.completedAt,
		ExitResult:       h.exit.Copy(),
		DriverAttributes: attrs,
	}
}

// wait returns a channel that receives the exit result once the VM stopped.
func (h *taskHandle) wait() <-chan *drivers.ExitResult {
	ch := make(chan *drivers.ExitResult, 1)
	go func() {
		<-h.done
		h.mu.RLock()
		res := h.exit.Copy()
		h.mu.RUnlock()
		ch <- res
		close(ch)
	}()
	return ch
}

func (h *taskHandle) encodeState() ([]byte, error) {
	st := driverState{
		Version:     driverStateVersion,
		BoxID:       h.boxID,
		VolumeDir:   h.volumeDir,
		DiskPath:    h.diskPath,
		InstanceDir: h.instanceDir,
		CgroupMode:  h.cgroupMode,
		Config:      h.cfg,
	}
	return json.Marshal(st)
}

func decodeDriverState(data []byte) (*driverState, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("task handle carries no driver state")
	}
	var st driverState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("decode task handle state: %w", err)
	}
	if st.Version != driverStateVersion {
		return nil, fmt.Errorf("task handle state version %d is not supported (want %d)", st.Version, driverStateVersion)
	}
	if st.BoxID == "" {
		return nil, fmt.Errorf("task handle state has no box id")
	}
	return &st, nil
}

// taskStore maps task ids to handles.
type taskStore struct {
	mu    sync.RWMutex
	tasks map[string]*taskHandle
}

func newTaskStore() *taskStore { return &taskStore{tasks: map[string]*taskHandle{}} }

func (s *taskStore) Add(h *taskHandle) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks[h.tc.ID] = h
}

func (s *taskStore) Get(id string) *taskHandle {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.tasks[id]
}

func (s *taskStore) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tasks, id)
}

// OthersForBox returns the other task handles that use the same box. A box is
// shared by every allocation that names it, so a stop request from one of them
// must not take the VM away from the others.
func (s *taskStore) OthersForBox(boxID, taskID string) []*taskHandle {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*taskHandle
	for id, h := range s.tasks {
		if id != taskID && h.boxID == boxID {
			out = append(out, h)
		}
	}
	return out
}

// BoxIDs returns the box ids of all tasks the driver knows about.
func (s *taskStore) BoxIDs() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]string, len(s.tasks))
	for id, h := range s.tasks {
		out[h.boxID] = id
	}
	return out
}
