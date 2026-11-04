package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/stretchr/testify/require"
)

func TestCapacityOutput(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.cluster.addNode("node-2", "node-2", "arm64", 4000, 8000, 50000)

	require.Equal(t, ExitOK, ta.run(t, "capacity"))
	table := ta.out.String()
	require.Contains(t, table, "NODE")
	require.Contains(t, table, "ARCH")
	require.Contains(t, table, "node-1")
	require.Contains(t, table, "node-2")
	// The human output says what each architecture offers, separately.
	require.Contains(t, ta.errOut.String(), "amd64: 1 node with")
	require.Contains(t, ta.errOut.String(), "arm64: 1 node with")

	ta.out.Reset()
	ta.errOut.Reset()
	require.Equal(t, ExitOK, ta.run(t, "--json", "capacity"))
	var doc capacityOutput
	require.NoError(t, json.Unmarshal(ta.out.Bytes(), &doc))
	require.Len(t, doc.Nodes, 2)
	require.Len(t, doc.Arches, 2, "architectures are summarised, never added together")
	require.Equal(t, "amd64", doc.Arches[0].Arch)
	require.Equal(t, int64(12000), doc.Nodes[0].CPU.Total)

	// The disk the boxes occupy is part of the node's picture.
	ta.out.Reset()
	ta.errOut.Reset()
	ta.addBox(t, "dev1")
	require.Equal(t, ExitOK, ta.run(t, "--json", "capacity"))
	require.NoError(t, json.Unmarshal(ta.out.Bytes(), &doc))
	require.Equal(t, int64(300<<20), doc.Nodes[0].DiskUsedBytes)
	require.Equal(t, int64(8)<<30, doc.Nodes[0].DiskVirtualBytes)
	require.Len(t, doc.Nodes[0].Disks, 1)
	require.Equal(t, "dev1", doc.Nodes[0].Disks[0].BoxID)
}

// TestCapacityNamesOrphans: the driver never removes an orphan, so the human
// output names it with what to do about it.
func TestCapacityNamesOrphans(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	node := ta.cluster.addNode("node-2", "node-2", "arm64", 4000, 8000, 50000)
	node.Attributes["driver.box.orphans"] = "lost1,lost2"

	require.Equal(t, ExitOK, ta.run(t, "capacity"))
	notes := ta.errOut.String()
	require.Contains(t, notes, "node-2/lost1: an orphan")
	require.Contains(t, notes, "node-2/lost2: an orphan")
	require.Contains(t, notes, "limactl delete")
	require.NotContains(t, notes, "node-1/", "a node without orphans names none")
}

// TestCommandHelp: --help of a command prints its arguments and every flag
// and runs nothing; a guest command line after "--" keeps its own -h.
func TestCommandHelp(t *testing.T) {
	for _, args := range [][]string{
		{"create", "--help"}, {"create", "dev1", "-h"}, {"help", "create"},
	} {
		ta := newTestApp(t, indexJSON)
		require.Equal(t, ExitOK, ta.run(t, args...), ta.errOut.String())
		help := ta.errOut.String()
		require.Contains(t, help, "box create <id> --image")
		require.Contains(t, help, "--arch VALUE")
		require.Contains(t, help, "--memory-mib N")
		require.Contains(t, help, "(default 2048)")
		require.Empty(t, ta.cluster.jobs, "help registers nothing")
	}

	ta := newTestApp(t, indexJSON)
	require.Equal(t, ExitOK, ta.run(t, "session", "new", "--help"))
	require.Contains(t, ta.errOut.String(), "flags of box session new")
	require.Contains(t, ta.errOut.String(), "--model VALUE")

	ta = newTestApp(t, indexJSON)
	require.Equal(t, ExitOK, ta.run(t, "session", "--help"))
	require.Contains(t, ta.errOut.String(), "box session close <id> <session>")

	ta = newTestApp(t, indexJSON)
	require.Equal(t, ExitOK, ta.run(t, "cp", "--help"))
	require.Contains(t, ta.errOut.String(), "box cp <src> <dst>")

	// A missing argument names the help that says which.
	ta = newTestApp(t, indexJSON)
	require.Equal(t, ExitUsage, ta.run(t, "create"))
	require.Contains(t, ta.errOut.String(), "`box create --help`")
}

func TestCapacityWithoutNodes(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.cluster.nodes = map[string]*api.Node{}
	require.Equal(t, ExitOK, ta.run(t, "capacity"))
	require.Contains(t, ta.errOut.String(), "no Nomad client node is registered")
}

func TestImagesOutput(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	require.Equal(t, ExitOK, ta.run(t, "images"))
	table := ta.out.String()
	require.Contains(t, table, "VERSION")
	require.Contains(t, table, "1.0.0")
	require.Contains(t, table, "amd64")
	require.Contains(t, table, "arm64")
	require.Contains(t, ta.errOut.String(), "--memory-mib 2048", "the hint is the default create takes")

	ta.out.Reset()
	ta.errOut.Reset()
	require.Equal(t, ExitOK, ta.run(t, "images", "--json"))
	var doc imagesOutput
	require.NoError(t, json.Unmarshal(ta.out.Bytes(), &doc))
	require.True(t, doc.Available)
	require.Len(t, doc.Images, 2)
	require.Equal(t, "1.0.0", doc.Images[0].Version)
}

func TestImagesMissingIndex(t *testing.T) {
	ta := newTestApp(t, "")
	ta.images.status = 404
	// A missing index is not a failure: nobody published an image yet.
	require.Equal(t, ExitOK, ta.run(t, "images"))
	require.Contains(t, ta.errOut.String(), "no base images are published yet")

	ta.out.Reset()
	ta.errOut.Reset()
	require.Equal(t, ExitOK, ta.run(t, "images", "--json"))
	var doc imagesOutput
	require.NoError(t, json.Unmarshal(ta.out.Bytes(), &doc))
	require.False(t, doc.Available)
	require.Contains(t, doc.Error, "not published yet")
}

func TestListOutput(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.addBox(t, "dev2")

	require.Equal(t, ExitOK, ta.run(t, "ls"))
	table := ta.out.String()
	require.Contains(t, table, "dev1")
	require.Contains(t, table, "dev2")
	require.Contains(t, table, "running")

	ta.out.Reset()
	ta.errOut.Reset()
	require.Equal(t, ExitOK, ta.run(t, "ls", "--json"))
	var doc lsOutput
	require.NoError(t, json.Unmarshal(ta.out.Bytes(), &doc))
	require.Len(t, doc.Boxes, 2)
	require.Contains(t, table, "USED", "the table says what each disk occupies")
	require.Equal(t, "dev1", doc.Boxes[0].ID)
	require.Equal(t, "running", doc.Boxes[0].State)
	require.Equal(t, "node-1", doc.Boxes[0].Node)
	require.Equal(t, 2, doc.Boxes[0].CPUs)
	require.Equal(t, int64(8)<<30, doc.Boxes[0].VolumeCapacityBytes)
	require.NotNil(t, doc.Boxes[0].AllocationID)
	// The disk each VM occupies is the driver's number, not the volume capacity.
	require.Equal(t, int64(300<<20), doc.Boxes[0].DiskUsedBytes)
	require.Equal(t, int64(8)<<30, doc.Boxes[0].DiskVirtualBytes)
	require.Equal(t, "node-1", doc.Boxes[0].NodeID)
}

func TestListEmpty(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	require.Equal(t, ExitOK, ta.run(t, "ls"))
	require.Contains(t, ta.errOut.String(), "no boxes yet")
}

func TestStatusOutput(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	ta.guest.responses["info"] = `{"boxd_version":"0.3.0","executors":[{"name":"prime","version":"1","available":true}],"sessions":1,"mem_total_mib":1024,"mem_free_mib":900,"cpus":2}`
	ta.guest.responses["session ls"] = `{"sessions":[{"id":"s-1","executor":"prime","model":"p/m","state":"idle","last_turn":2,"title":"fix the tests"}]}`

	require.Equal(t, ExitOK, ta.run(t, "status", "dev1"))
	human := ta.errOut.String()
	require.Contains(t, human, "box dev1")
	require.Contains(t, human, "boxd 0.3.0")
	require.Contains(t, human, "s-1")

	ta.out.Reset()
	ta.errOut.Reset()
	require.Equal(t, ExitOK, ta.run(t, "status", "dev1", "--json"))
	var doc statusOutput
	require.NoError(t, json.Unmarshal(ta.out.Bytes(), &doc))
	require.Equal(t, "dev1", doc.Box.ID)
	require.Equal(t, "running", doc.Box.State)
	require.NotNil(t, doc.Guest)
	require.Equal(t, "0.3.0", doc.Guest.BoxdVersion)
	require.Len(t, doc.Sessions, 1)
	require.Equal(t, "fix the tests", doc.Sessions[0].Title)
	require.NotNil(t, doc.Volume)
	require.Equal(t, int64(8)<<30, doc.Volume.CapacityBytes)
	require.Equal(t, 2, doc.Spec.CPUs)
	require.Equal(t, 1024+BoxMemoryOverheadMiB, doc.Spec.TaskMemMiB)
	require.Equal(t, int64(300<<20), doc.Box.DiskUsedBytes)
	require.Equal(t, int64(8)<<30, doc.Box.DiskVirtualBytes)
}

func TestStatusWhenGuestIsDown(t *testing.T) {
	ta := newTestApp(t, indexJSON)
	ta.addBox(t, "dev1")
	// No boxctl answers: the Nomad half of the status is still useful.
	require.Equal(t, ExitOK, ta.run(t, "status", "dev1"))
	require.Contains(t, ta.errOut.String(), "guest")

	ta.out.Reset()
	ta.errOut.Reset()
	require.Equal(t, ExitOK, ta.run(t, "status", "dev1", "--json"))
	var doc statusOutput
	require.NoError(t, json.Unmarshal(ta.out.Bytes(), &doc))
	require.Nil(t, doc.Guest)
	require.NotEmpty(t, doc.GuestError)
}

func TestHumanHelpers(t *testing.T) {
	require.Equal(t, "-", dash(""))
	require.Equal(t, "x", dash("x"))
	require.Equal(t, "-", dashInt(0))
	require.Equal(t, "3", dashInt(3))
	require.Equal(t, "45s", humanDuration(45*time.Second))
	require.Equal(t, "2m03s", humanDuration(123*time.Second))
	require.Equal(t, "3h05m", humanDuration(3*time.Hour+5*time.Minute))
	require.Equal(t, "1.5 GiB", humanBytes(3<<29))
	require.Equal(t, "512 MiB", humanBytes(512<<20))
	require.Equal(t, "-", humanBytes(0))
	require.True(t, strings.HasPrefix(humanMiB(12288), "12.0 GiB"))
	require.Equal(t, 0, clampPercent(1, 0))
}
