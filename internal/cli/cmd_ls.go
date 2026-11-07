package cli

import (
	"context"
	"fmt"
	"strings"
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

// stateCell is the STATE column of `box ls`. A VM qemu has paused says so in the
// state a person reads -- "paused:io-error" -- while the JSON keeps Nomad's state
// and carries qemu's in a field of its own, because every command switches on the
// state and a frozen VM is still a box Nomad runs.
func stateCell(box BoxRecord) string {
	if box.VMState == "" {
		return dash(box.State)
	}
	return "paused:" + box.VMState
}

// untilCell says when the disk of a box that is not running is deleted: how long
// is left, "due" for one the pass has not deleted yet, and "not seen" for a box
// no pass has seen not running -- which is a fact, not a time invented for it.
func untilCell(box BoxRecord, now time.Time) string {
	switch box.State {
	case "running", "starting", "unknown", "deleted", "gone":
		return "-"
	}
	if box.RemovesAt == "" {
		if box.TermUnknown != "" {
			return "unknown"
		}
		return "not seen"
	}
	at, err := time.Parse(time.RFC3339, box.RemovesAt)
	if err != nil {
		return "?"
	}
	left := at.Sub(now)
	if left <= 0 {
		return "due"
	}
	return humanDuration(left)
}

// lsOutput is the JSON document of `box ls`.
type lsOutput struct {
	Boxes []BoxRecord `json:"boxes"`
}

// cmdList lists the boxes on the cluster, or one project's boxes.
func (a *App) cmdList(ctx context.Context, args []string) error {
	fs := newFlagSet("ls")
	project := fs.String("project", "", "show only the boxes of this project")
	mine := fs.Bool("mine", false, "show only the boxes of the project this directory belongs to "+
		"($BOX_PROJECT, else the name of its git repository)")
	if err := parseFlags(fs, args, 0); err != nil {
		return err
	}
	only, err := a.lsProject(*project, *mine, flagSet(fs, "project"))
	if err != nil {
		return err
	}
	boxes, term, err := a.collectBoxes(ctx)
	if err != nil {
		return err
	}
	if only != "" {
		boxes = boxesOfProject(boxes, only)
		if len(boxes) == 0 {
			if a.JSON {
				return a.printJSON(lsOutput{Boxes: []BoxRecord{}})
			}
			a.note("this cluster has no boxes of the project %s; `box ls` lists every box", only)
			return nil
		}
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
	// What qemu says about a VM it stopped: a box the host cannot write to is
	// "running" to Nomad and frozen in fact, and this column is where that shows.
	a.fillVMStates(ctx, boxes)
	// How long each running box has been idle is the one thing only its guest
	// knows; it is asked in parallel, under the guest call bound, and a box that
	// does not answer says so in its cell.
	activities := a.guestActivities(ctx, boxes)
	for i := range boxes {
		res, ok := activities[boxes[i].ID]
		if !ok {
			continue
		}
		boxes[i].Activity = res.activity
		if res.err != nil {
			boxes[i].GuestError = res.err.Error()
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
			dash(box.Project),
			dash(box.Node),
			dash(box.Arch),
			stateCell(box),
			fmt.Sprintf("%d", box.CPUs),
			fmt.Sprintf("%d MiB", box.MemoryMiB),
			fmt.Sprintf("%d GiB", box.DiskGiB),
			untilCell(box, now),
			humanBytes(box.VolumeCapacityBytes),
			diskColumn(box),
			alloc,
			uptime,
			idleCell(box),
		})
	}
	a.printTable([]string{"ID", "PROJECT", "NODE", "ARCH", "STATE", "CPU", "MEM", "DISK", "DISK UNTIL", "VOLUME",
		"USED", "ALLOC", "UPTIME", "IDLE"}, rows)
	// A box that is not running has a term, and the column says how much of it is
	// left. A box no pass has seen not running has no term yet: say which they
	// are, and do not give them a time.
	var unseen, unknownTerm []string
	for _, box := range boxes {
		switch box.State {
		case "running", "starting", "unknown", "deleted", "gone":
			continue
		}
		if box.TermUnknown != "" {
			unknownTerm = append(unknownTerm, box.ID)
			continue
		}
		if box.RemovesAt == "" {
			unseen = append(unseen, box.ID)
		}
	}
	if len(unseen) > 0 {
		if term.known() {
			a.note("no reap pass has seen %s not running yet: a pass runs every 15 minutes (`box reap`), and a disk "+
				"is deleted %s after the first pass that sees its box stopped", strings.Join(unseen, ", "),
				termWords(term.term))
		} else {
			a.note("no reap pass has seen %s not running yet: a pass runs every 15 minutes (`box reap`), and a disk "+
				"is deleted after the cluster's stopped-box term, which cannot be read now: %s",
				strings.Join(unseen, ", "), term.why)
		}
	}
	if len(unknownTerm) > 0 {
		a.note("the cluster's stopped-box term cannot be read, so DISK UNTIL is unknown for %s: %s",
			strings.Join(unknownTerm, ", "), term.why)
	}
	for _, box := range boxes {
		if box.Note != "" {
			a.note("%s: %s", box.ID, box.Note)
		}
		if box.VMState != "" {
			a.note("%s: qemu has paused its VM (%s); a full host disk is the usual reason; "+
				"`box status %s` says what the node reports, and resuming it is the operator's decision", box.ID, box.VMState, box.ID)
		}
	}
	return nil
}
