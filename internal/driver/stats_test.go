package driver

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

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
