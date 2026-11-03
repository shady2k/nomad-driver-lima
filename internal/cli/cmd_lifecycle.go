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
		out.Warn = "the box was already stopped; its disk is untouched"
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
	out.Warn = "the disk was kept; `box start " + id + "` runs the box again"
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
		out.Warn = "the box is already running"
		return a.printLifecycle(&out)
	}
	if !a.volumeExists(ctx, id) {
		return errorf("the disk of box %s is gone: the volume %s does not exist, so the box cannot start. "+
			"`box rm %s --yes` removes the job, or `box create %s ...` makes a new box", id, BoxVolumeName(id), id, id)
	}
	spec, err := SpecFromJob(job)
	if err != nil {
		return err
	}
	// The executors variable is read again: a start is the moment to pick up
	// credentials that were added since the box was created.
	spec.ExecutorItems = a.executorItems(ctx)
	spec.VaultRole = a.vaultRoleFor(spec.VaultRole)
	engineered, err := RenderJob(spec)
	if err != nil {
		return err
	}
	resp, err := a.Nomad.RegisterJob(ctx, engineered)
	if err != nil {
		return wrapf(err, "Nomad rejected the job %s", BoxJobName(id))
	}
	evalID := ""
	if resp != nil {
		evalID = resp.EvalID
	}
	if !*wait {
		out.Warn = "the box is starting; `box status " + id + "` shows when it is ready"
		return a.printLifecycle(&out)
	}
	rec, err := a.waitForBoxRunning(ctx, id, evalID, *timeout)
	if err != nil {
		out.Box = rec
		return err
	}
	out.Box = rec
	out.Warn = "the box runs on its old disk"
	return a.printLifecycle(&out)
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
		ok, err := a.confirm(fmt.Sprintf("delete box %s and its disk (%d GiB of work)? [y/N] ", id, box.DiskGiB))
		if err != nil {
			return err
		}
		if !ok {
			a.note("nothing was deleted")
			return nil
		}
	}
	out := lifecycleOutput{Action: "rm", Box: box, Volume: BoxVolumeName(id), Deleted: true}

	if err := a.Nomad.StopJob(ctx, BoxJobName(id), true); err != nil && !isNotFoundErr(err) {
		return wrapf(err, "Nomad could not delete the job %s", BoxJobName(id))
	}
	if box.AllocationID != "" {
		if err := a.waitForNoLiveAllocs(ctx, BoxJobName(id), *timeout); err != nil {
			return err
		}
	}

	volumes, err := a.Nomad.HostVolumes(ctx)
	if err != nil {
		return wrapf(err, "cannot list the volumes")
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
				return wrapf(err2, "Nomad could not delete the volume %s, so the disk is still on node %s",
					stub.Name, stub.NodeID)
			}
		}
		deleted = append(deleted, stub.Name)
	}
	if len(deleted) == 0 {
		out.Warn = "the job was deleted; there was no volume left to delete"
	}
	box.State = "deleted"
	out.Box = box
	if len(deleted) > 0 {
		out.Changes = append(out.Changes, "deleted volume "+strings.Join(deleted, ", "))
		// Nomad answers the volume delete before every server has dropped the
		// registration, and `box create` refuses a name that is still
		// registered. Wait for it, so rm -> create with the same id works.
		if err := a.waitForNoVolume(ctx, BoxVolumeName(id), *timeout); err != nil {
			return err
		}
	}
	return a.printLifecycle(&out)
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
	diskGrew := false
	if *diskGiB > 0 {
		if *diskGiB < spec.DiskGiB {
			return usagef("the disk of box %s is %d GiB and can only grow, not shrink to %d GiB", id, spec.DiskGiB, *diskGiB)
		}
		if *diskGiB != spec.DiskGiB {
			out.Changes = append(out.Changes, fmt.Sprintf("disk %d -> %d GiB", spec.DiskGiB, *diskGiB))
			diskGrew = true
			spec.DiskGiB = *diskGiB
		}
	}
	if len(out.Changes) == 0 {
		out.Warn = "nothing to change: the box already has these values"
		return a.printLifecycle(&out)
	}

	spec.ExecutorItems = a.executorItems(ctx)
	spec.VaultRole = a.vaultRoleFor(spec.VaultRole)
	engineered, err := RenderJob(spec)
	if err != nil {
		return err
	}
	if diskGrew {
		if err := a.growVolume(ctx, id, spec.DiskGiB); err != nil {
			return err
		}
	}
	resp, err := a.Nomad.RegisterJob(ctx, engineered)
	if err != nil {
		return wrapf(err, "Nomad rejected the new size for box %s", id)
	}
	evalID := ""
	if resp != nil {
		evalID = resp.EvalID
	}
	if !*wait {
		out.Warn = "the box is being resized; `box status " + id + "` shows when it is back"
		return a.printLifecycle(&out)
	}
	rec, err := a.waitForBoxRunning(ctx, id, evalID, *timeout)
	if err != nil {
		out.Box = rec
		return err
	}
	out.Box = rec
	return a.printLifecycle(&out)
}

// growVolume raises the capacity Nomad records for the box disk.
func (a *App) growVolume(ctx context.Context, id string, diskGiB int) error {
	volumes, err := a.Nomad.HostVolumes(ctx)
	if err != nil {
		return wrapf(err, "cannot list the volumes")
	}
	for _, stub := range volumes {
		if stub == nil || stub.Name != BoxVolumeName(id) {
			continue
		}
		bytes := int64(diskGiB) * (1 << 30)
		if stub.CapacityBytes >= bytes {
			return nil
		}
		volume, err := a.Nomad.HostVolume(ctx, stub.ID)
		if err != nil {
			return wrapf(err, "cannot read the volume %s", stub.Name)
		}
		volume.RequestedCapacityMinBytes = bytes
		volume.RequestedCapacityMaxBytes = bytes
		if _, err := a.Nomad.RegisterHostVolume(ctx, volume); err != nil {
			return wrapf(err, "Nomad could not raise the capacity of %s to %d GiB", stub.Name, diskGiB)
		}
		return nil
	}
	return notFoundf("the volume %s of box %s does not exist, so its disk cannot grow", BoxVolumeName(id), id)
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
