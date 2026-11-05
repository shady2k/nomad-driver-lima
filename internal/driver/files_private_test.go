package driver

import (
	"path/filepath"
	"testing"

	"github.com/hashicorp/nomad/plugins/drivers"
	"github.com/stretchr/testify/require"
)

// Nomad's TaskConfig.TaskDir leaves PrivateDir empty. A host that stages task
// files in the task's private directory got "" from it, and so a second copy of
// every credential outside that directory.
func TestTaskPrivateDirIsNamedByTheDriver(t *testing.T) {
	tc := &drivers.TaskConfig{Name: "box", AllocDir: "/data/alloc/abc"}
	require.Empty(t, tc.TaskDir().PrivateDir, "Nomad started filling it: taskPrivateDir can go")
	require.Equal(t, filepath.Join("/data/alloc/abc", "box", "private"), taskPrivateDir(tc))
}
