package limactl

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseListJSONLines(t *testing.T) {
	out := `{"name":"box1","status":"Running","dir":"/lima/box1","vmType":"qemu","arch":"x86_64","cpus":2,"memory":1073741824,"disk":8589934592,"hostAgentPID":42,"driverPID":43}
{"name":"box2","status":"Stopped","dir":"/lima/box2","vmType":"qemu","arch":"x86_64","cpus":1,"memory":536870912,"disk":4294967296}
`
	got, err := parseList([]byte(out))
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, "box1", got[0].Name)
	require.True(t, got[0].IsRunning())
	require.Equal(t, int64(1073741824), got[0].MemoryBytes())
	require.Equal(t, 43, got[0].DriverPID)
	require.False(t, got[1].IsRunning())
	require.Equal(t, "box2", got[1].Name)
}

func TestParseListEmptyAndArray(t *testing.T) {
	got, err := parseList(nil)
	require.NoError(t, err)
	require.Empty(t, got)

	got, err = parseList([]byte("  \n"))
	require.NoError(t, err)
	require.Empty(t, got)

	got, err = parseList([]byte(`[{"name":"a","status":"Stopped"}]`))
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "a", got[0].Name)
}

func TestParseListRejectsGarbage(t *testing.T) {
	_, err := parseList([]byte("{not json"))
	require.Error(t, err)
}

func TestParseVersion(t *testing.T) {
	require.Equal(t, "2.2.0", parseVersion("limactl version 2.2.0\n"))
	require.Equal(t, "2.0.1", parseVersion("limactl version 2.0.1"))
	require.Equal(t, "1.2.3", parseVersion("limactl version 1.2.3 (abcdef)"))
}

func TestInstanceStatusHelpers(t *testing.T) {
	inst := &Instance{Name: "b", Status: StatusBroken, Message: "hostagent gone"}
	require.True(t, inst.IsBroken())
	require.False(t, inst.IsRunning())
	require.Contains(t, inst.StatusMessage(), "hostagent gone")

	inst = &Instance{Name: "b", Status: StatusBroken, Errors: []string{"qemu driver is running but host agent is not"}}
	require.Contains(t, inst.StatusMessage(), "host agent is not")

	require.Equal(t, "/lima/b", inst.InstanceDir("/lima"))
}

func TestCommandErrorKeepsStderr(t *testing.T) {
	err := &CommandError{Args: []string{"start", "box1"}, Stderr: "fatal: instance already exists", Err: errors.New("exit status 1")}
	require.Contains(t, err.Error(), "fatal: instance already exists")
	require.Contains(t, err.Error(), "start box1")
	require.True(t, IsNotFound(&CommandError{Args: []string{"list"}, Stderr: "No instance found"}))
	require.False(t, IsNotFound(errors.New("nope")))
}

func TestFormatGiB(t *testing.T) {
	require.Equal(t, "2", formatGiB(2))
	require.Equal(t, "1.5", formatGiB(1.5))
}

func TestEnvironSetsLimaHome(t *testing.T) {
	c := &Client{Bin: "limactl", Home: "/lima", Env: []string{"PATH=/bin", "LIMA_HOME=/wrong"}}
	env := strings.Join(c.Environ(), " ")
	require.Contains(t, env, "LIMA_HOME=/lima")
	require.NotContains(t, env, "LIMA_HOME=/wrong")
	require.Contains(t, env, "PATH=/bin")
}

func TestInstanceDirAndDiskPath(t *testing.T) {
	c := New("limactl", "/lima")
	require.Equal(t, "/lima/box1", c.InstanceDir("box1"))
	require.Equal(t, "/lima/box1/disk", c.DiskPath("box1"))
	require.Equal(t, "limactl", c.Bin)
}

// TestParseListAcceptsErrorObjects covers the shape Lima 2.2.0 prints for an
// instance that has errors: a list of {Op, Path, Err} objects, not strings. One
// such instance must not make the whole list unreadable, because the driver lists
// the instances to find every box on the node.
func TestParseListAcceptsErrorObjects(t *testing.T) {
	out := `{"name":"box1","status":"Running","dir":"/lima/box1","cpus":2,"hostAgentPID":42,"driverPID":43}
{"name":"broken1","status":"Broken","dir":"/lima/broken1","errors":[{"Op":"open","Path":"/lima/broken1/lima.yaml","Err":2}]}
{"name":"broken2","status":"Broken","dir":"/lima/broken2","errors":["hostagent did not respond"]}
{"name":"broken3","status":"Broken","dir":"/lima/broken3","errors":"one message"}
`
	got, err := parseList([]byte(out))
	require.NoError(t, err)
	require.Len(t, got, 4)
	require.Equal(t, "Running", got[0].Status)
	require.Equal(t, Errors{"open /lima/broken1/lima.yaml: 2"}, got[1].Errors)
	require.Contains(t, got[1].StatusMessage(), "open /lima/broken1/lima.yaml")
	require.Equal(t, Errors{"hostagent did not respond"}, got[2].Errors)
	require.Equal(t, Errors{"one message"}, got[3].Errors)
}
