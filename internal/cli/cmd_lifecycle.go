package cli

import (
	"bufio"
	"context"
	"fmt"
	"strings"
	"time"
)

// lifecycleOutput is the JSON document of stop, start, rm and resize.
type lifecycleOutput struct {
	Action  string    `json:"action"`
	Box     BoxRecord `json:"box"`
	Volume  string    `json:"volume,omitempty"`
	Deleted bool      `json:"deleted,omitempty"`
	Warn    string    `json:"warning,omitempty"`
	Changes []string  `json:"changes,omitempty"`
}

// cmdStop stops a box and keeps its disk.
func (a *App) cmdStop(ctx context.Context, args []string) error {
	fs := newFlagSet("stop")
	timeout := fs.Duration("timeout", 3*time.Minute, "how long to wait for the box to stop")
	if err := parseFlags(fs, args, 1); err != nil {
		return err
	}
	id := fs.Arg(0)
	box, _, err := a.findBox(ctx, id)
	if err != nil {
		return err
	}
	out := lifecycleOutput{Action: "stop", Box: box, Volume: BoxVolumeName(id)}
	if box.State == "stopped" {
		out.Warn = "the box was already stopped; its disk is deleted 24 hours after it stopped, unless `box start " +
			id + "` runs it again"
		return a.printLifecycle(&out)
	}
	if err := a.Nomad.StopJob(ctx, BoxJobName(id), false); err != nil {
		return wrapf(err, "Nomad could not stop the job %s", BoxJobName(id))
	}
	stopped, err := a.waitForBoxState(ctx, id, "stopped", *timeout)
	if err != nil {
		out.Box = stopped
		return err
	}
	out.Box = stopped
	out.Warn = "the disk is kept until the box has been stopped for 24 hours: `box start " + id +
		"` runs the box again now, and `box reap` deletes the disk after that"
	return a.printLifecycle(&out)
}

// cmdStart starts a stopped box on its existing disk.
func (a *App) cmdStart(ctx context.Context, args []string) error {
	fs := newFlagSet("start")
	timeout := fs.Duration("timeout", defaultWaitTimeout, "how long to wait for the box to run")
	wait := fs.Bool("wait", true, "wait until the box is running")
	if err := parseFlags(fs, args, 1); err != nil {
		return err
	}
	id := fs.Arg(0)
	box, job, err := a.findBox(ctx, id)
	if err != nil {
		return err
	}
	out := lifecycleOutput{Action: "start", Box: box, Volume: BoxVolumeName(id)}
	if box.State == "running" {
		// A box that runs is not on a term: a clock a pass wrote before it came
		// back is cleared here too.
		if _, err := a.clearBoxClock(ctx, id); err != nil {
			return err
		}
		// A start is also the moment to make sure the box can be started again
		// after Nomad collects its job: a box created before descriptions were
		// stored has none yet, and every start is a fresh one.
		if err := a.storeBoxSpec(ctx, id, job); err != nil {
			return wrapKeep(err, "the box %s runs, but its description was not stored, so `box start %s` could not "+
				"bring it back once Nomad collects its job", id, id)
		}
		out.Warn = "the box is already running; its description in " + BoxSpecVariable(id) + " is up to date"
		return a.printLifecycle(&out)
	}
	if box.State == "orphan" || job == nil {
		return orphanCannot(id, "start")
	}
	if !a.volumeExists(ctx, id) {
		return errorf("the disk of box %s is gone: the volume %s does not exist, so the box cannot start. "+
			"`box rm %s --yes` removes what is left of it, or `box create %s ...` makes a new box",
			id, BoxVolumeName(id), id, id)
	}
	spec, err := SpecFromJob(job)
	if err != nil {
		return err
	}
	// A spec that cannot be rendered stops here, before the clock of the box is
	// touched: a start that fails must leave the box exactly as it was, on its
	// old term.
	if err := spec.Validate(); err != nil {
		return err
	}
	// A start is also where the box picks up what its node reserves per vCPU now:
	// the node's overcommit may have changed since the box was created, and a
	// start re-registers the job anyway. A resize and an upgrade do the same.
	a.refreshCPUReservation(ctx, &spec, box.NodeID)
	// A node below its disk reserve takes no box back on: the disk is written to
	// as soon as the VM boots, and a node with no room left freezes it. Refused
	// here, before the job is registered, so nothing is changed.
	if err := a.lowDiskRefusal(ctx, box.NodeID, id, "start"); err != nil {
		return err
	}
	// The clock goes before the job is registered: a box that comes back is not on
	// a term any more, and a pass that finds the clock gone knows the box is
	// starting. The delete is checked, so a pass that claimed the clock first is
	// seen and this start stops here, before a job runs on a disk that is going.
	// A failure stops the start for the same reason.
	if err := a.claimBoxForStart(ctx, id); err != nil {
		return err
	}
	if !*wait {
		// The job is registered even when the command does not wait: the box
		// starts, and `box status` shows when it is ready.
		if _, err := a.startBoxJob(ctx, id, spec, false, *timeout); err != nil {
			return err
		}
		out.Warn = "the box is starting; `box status " + id + "` shows when it is ready"
		return a.printLifecycle(&out)
	}
	rec, err := a.startBoxJob(ctx, id, spec, true, *timeout)
	if err != nil {
		out.Box = rec
		return err
	}
	out.Box = rec
	out.Warn = "the box runs on its old disk"
	return a.printLifecycle(&out)
}

// startBoxJob registers a box's job again on its existing disk and, when wait
// is set, waits until the box runs. It is what `box start` does, and what a bake
// does to give the box back after it stopped it.
//
// The executors variable is read again: a start is the moment to pick up
// credentials that were added since the box was created (or created since it was
// stopped).
func (a *App) startBoxJob(ctx context.Context, id string, spec BoxSpec, wait bool, timeout time.Duration) (BoxRecord, error) {
	spec.ExecutorItems = a.executorItems(ctx)
	spec.VaultRole = a.vaultRoleFor(spec.VaultRole)
	a.noteNoVaultRole(spec)
	engineered, err := RenderJob(spec)
	if err != nil {
		return BoxRecord{}, err
	}
	resp, err := a.Nomad.RegisterJob(ctx, engineered)
	if err != nil {
		return BoxRecord{}, wrapf(err, "Nomad rejected the job %s%s", BoxJobName(id), registerDenied(err, BoxVolumeName(id)))
	}
	evalID := ""
	if resp != nil {
		evalID = resp.EvalID
	}
	// The description is what keeps the box after Nomad collects the job, so it
	// is written before the wait: a box that times out is still a box that can be
	// started again tomorrow.
	if err := a.storeBoxSpec(ctx, id, engineered); err != nil {
		return BoxRecord{}, wrapKeep(err, "the box %s was registered, but its description was not stored, so "+
			"`box start %s` cannot bring it back once Nomad collects its job", id, id)
	}
	if !wait {
		return BoxRecord{}, nil
	}
	return a.waitForBoxRunning(ctx, id, evalID, timeout)
}

// cmdRemove deletes a box and its disk.
func (a *App) cmdRemove(ctx context.Context, args []string) error {
	fs := newFlagSet("rm")
	yes := fs.Bool("yes", false, "delete without asking")
	timeout := fs.Duration("timeout", 3*time.Minute, "how long to wait for the box to stop before the disk is deleted")
	if err := parseFlags(fs, args, 1); err != nil {
		return err
	}
	id := fs.Arg(0)
	box, _, err := a.findBox(ctx, id)
	if err != nil {
		return err
	}
	if !*yes {
		question := fmt.Sprintf("delete box %s and its disk (%d GiB of work)? [y/N] ", id, box.DiskGiB)
		switch box.State {
		case "orphan":
			question = fmt.Sprintf("delete the disk %s of the orphan %s? Nothing describes it, so the disk is all "+
				"that is left (%d GiB): [y/N] ", BoxVolumeName(id), id, box.DiskGiB)
		case "stopped":
			question = fmt.Sprintf("delete box %s and its disk (%d GiB of work)? The box is stopped, and "+
				"`box start %s` would run it again: [y/N] ", id, box.DiskGiB, id)
		case "gone":
			question = fmt.Sprintf("delete what is left of box %s (the description in %s)? Its disk is gone: [y/N] ",
				id, BoxSpecVariable(id))
		}
		ok, err := a.confirm(question)
		if err != nil {
			return err
		}
		if !ok {
			a.note("nothing was deleted")
			return nil
		}
	}
	out := lifecycleOutput{Action: "rm", Box: box, Volume: BoxVolumeName(id), Deleted: true}

	deleted, err := a.deleteBoxVolumes(ctx, box, *timeout)
	if err != nil {
		return err
	}
	if len(deleted) == 0 {
		out.Warn = "there was no volume left to delete"
	}
	// The description goes with the disk: a box is the two of them, and a
	// description without a disk would be a box `box start` cannot run.
	//
	// The disk is gone at this point, so a failure here is partial work and says
	// so: the box is deleted and a record of it is left, which `box ls` still
	// shows and `box start` cannot run. Only the person can remove it.
	forgot, err := a.forgetBoxSpec(ctx, id)
	if err != nil {
		return partialf("the box %s is gone -- its job and its disk were deleted -- but its description %s is left behind: "+
			"so `box ls` still shows it and `box start %s` would find a box with no disk. Delete the record with "+
			"`nomad var delete %s` (%s)",
			id, BoxSpecVariable(id), id, BoxSpecVariable(id), errOr(err))
	}
	// The clock goes with the disk for the same reason, and one more: a box
	// created later with the same id must not inherit it.
	unclocked, err := a.clearBoxClock(ctx, id)
	if err != nil {
		return partialf("the box %s is gone -- its job and its disk were deleted -- but its clock %s is left behind: "+
			"a box created later with the same id would inherit it. Delete it with `nomad var delete %s` (%s)",
			id, BoxStoppedVariable(id), BoxStoppedVariable(id), errOr(err))
	}
	box.State = "deleted"
	out.Box = box
	if len(deleted) > 0 {
		out.Changes = append(out.Changes, "deleted volume "+strings.Join(deleted, ", "))
	}
	if forgot {
		out.Changes = append(out.Changes, "deleted the description "+BoxSpecVariable(id))
	}
	if unclocked {
		out.Changes = append(out.Changes, "deleted the clock "+BoxStoppedVariable(id))
	}
	return a.printLifecycle(&out)
}

// deleteBox deletes a box and its disk: the job, its allocations and the volume.
func (a *App) deleteBox(ctx context.Context, box BoxRecord, timeout time.Duration) error {
	_, err := a.deleteBoxVolumes(ctx, box, timeout)
	return err
}

// deleteBoxVolumes deletes the job and the disk of a box, and returns the volumes
// it deleted.
func (a *App) deleteBoxVolumes(ctx context.Context, box BoxRecord, timeout time.Duration) ([]string, error) {
	id := box.ID
	if err := a.Nomad.StopJob(ctx, BoxJobName(id), true); err != nil && !isNotFoundErr(err) {
		return nil, wrapf(err, "Nomad could not delete the job %s", BoxJobName(id))
	}
	if box.AllocationID != "" {
		if err := a.waitForNoLiveAllocs(ctx, BoxJobName(id), timeout); err != nil {
			return nil, err
		}
	}

	volumes, err := a.Nomad.HostVolumes(ctx)
	if err != nil {
		return nil, wrapf(err, "cannot list the volumes")
	}
	var deleted []string
	for _, stub := range volumes {
		if stub == nil || stub.Name != BoxVolumeName(id) {
			continue
		}
		if err := a.Nomad.DeleteHostVolume(ctx, stub.ID, false); err != nil {
			// The volume may still carry a terminal allocation; force lets the
			// plugin delete the disk anyway.
			if err2 := a.Nomad.DeleteHostVolume(ctx, stub.ID, true); err2 != nil {
				return nil, wrapf(err2, "Nomad could not delete the volume %s, so the disk is still on node %s",
					stub.Name, stub.NodeID)
			}
		}
		deleted = append(deleted, stub.Name)
	}
	if len(deleted) > 0 {
		// Nomad answers the volume delete before every server has dropped the
		// registration, and `box create` refuses a name that is still
		// registered. Wait for it, so rm -> create with the same id works.
		if err := a.waitForNoVolume(ctx, BoxVolumeName(id), timeout); err != nil {
			return deleted, err
		}
	}
	return deleted, nil
}

// cmdResize changes the size of a box.
//
// The driver does stop -> edit -> start, so the box is restarted; the disk is
// kept and can only grow.
func (a *App) cmdResize(ctx context.Context, args []string) error {
	fs := newFlagSet("resize")
	cpus := fs.Int("cpus", 0, "number of guest vCPUs")
	memoryMiB := fs.Int("memory-mib", 0, "guest RAM in MiB")
	diskGiB := fs.Int("disk-gib", 0, "box disk size in GiB (it can only grow)")
	timeout := fs.Duration("timeout", defaultWaitTimeout, "how long to wait for the box to come back")
	wait := fs.Bool("wait", true, "wait until the box is running again")
	if err := parseFlags(fs, args, 1); err != nil {
		return err
	}
	id := fs.Arg(0)
	if *cpus == 0 && *memoryMiB == 0 && *diskGiB == 0 {
		return usagef("box resize %s: give at least one of --cpus, --memory-mib or --disk-gib", id)
	}
	box, job, err := a.findBox(ctx, id)
	if err != nil {
		return err
	}
	if box.State == "orphan" || job == nil {
		return orphanCannot(id, "resize")
	}
	if box.JobMissing {
		return stoppedCannot(id, "box resize "+id)
	}
	spec, err := SpecFromJob(job)
	if err != nil {
		return err
	}
	out := lifecycleOutput{Action: "resize", Box: box, Volume: BoxVolumeName(id)}
	if *cpus > 0 {
		if err := requirePositive("cpus", *cpus); err != nil {
			return err
		}
		if *cpus != spec.CPUs {
			out.Changes = append(out.Changes, fmt.Sprintf("cpus %d -> %d", spec.CPUs, *cpus))
			spec.CPUs = *cpus
		}
	}
	if *memoryMiB > 0 {
		if *memoryMiB < 256 {
			return usagef("--memory-mib must be at least 256, got %d", *memoryMiB)
		}
		if *memoryMiB != spec.MemoryMiB {
			out.Changes = append(out.Changes, fmt.Sprintf("memory %d -> %d MiB", spec.MemoryMiB, *memoryMiB))
			spec.MemoryMiB = *memoryMiB
		}
	}
	growsDisk := false
	if *diskGiB > 0 {
		if *diskGiB < spec.DiskGiB {
			return usagef("the disk of box %s is %d GiB and can only grow, not shrink to %d GiB", id, spec.DiskGiB, *diskGiB)
		}
		if *diskGiB != spec.DiskGiB {
			out.Changes = append(out.Changes, fmt.Sprintf("disk %d -> %d GiB", spec.DiskGiB, *diskGiB))
			spec.DiskGiB = *diskGiB
			growsDisk = true
		}
	}
	if len(out.Changes) == 0 {
		out.Warn = "nothing to change: the box already has these values"
		return a.printLifecycle(&out)
	}
	a.refreshCPUReservation(ctx, &spec, box.NodeID)
	// A box that grows its disk needs the room to write it; a resize that only
	// changes CPU or memory writes nothing new, and a low node still runs it.
	if growsDisk {
		if err := a.lowDiskRefusal(ctx, box.NodeID, id, "grow its disk"); err != nil {
			return err
		}
	}
	note := a.shareNote(ctx, box.NodeID, id, spec.CPUs, spec.MemoryMiB, spec.DiskGiB)

	spec.ExecutorItems = a.executorItems(ctx)
	spec.VaultRole = a.vaultRoleFor(spec.VaultRole)
	a.noteNoVaultRole(spec)
	engineered, err := RenderJob(spec)
	if err != nil {
		return err
	}
	// The disk grows in the driver, from the job's disk_gib: Nomad's dynamic host
	// volume keeps the capacity its plugin reported when the volume was created,
	// and a re-registration of the volume does not change it (see the note
	// `box status` prints when the two disagree). So nothing here touches the
	// volume record.
	resp, err := a.Nomad.RegisterJob(ctx, engineered)
	if err != nil {
		return wrapf(err, "Nomad rejected the new size for box %s%s", id, registerDenied(err, BoxVolumeName(id)))
	}
	evalID := ""
	if resp != nil {
		evalID = resp.EvalID
	}
	if err := a.storeBoxSpec(ctx, id, engineered); err != nil {
		return wrapKeep(err, "the box %s is restarting at its new size, but its description was not stored, so "+
			"`box start %s` would bring it back at the old size once Nomad collects its job", id, id)
	}
	if !*wait {
		out.Warn = "the box is being resized; `box status " + id + "` shows when it is back"
		a.noteShare(note)
		return a.printLifecycle(&out)
	}
	rec, err := a.waitForBoxRunning(ctx, id, evalID, *timeout)
	if err != nil {
		out.Box = rec
		return err
	}
	out.Box = rec
	a.noteShare(note)
	return a.printLifecycle(&out)
}

// shareNote says how a box of this size takes more than a quarter of its node's
// CPU, memory or disk, or "" when it does not or the node cannot be described.
func (a *App) shareNote(ctx context.Context, nodeID, id string, cpus, memoryMiB, diskGiB int) string {
	nodes, err := a.nodeCapacities(ctx)
	if err != nil {
		return ""
	}
	for _, n := range nodes {
		if nodeID != "" && n.NodeID != nodeID {
			continue
		}
		if note := n.ShareNote(id, cpus, memoryMiB, diskGiB); note != "" {
			return note
		}
	}
	return ""
}

// lowDiskRefusal refuses a command that would write to a box disk on a node
// below its disk reserve, before anything is registered or changed.
//
// The driver never refuses to restart a task for this: a node that is low is a
// node whose boxes must keep running, and stopping them would turn low disk into
// dead boxes. What a node refuses is more work on it -- a box started or grown
// there -- and the sentence says what is free, what the node keeps back, and that
// nothing was changed.
func (a *App) lowDiskRefusal(ctx context.Context, nodeID, id, action string) error {
	if nodeID == "" {
		return nil
	}
	nodes, err := a.nodeCapacities(ctx)
	if err != nil {
		return nil // a node Nomad cannot describe is no proof of a full disk
	}
	for _, node := range nodes {
		if node.NodeID != nodeID && node.Name != nodeID {
			continue
		}
		if !node.LowDisk() {
			return nil
		}
		return errorf("box %s cannot %s on node %s: it has %s free where it keeps %s in reserve for itself, so "+
			"it takes no new work. Nothing was changed. Free space on the node first: `box rm <id> --yes` deletes "+
			"the disk of a box you no longer need", id, action, node.Name, humanSpace(node.DiskFreeBytes),
			humanSpace(node.DiskReserveBytes))
	}
	return nil
}

// printLifecycle writes the result of a lifecycle command.
func (a *App) printLifecycle(out *lifecycleOutput) error {
	if a.JSON {
		return a.printJSON(out)
	}
	switch out.Action {
	case "rm":
		a.note("deleted box %s and its disk", out.Box.ID)
	default:
		a.note("%s: box %s is %s%s", out.Action, out.Box.ID, out.Box.State, allocationSuffix(out.Box))
	}
	for _, change := range out.Changes {
		a.note("  %s", change)
	}
	if out.Warn != "" {
		a.note("  %s", out.Warn)
	}
	return nil
}

// confirm asks a yes/no question on the terminal.
func (a *App) confirm(question string) (bool, error) {
	if a.In == nil {
		return false, usagef("%s: no terminal to ask on; pass --yes to delete without asking", "")
	}
	if a.Terminal != nil && !a.Terminal.IsInteractive() {
		return false, usagef("box rm: refusing to delete a box from a non-interactive shell; pass --yes")
	}
	fmt.Fprint(a.ErrOut, question)
	reader := bufio.NewReader(a.In)
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return false, usagef("box rm: no answer was read; pass --yes to delete without asking")
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	}
	return false, nil
}

// volumeExists reports whether the disk volume of a box still exists.
func (a *App) volumeExists(ctx context.Context, id string) bool {
	volumes, err := a.Nomad.HostVolumes(ctx)
	if err != nil {
		return true // an unreadable list is not proof the disk is gone
	}
	for _, stub := range volumes {
		if stub != nil && stub.Name == BoxVolumeName(id) {
			return true
		}
	}
	return false
}

// waitForBoxState waits until a box reports the wanted state.
func (a *App) waitForBoxState(ctx context.Context, id, want string, timeout time.Duration) (BoxRecord, error) {
	deadline := a.now().Add(timeout)
	var last BoxRecord
	for {
		rec, _, err := a.findBox(ctx, id)
		if err != nil {
			return last, err
		}
		last = rec
		if rec.State == want {
			return rec, nil
		}
		if !a.now().Before(deadline) {
			return rec, timeoutf("the box %s is still %s after %s; `box status %s` shows what Nomad reports",
				id, rec.State, timeout, id)
		}
		if err := a.sleep(ctx, waitPollInterval); err != nil {
			return rec, wrapf(err, "stopped waiting for the box %s", id)
		}
	}
}

// waitForNoVolume waits until Nomad no longer lists a dynamic host volume, so
// that a box with the same id can be created right away.
//
// The registration is what `box create` collides with; the disk itself is
// already gone when the plugin answered the delete.
func (a *App) waitForNoVolume(ctx context.Context, name string, timeout time.Duration) error {
	deadline := a.now().Add(timeout)
	var last string
	for {
		volumes, err := a.Nomad.HostVolumes(ctx)
		if err != nil {
			last = "Nomad could not list the volumes: " + oneLine(err.Error(), 160)
		} else {
			last = ""
			for _, stub := range volumes {
				if stub == nil || stub.Name != name {
					continue
				}
				last = fmt.Sprintf("%s is still registered on node %s (state %s, id %s)",
					stub.Name, dash(stub.NodeID), dash(string(stub.State)), dash(stub.ID))
			}
			if last == "" {
				return nil
			}
		}
		if !a.now().Before(deadline) {
			return timeoutf("the disk was deleted, but %s after %s; a box with the id %s cannot be created until Nomad drops the registration "+
				"(`nomad volume status -type=host -verbose %s`)",
				last, timeout.Round(time.Second), strings.TrimPrefix(name, boxVolumePrefix), name)
		}
		if err := a.sleep(ctx, waitPollInterval); err != nil {
			return wrapf(err, "stopped waiting for the volume %s to disappear", name)
		}
	}
}

// waitForNoLiveAllocs waits until every allocation of a job is terminal, so
// the volume can be deleted.
func (a *App) waitForNoLiveAllocs(ctx context.Context, jobID string, timeout time.Duration) error {
	deadline := a.now().Add(timeout)
	for {
		stubs, err := a.Nomad.Allocations(ctx)
		if err != nil {
			return wrapf(err, "cannot list the allocations")
		}
		live := 0
		for _, stub := range stubs {
			if stub != nil && stub.JobID == jobID && !isTerminalAlloc(stub.ClientStatus) {
				live++
			}
		}
		if live == 0 {
			return nil
		}
		if !a.now().Before(deadline) {
			return timeoutf("%s allocation(s) of %s are still running after %s; the disk cannot be deleted while they use it",
				fmt.Sprint(live), jobID, timeout)
		}
		if err := a.sleep(ctx, waitPollInterval); err != nil {
			return wrapf(err, "stopped waiting for %s to stop", jobID)
		}
	}
}

// refreshCPUReservation sets what one vCPU of the box reserves on its node, so
// a box re-registered for any other reason gets the node's real figure. A node
// Nomad cannot describe leaves the job's figure as it is.
func (a *App) refreshCPUReservation(ctx context.Context, spec *BoxSpec, nodeID string) {
	if nodeID == "" {
		nodeID = spec.NodeID
	}
	nodes, err := a.nodeCapacities(ctx)
	if err != nil {
		return
	}
	for _, n := range nodes {
		if nodeID != "" && n.NodeID == nodeID {
			spec.CPUMHzPerVCPU = int(n.vcpuMHz())
			return
		}
	}
}

// noteShare writes a box's share of its shared node to stderr. It is a note and
// not a refusal: the command's exit code is the same with it and without it.
func (a *App) noteShare(note string) {
	if note == "" || a.ErrOut == nil {
		return
	}
	fmt.Fprintf(a.ErrOut, "note: %s\n", note)
}

// orphanCannot refuses a command that needs a box's job on a box whose disk
// nothing describes. There is no way back for such a box: nothing says how it
// was made, and inventing a job for it would run a VM on a disk nobody can
// account for.
func orphanCannot(id, action string) error {
	return errorf("the box %s is an orphan: its disk %s is there, and neither a job nor a description says how the "+
		"box was made, so `box %s` cannot act on it. `box rm %s --yes` deletes the disk, or `box create %s ...` "+
		"makes a new box", id, BoxVolumeName(id), action, id, id)
}

// stoppedCannot refuses a command that changes a box whose job Nomad no longer
// has. The box is its disk: a start brings it back, and the change can follow.
func stoppedCannot(id, next string) error {
	return errorf("the box %s is stopped: Nomad no longer has its job. Its disk is deleted 24 hours after it "+
		"stopped, and `box start %s` runs it again until then; `%s` works on it after that", id, id, next)
}
