package cli

import (
	"context"
	"fmt"
	"strings"
)

// What qemu says about a box's VM.
//
// A host filesystem that fills up does not stop a VM: qemu keeps the guest's
// writes in its own buffer and, when one cannot reach the disk, stops the VM in
// the io-error run state. Nomad and Lima go on reporting the box as running, and
// the guest stops answering -- so every command that finds a silent guest has to
// say what the node knows, which is that the VM is frozen.
//
// The driver publishes qemu's own state per instance (driver.box.instance.<id>
// .qemu_status), and nothing here ever states a cause qemu did not report: an
// io-error is an I/O error, and a full disk is the usual reason for one.

// instanceState returns what the node's attributes say about one box's VM.
func (n *NodeCapacity) instanceState(boxID string) (BoxInstance, bool) {
	for _, inst := range n.Instances {
		if inst.BoxID == boxID {
			return inst, true
		}
	}
	return BoxInstance{}, false
}

// vmStatesOf reads the qemu state of every box on a node, from the node's
// attributes. The cache keeps one read per node for a command that looks at many
// boxes.
func (a *App) vmStatesOf(ctx context.Context, cache map[string]map[string]string, nodeID string) map[string]string {
	if nodeID == "" || cache == nil {
		return nil
	}
	if states, ok := cache[nodeID]; ok {
		return states
	}
	states := map[string]string{}
	node, err := a.Nomad.NodeInfo(ctx, nodeID)
	if err == nil {
		for _, inst := range BuildCapacity(node, nil).Instances {
			if inst.QEMUState != "" {
				states[inst.BoxID] = inst.QEMUState
			}
		}
	}
	cache[nodeID] = states
	return states
}

// fillVMStates puts what qemu says about each box's VM into its record. It is a
// field of its own and not BoxRecord.State: every command switches on that state,
// and a box whose VM is frozen is still a box Nomad runs.
func (a *App) fillVMStates(ctx context.Context, boxes []BoxRecord) {
	cache := map[string]map[string]string{}
	for i := range boxes {
		nodeID := boxes[i].NodeID
		if nodeID != "" {
			if state := a.vmStatesOf(ctx, cache, nodeID)[boxes[i].ID]; state != "" {
				boxes[i].VMState = state
			}
		}
	}
}

// vmStateSentence says qemu has paused the box's VM, in the words of a person who
// has to act on it, or "" when the node reports no such thing.
//
// free and freeKnown are the node's free disk space, which is the number that says
// whether the disk is the reason.
func vmStateSentence(boxID, status string, free int64, freeKnown bool) string {
	if status == "" || status == "running" {
		return ""
	}
	msg := fmt.Sprintf("the VM of box %s is paused by qemu (%s)", boxID, status)
	if status == "io-error" {
		msg += ": the host could not write its disk. A full host disk is the usual reason"
		if freeKnown {
			msg += fmt.Sprintf(", and %s is free on its node now", humanSpace(free))
		}
	}
	return msg + ". The guest does not answer while the VM is paused, and resuming it is the operator's " +
		"decision: `cont` in qemu's monitor (docs/known-limits.md)"
}

// vmNote is the sentence of a box whose VM qemu has paused, read from its node, or
// "" when the node reports none or cannot be read.
func (a *App) vmNote(ctx context.Context, box BoxRecord) string {
	if box.NodeID == "" {
		return ""
	}
	bounded, cancel := context.WithTimeout(ctx, guestCallTimeout)
	defer cancel()
	node, err := a.Nomad.NodeInfo(bounded, box.NodeID)
	if err != nil {
		return ""
	}
	capacity := BuildCapacity(node, nil)
	inst, ok := capacity.instanceState(box.ID)
	if !ok {
		return ""
	}
	return vmStateSentence(box.ID, inst.QEMUState, capacity.DiskFreeBytes, capacity.DiskFreeKnown)
}

// vmNoteFor is vmNote for a box named only by its id: a command that lost contact
// with a box says why when the node knows.
func (a *App) vmNoteFor(ctx context.Context, id string) string {
	bounded, cancel := context.WithTimeout(ctx, guestCallTimeout)
	defer cancel()
	box, _, err := a.findBox(bounded, id)
	if err != nil {
		return ""
	}
	return a.vmNote(ctx, box)
}

// withVMNote adds the sentence about a paused VM to a message, as a sentence of
// its own: the message ends where it did, and the reason follows it.
func withVMNote(message, note string) string {
	if note == "" {
		return message
	}
	if !strings.HasSuffix(message, ".") {
		message += ";"
	}
	return message + " " + strings.ToUpper(note[:1]) + note[1:] + "."
}
