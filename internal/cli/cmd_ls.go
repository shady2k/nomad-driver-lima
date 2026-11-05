package cli

import (
	"context"
	"fmt"
	"time"
)

// diskColumn renders what the disk occupies on the host, or "-" when the
// driver has not reported it yet.
func diskColumn(box BoxRecord) string {
	if box.DiskUsedBytes == 0 && box.DiskVirtualBytes == 0 {
		return "-"
	}
	if box.DiskVirtualBytes > 0 {
		return fmt.Sprintf("%s of %s", humanBytes(box.DiskUsedBytes), humanBytes(box.DiskVirtualBytes))
	}
	return humanBytes(box.DiskUsedBytes)
}

// lsOutput is the JSON document of `box ls`.
type lsOutput struct {
	Boxes []BoxRecord `json:"boxes"`
}

// cmdList lists the boxes on the cluster.
func (a *App) cmdList(ctx context.Context, args []string) error {
	fs := newFlagSet("ls")
	if err := parseFlags(fs, args, 0); err != nil {
		return err
	}
	boxes, err := a.collectBoxes(ctx)
	if err != nil {
		return err
	}
	volumes, err := a.Nomad.HostVolumes(ctx)
	if err != nil {
		// Volumes are a detail; the boxes are the answer.
		volumes = nil
	}
	capacity := map[string]int64{}
	volumeNode := map[string]string{}
	names := map[string]string{}
	for _, stub := range volumes {
		if stub != nil {
			capacity[stub.Name] = stub.CapacityBytes
			volumeNode[stub.Name] = stub.NodeID
		}
	}
	disks := map[string]map[string]BoxDisk{}
	for i := range boxes {
		if bytes, ok := capacity[boxes[i].Volume]; ok {
			boxes[i].VolumeCapacityBytes = bytes
		}
		// The disk of a stopped box is still on its volume's node, so the
		// volume is the fallback when there is no allocation.
		nodeID := boxes[i].NodeID
		if nodeID == "" {
			nodeID = volumeNode[boxes[i].Volume]
		}
		boxes[i].NodeID = nodeID
		if boxes[i].Node == "" {
			boxes[i].Node = a.nodeNameFor(ctx, names, nodeID)
		}
		byBox, err := a.nodeDisks(ctx, disks, nodeID)
		if err != nil {
			continue // the disk column is a detail; the boxes are the answer
		}
		if disk, ok := byBox[boxes[i].ID]; ok {
			boxes[i].DiskUsedBytes = disk.UsedBytes
			boxes[i].DiskVirtualBytes = disk.VirtualBytes
		}
	}
	if a.JSON {
		return a.printJSON(lsOutput{Boxes: boxes})
	}
	if len(boxes) == 0 {
		a.note("this cluster has no boxes yet; `box create <id> --image <version> ...` makes one")
		return nil
	}
	rows := make([][]string, 0, len(boxes))
	now := a.now()
	for _, box := range boxes {
		uptime := "-"
		if box.UptimeSeconds > 0 {
			uptime = humanDuration(time.Duration(box.UptimeSeconds) * time.Second)
		}
		alloc := shortID(box.AllocationID)
		rows = append(rows, []string{
			box.ID,
			dash(box.Node),
			dash(box.Arch),
			box.State,
			fmt.Sprintf("%d", box.CPUs),
			fmt.Sprintf("%d MiB", box.MemoryMiB),
			fmt.Sprintf("%d GiB", box.DiskGiB),
			humanBytes(box.VolumeCapacityBytes),
			diskColumn(box),
			alloc,
			uptime,
		})
	}
	a.printTable([]string{"ID", "NODE", "ARCH", "STATE", "CPU", "MEM", "DISK", "VOLUME", "USED", "ALLOC", "UPTIME"}, rows)
	for _, box := range boxes {
		if box.Note != "" {
			a.note("%s: %s", box.ID, box.Note)
		}
	}
	_ = now
	return nil
}
