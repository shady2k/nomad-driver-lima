package cli

import (
	"context"
	"fmt"
	"strings"
)

// capacityOutput is the JSON document of `box capacity`.
type capacityOutput struct {
	Nodes  []*NodeCapacity `json:"nodes"`
	Arches []archSummary   `json:"arches"`
}

// diskSummary renders the box disks of one node for the table.
func diskSummary(node *NodeCapacity) string {
	if node.DiskUsedBytes == 0 && node.DiskVirtualBytes == 0 {
		return "-"
	}
	if node.DiskVirtualBytes > 0 {
		return fmt.Sprintf("%s of %s", humanBytes(node.DiskUsedBytes), humanBytes(node.DiskVirtualBytes))
	}
	return humanBytes(node.DiskUsedBytes)
}

// uncountedSummary renders the VMs of a node that no Nomad allocation covers.
func uncountedSummary(node *NodeCapacity) string {
	if len(node.Uncounted) == 0 {
		return "-"
	}
	return fmt.Sprintf("%d (%s)", len(node.Uncounted), humanMiB(node.UncountedMemoryMiB))
}

// cmdCapacity lists the nodes that can run boxes.
func (a *App) cmdCapacity(ctx context.Context, args []string) error {
	fs := newFlagSet("capacity")
	if err := parseFlags(fs, args, 0); err != nil {
		return err
	}
	nodes, err := a.nodeCapacities(ctx)
	if err != nil {
		return err
	}
	// A box is its disk, and a box Nomad has no job for still holds one: count
	// the boxes from the boxes, not from the allocations, so that a listed disk
	// is never a node with no boxes.
	boxes, boxErr := a.collectBoxes(ctx)
	if boxErr != nil {
		a.note("note: the boxes could not be read, so BOXES counts only what Nomad's allocations say: %s", boxErr)
	}
	var byBox map[string]BoxRecord
	if boxErr == nil {
		countNodeBoxes(nodes, boxes)
		byBox = make(map[string]BoxRecord, len(boxes))
		for _, box := range boxes {
			byBox[box.ID] = box
		}
	}
	arches := summarizeArches(nodes)
	if a.JSON {
		return a.printJSON(capacityOutput{Nodes: nodes, Arches: arches})
	}
	if len(nodes) == 0 {
		a.note("no Nomad client node is registered; start a client with the box driver and the %s plugin", boxVolumePluginID)
		return nil
	}
	rows := make([][]string, 0, len(nodes)+len(arches))
	headers := []string{"NODE", "ARCH", "OS", "STATUS", "ELIGIBLE", "CPU free/total", "MEM free/total", "DISK free/total", "BOX DISKS used", "UNCOUNTED VMs", "DRIVER", "VOLUME PLUGIN", "BOXES"}
	for _, node := range nodes {
		eligible := "no"
		if node.Eligible {
			eligible = "yes"
		}
		driver := "missing"
		if node.Driver.Installed {
			driver = dash(node.Driver.Version)
			if node.Driver.LimaVersion != "" {
				driver += " lima " + node.Driver.LimaVersion
			}
		}
		plugin := "missing"
		for _, p := range node.VolumePlugins {
			if p.ID == boxVolumePluginID {
				plugin = dash(p.Version)
			}
		}
		rows = append(rows, []string{
			node.Name,
			dash(node.Arch),
			dash(node.OS),
			node.Status,
			eligible,
			fmt.Sprintf("%s/%s", humanMHz(node.CPU.Free), humanMHz(node.CPU.Total)),
			fmt.Sprintf("%s/%s", humanMiB(node.Memory.Free), humanMiB(node.Memory.Total)),
			fmt.Sprintf("%s/%s", humanMiB(node.Disk.Free), humanMiB(node.Disk.Total)),
			diskSummary(node),
			uncountedSummary(node),
			driver,
			plugin,
			boxesCell(node),
		})
	}
	a.printTable(headers, rows)

	// A running VM Nomad does not account for is what makes a node look roomier
	// than it is; say which one it is, so the operator can stop or remove it.
	for _, node := range nodes {
		for _, inst := range node.Uncounted {
			a.note("%s/%s: a running VM with no running allocation — %d vCPU, %s RAM, %d GiB disk are already taken",
				node.Name, inst.BoxID, inst.CPUs, humanMiB(inst.MemoryMiB), inst.DiskGiB)
		}
	}

	// An orphan is a Lima instance no task has owned for a while, or whose disk
	// is gone. The driver never removes one, so the operator has to hear of it.
	for _, node := range nodes {
		for _, name := range strings.Split(node.Driver.Orphans, ",") {
			if name = strings.TrimSpace(name); name != "" {
				a.note("%s/%s: an orphan, a VM with no box task for 10 minutes or without its disk; the driver leaves it "+
					"alone, remove it on the node with limactl delete as the driver's runtime user", node.Name, name)
			}
		}
	}

	// Per box, because the owner asked for the space each VM occupies, not only
	// the sum.
	for _, node := range nodes {
		for _, disk := range node.Disks {
			a.note("%s/%s: %s used of %s%s", node.Name, disk.BoxID, humanBytes(disk.UsedBytes),
				humanBytes(disk.VirtualBytes), diskBoxNote(byBox, disk.BoxID))
		}
	}

	// Architectures are never added together: a vCPU on amd64 cannot run an
	// arm64 image. The subtotals are therefore per architecture.
	for _, summary := range arches {
		a.note("%s: %s with %s free memory, %s MHz free CPU, %s free disk (%d of %d ready for boxes)",
			summary.Arch, describeCount(summary.Nodes, "node"), humanMiB(summary.MemoryMiB),
			humanMHz(summary.CPUShares), humanMiB(summary.DiskMiB), summary.ReadyNodes, summary.Nodes)
	}
	var problems []string
	for _, node := range nodes {
		if problem := node.NodeProblem(PlacementRequest{}); problem != "" {
			problems = append(problems, problem)
		}
	}
	if len(problems) > 0 {
		a.note("nodes that cannot take a new box: %s", strings.Join(problems, "; "))
	}
	return nil
}

// countNodeBoxes counts the boxes of every node by the node their disk is on,
// and takes the place of the allocation-based count: a box Nomad has no job for
// still holds a disk on its node, so a table that lists that disk must not say
// the node holds no boxes.
func countNodeBoxes(nodes []*NodeCapacity, boxes []BoxRecord) {
	byNode := make(map[string]*NodeCapacity, len(nodes))
	for _, node := range nodes {
		byNode[node.NodeID] = node
		node.Boxes, node.BoxesStopped, node.OrphanDisks = 0, 0, 0
	}
	for _, box := range boxes {
		node := byNode[box.NodeID]
		if node == nil {
			continue
		}
		switch box.State {
		case "orphan":
			node.OrphanDisks++
		case "deleted":
			// A box `box rm` removed has no disk left to count.
		default:
			node.Boxes++
			switch box.State {
			case "stopped", "gone":
				node.BoxesStopped++
			}
		}
	}
}

// boxesCell renders the box count of a node, saying which of them are stopped and
// which of its disks nothing describes.
func boxesCell(node *NodeCapacity) string {
	cell := fmt.Sprintf("%d", node.Boxes)
	var extra []string
	if node.BoxesStopped > 0 {
		extra = append(extra, fmt.Sprintf("%d stopped", node.BoxesStopped))
	}
	if node.OrphanDisks > 0 {
		extra = append(extra, fmt.Sprintf("%d orphan", node.OrphanDisks))
	}
	if len(extra) > 0 {
		cell += " (" + strings.Join(extra, ", ") + ")"
	}
	return cell
}

// diskBoxNote says what a box disk on a node belongs to, so the disk lines and
// the BOXES column of the table agree.
func diskBoxNote(boxes map[string]BoxRecord, boxID string) string {
	if boxes == nil {
		// The boxes could not be read: the disks are what the driver reports,
		// and nothing here knows what owns them.
		return ""
	}
	box, ok := boxes[boxID]
	if !ok {
		return " (no box of this cluster owns this disk)"
	}
	switch box.State {
	case "stopped":
		return " (" + boxID + " is stopped: its disk is kept, and `box start " + boxID + "` runs it again)"
	case "running":
		return " (" + boxID + " is running)"
	case "orphan":
		return " (" + boxID + " is an orphan: nothing describes this disk, and `box rm " + boxID + " --yes` deletes it)"
	default:
		return " (" + boxID + " is " + dash(box.State) + ")"
	}
}
