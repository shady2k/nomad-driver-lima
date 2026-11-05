package driver

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/plugins/drivers"
	"github.com/stretchr/testify/require"

	"github.com/shady2k/nomad-driver-lima/internal/box"
)

// TestStreamStatsSamplesTheVMProcesses: the box's VM processes are found by
// the pid files in the instance directory, and their usage reaches Nomad.
func TestStreamStatsSamplesTheVMProcesses(t *testing.T) {
	dir := t.TempDir()
	pid := strconv.Itoa(os.Getpid())
	require.NoError(t, os.WriteFile(filepath.Join(dir, box.DriverPIDFile(box.VMTypeQEMU)), []byte(pid+"\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ha.pid"), []byte(pid), 0o644))

	d := &Driver{}
	h := newTaskHandle(&drivers.TaskConfig{ID: "t1"}, TaskConfig{BoxID: "box1"})
	h.setBoxDetails(boxState{InstanceDir: dir})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ch := make(chan *drivers.TaskResourceUsage, 1)
	go d.streamStats(ctx, h, 50*time.Millisecond, ch)

	select {
	case usage := <-ch:
		require.NotNil(t, usage.ResourceUsage)
		require.Greater(t, usage.ResourceUsage.MemoryStats.RSS, uint64(0))
		require.Greater(t, usage.Timestamp, int64(0))
	case <-ctx.Done():
		t.Fatal("no stats sample arrived")
	}
}

// TestSampleBoxSaysWhyThereIsNoSample: an instance directory without pid files
// and a box limactl does not list is a reason, not a silent gap.
func TestSampleBoxSaysWhyThereIsNoSample(t *testing.T) {
	d := &Driver{}
	h := newTaskHandle(&drivers.TaskConfig{ID: "t1"}, TaskConfig{BoxID: "box1"})
	h.setBoxDetails(boxState{InstanceDir: t.TempDir()})
	_, err := d.sampleBox(context.Background(), h)
	require.ErrorContains(t, err, "no VM process found")

	h = newTaskHandle(&drivers.TaskConfig{ID: "t2"}, TaskConfig{BoxID: "box2"})
	_, err = d.sampleBox(context.Background(), h)
	require.ErrorContains(t, err, "instance directory")
}

// TestVMPIDIsAnEventAndNotAnAttribute: Nomad 2.0 consumes no DriverAttributes (its
// TaskStatus has none since TaskHandle was removed), so an attribute is invisible
// to every reader. The pid of the VM therefore reaches a reader as a task event,
// which `box status` prints, and never as an attribute nobody reads.
func TestVMPIDIsAnEventAndNotAnAttribute(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := NewDriver(ctx, hclog.NewNullLogger())
	events, err := d.TaskEvents(ctx)
	require.NoError(t, err)

	dir := t.TempDir()
	pid := strconv.Itoa(os.Getpid())
	require.NoError(t, os.WriteFile(filepath.Join(dir, box.DriverPIDFile(box.VMTypeQEMU)), []byte(pid+"\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ha.pid"), []byte(pid), 0o644))

	h := newTaskHandle(&drivers.TaskConfig{ID: "t1", Name: "box", AllocID: "alloc1"}, TaskConfig{BoxID: "box1"})
	h.setBoxDetails(boxState{InstanceDir: dir})
	_, err = d.sampleBox(ctx, h)
	require.NoError(t, err)

	select {
	case event := <-events:
		require.Contains(t, event.Message, "VM process of box box1: pid "+pid)
		require.Equal(t, "stats", event.Annotations["kind"])
	case <-time.After(2 * time.Second):
		t.Fatal("no task event named the pid of the VM")
	}
	require.NotContains(t, h.taskStatus().DriverAttributes, "vm_pid",
		"an attribute is invisible through Nomad 2.0: the event is the record")
}
