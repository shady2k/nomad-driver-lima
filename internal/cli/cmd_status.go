package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/hashicorp/nomad/api"
)

// statusOutput is the JSON document of `box status`.
type statusOutput struct {
	Box        BoxRecord        `json:"box"`
	Spec       *statusSpec      `json:"spec"`
	Allocation *statusAlloc     `json:"allocation,omitempty"`
	TaskEvents []*api.TaskEvent `json:"task_events,omitempty"`
	Volume     *statusVolume    `json:"volume,omitempty"`
	Guest      *BoxInfo         `json:"guest,omitempty"`
	Sessions   []Session        `json:"sessions,omitempty"`
	// GuestError explains why the guest could not be asked, if it could not.
	GuestError string `json:"guest_error,omitempty"`
}

type statusSpec struct {
	Image      string `json:"image,omitempty"`
	ImageURL   string `json:"image_url,omitempty"`
	SHA256     string `json:"sha256,omitempty"`
	Arch       string `json:"arch"`
	CPUs       int    `json:"cpus"`
	MemoryMiB  int    `json:"memory_mib"`
	DiskGiB    int    `json:"disk_gib"`
	TaskMemMiB int    `json:"task_memory_mib"`
	TaskCPUMHz int    `json:"task_cpu_mhz"`
	// Executors lists the executors this box was given files for, and
	// ExecutorFiles the files themselves.
	Executors     []string `json:"executors,omitempty"`
	ExecutorFiles []string `json:"executor_files,omitempty"`
}

type statusAlloc struct {
	ID            string `json:"id"`
	NodeID        string `json:"node_id"`
	NodeName      string `json:"node_name"`
	ClientStatus  string `json:"client_status"`
	DesiredStatus string `json:"desired_status"`
	TaskState     string `json:"task_state,omitempty"`
	StartedAt     string `json:"started_at,omitempty"`
	Restarts      uint64 `json:"restarts"`
	Description   string `json:"description,omitempty"`
}

type statusVolume struct {
	ID            string `json:"id,omitempty"`
	Name          string `json:"name"`
	PluginID      string `json:"plugin_id,omitempty"`
	NodeID        string `json:"node_id,omitempty"`
	State         string `json:"state,omitempty"`
	CapacityBytes int64  `json:"capacity_bytes,omitempty"`
	HostPath      string `json:"host_path,omitempty"`
}

// cmdStatus shows one box: its job, its allocation and recent task events, the
// guest's own report and its sessions.
func (a *App) cmdStatus(ctx context.Context, args []string) error {
	fs := newFlagSet("status")
	events := fs.Int("events", 10, "how many recent task events to show")
	if err := parseFlags(fs, args, 1); err != nil {
		return err
	}
	id := fs.Arg(0)
	box, job, err := a.findBox(ctx, id)
	if err != nil {
		return err
	}
	spec, specErr := SpecFromJob(job)
	if specErr != nil {
		return specErr
	}
	out := &statusOutput{
		Box: box,
		Spec: &statusSpec{
			Image:      spec.ImageVersion,
			ImageURL:   spec.ImageURL,
			SHA256:     spec.ImageSHA256,
			Arch:       spec.Arch,
			CPUs:       spec.CPUs,
			MemoryMiB:  spec.MemoryMiB,
			DiskGiB:    spec.DiskGiB,
			TaskMemMiB: spec.TaskMemoryMiB(),
			TaskCPUMHz: spec.TaskCPUShares(),
		},
	}
	if keys := executorKeysFromJob(job); len(keys) > 0 {
		items := make(map[string]string, len(keys))
		for _, key := range keys {
			items[key] = ""
		}
		out.Spec.ExecutorFiles = keys
		out.Spec.Executors = executorsOf(executorFilesOf(items))
	}

	if volume := a.volumeFor(ctx, id); volume != nil {
		out.Volume = volume
	}
	// What the disk really occupies, as the box driver reports it on the node
	// that holds it: the volume's node is the one that counts, also when the box
	// is stopped and has no allocation.
	nodeID := box.NodeID
	if nodeID == "" && out.Volume != nil {
		nodeID = out.Volume.NodeID
	}
	if byBox, err := a.nodeDisks(ctx, map[string]map[string]BoxDisk{}, nodeID); err == nil {
		if disk, ok := byBox[box.ID]; ok {
			out.Box.DiskUsedBytes = disk.UsedBytes
			out.Box.DiskVirtualBytes = disk.VirtualBytes
		}
	}

	var alloc *api.Allocation
	if box.AllocationID != "" {
		alloc, err = a.Nomad.Allocation(ctx, box.AllocationID)
		if err != nil {
			return wrapf(err, "cannot read allocation %s", box.AllocationID)
		}
		out.Allocation = &statusAlloc{
			ID:            alloc.ID,
			NodeID:        alloc.NodeID,
			NodeName:      alloc.NodeName,
			ClientStatus:  alloc.ClientStatus,
			DesiredStatus: alloc.DesiredStatus,
			Description:   oneLine(alloc.ClientDescription, 200),
		}
		if task := alloc.TaskStates[boxTaskName]; task != nil {
			out.Allocation.TaskState = task.State
			out.Allocation.Restarts = task.Restarts
			if !task.StartedAt.IsZero() {
				out.Allocation.StartedAt = task.StartedAt.UTC().Format("2006-01-02T15:04:05Z")
			}
		}
		out.TaskEvents = taskEvents(alloc, *events)
	}

	// The guest is asked last: a box that is not running still has a status.
	if box.State == "running" {
		if client, err := a.boxctl(ctx, box); err != nil {
			out.GuestError = err.Error()
		} else {
			if info, err := client.Info(ctx); err != nil {
				out.GuestError = "the guest did not answer `boxctl info`: " + err.Error()
			} else {
				out.Guest = info
			}
			if sessions, err := client.SessionList(ctx); err == nil {
				out.Sessions = sessions
			} else if out.GuestError == "" {
				out.GuestError = "the guest did not answer `boxctl session ls`: " + err.Error()
			}
		}
	} else {
		out.GuestError = fmt.Sprintf("the box is %s, so the guest was not asked", box.State)
	}

	if a.JSON {
		return a.printJSON(out)
	}
	a.printStatus(out)
	return nil
}

func (a *App) printStatus(out *statusOutput) {
	box := out.Box
	a.note("box %s (job %s)", box.ID, box.Job)
	a.note("  state   %s", box.State)
	if box.Node != "" {
		a.note("  node    %s", box.Node)
	}
	if out.Allocation != nil {
		a.note("  alloc   %s  %s/%s  restarts %d", out.Allocation.ID, out.Allocation.ClientStatus,
			dash(out.Allocation.TaskState), out.Allocation.Restarts)
		if out.Allocation.StartedAt != "" {
			a.note("  started %s", out.Allocation.StartedAt)
		}
		if out.Allocation.Description != "" {
			a.note("  nomad   %s", out.Allocation.Description)
		}
	}
	if out.Box.Note != "" {
		a.note("  note    %s", out.Box.Note)
	}
	a.note("  spec    %s, image %s (%s), task reserves %d MiB / %d MHz",
		describeSpec(BoxSpec{CPUs: out.Spec.CPUs, MemoryMiB: out.Spec.MemoryMiB, DiskGiB: out.Spec.DiskGiB,
			ImageVersion: out.Spec.Image, Arch: out.Spec.Arch}),
		out.Spec.Image, out.Spec.Arch, out.Spec.TaskMemMiB, out.Spec.TaskCPUMHz)
	if out.Volume != nil {
		a.note("  disk    volume %s (%s, %s) at %s", out.Volume.Name, dash(out.Volume.PluginID),
			humanBytes(out.Volume.CapacityBytes), dash(out.Volume.HostPath))
	}
	if out.Box.DiskUsedBytes > 0 || out.Box.DiskVirtualBytes > 0 {
		a.note("  usage   %s used of %s (the rest is free inside the box)", humanBytes(out.Box.DiskUsedBytes),
			humanBytes(out.Box.DiskVirtualBytes))
	}
	if len(out.Spec.Executors) > 0 {
		a.note("  exec    %s (%d file(s), from %s)", strings.Join(out.Spec.Executors, ", "),
			len(out.Spec.ExecutorFiles), boxExecutorsVariable)
	} else {
		a.note("  exec    no executor credentials (box executors set --from-dir DIR --provider NAME)")
	}
	if len(out.TaskEvents) > 0 {
		a.note("  recent task events:")
		for _, event := range out.TaskEvents {
			message := oneLine(event.DisplayMessage, 160)
			if details := oneLine(event.Details["error"], 160); details != "" {
				message = details
			}
			a.note("    %s  %-22s %s", eventTime(event.Time), event.Type, message)
		}
	}
	if out.Guest != nil {
		a.note("  guest   boxd %s, %d vCPU, %d/%d MiB memory free, %d session(s)",
			dash(out.Guest.BoxdVersion), out.Guest.CPUs, out.Guest.MemFreeMiB, out.Guest.MemTotalMiB, out.Guest.Sessions)
		var available, unconfigured []string
		for _, executor := range out.Guest.Executors {
			if !executor.Available {
				continue
			}
			if executor.Configured != nil && !*executor.Configured {
				unconfigured = append(unconfigured, executor.Name)
				continue
			}
			available = append(available, fmt.Sprintf("%s %s", executor.Name, executor.Version))
		}
		if len(available) > 0 {
			a.note("  agents  %s", strings.Join(available, ", "))
		}
		if len(unconfigured) > 0 {
			a.note("  unconfigured %s (no credentials in /run/box/secrets: box executors set --from-dir DIR --provider NAME)",
				strings.Join(unconfigured, ", "))
		}
	} else if out.GuestError != "" {
		a.note("  guest   %s", out.GuestError)
	}
	if len(out.Sessions) > 0 {
		a.note("  sessions:")
		for _, session := range out.Sessions {
			a.note("    %s  %-8s %-24s %-8s turn %d  %s", session.ID, session.Executor, session.Model, session.State,
				session.LastTurn, oneLine(session.Title, 60))
		}
	}
}

// volumeFor returns the box disk volume, if it still exists.
func (a *App) volumeFor(ctx context.Context, id string) *statusVolume {
	volumes, err := a.Nomad.HostVolumes(ctx)
	if err != nil {
		return nil
	}
	for _, stub := range volumes {
		if stub == nil || stub.Name != BoxVolumeName(id) {
			continue
		}
		out := &statusVolume{
			ID:            stub.ID,
			Name:          stub.Name,
			PluginID:      stub.PluginID,
			NodeID:        stub.NodeID,
			State:         string(stub.State),
			CapacityBytes: stub.CapacityBytes,
		}
		if volume, err := a.Nomad.HostVolume(ctx, stub.ID); err == nil && volume != nil {
			out.HostPath = volume.HostPath
			out.CapacityBytes = volume.CapacityBytes
		}
		return out
	}
	return nil
}
