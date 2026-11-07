package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

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
	// VMNote says qemu has paused this box's VM, and why that is what a silent
	// guest means here; NodeNote says the node itself is low on disk, even when
	// this box is healthy.
	VMNote   string `json:"vm_note,omitempty"`
	NodeNote string `json:"node_note,omitempty"`
	// DiskTerm says when the disk of a box that is not running is deleted. It is
	// absent for a box that runs, which is never on a term.
	DiskTerm *statusDiskTerm `json:"disk_term,omitempty"`
}

type statusSpec struct {
	Image    string `json:"image,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
	SHA256   string `json:"sha256,omitempty"`
	// Platform is the version of the platform bundle the box mounts, empty for
	// an image that runs the boxd it carries.
	Platform   string `json:"platform,omitempty"`
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

// statusDiskTerm is the JSON field of `box status` that says when the disk of a
// box that is not running is deleted.
type statusDiskTerm struct {
	StoppedAt string `json:"stopped_at,omitempty"`
	RemovesAt string `json:"removes_at,omitempty"`
	// Note is the sentence a person reads: how long is left, or that no pass has
	// seen the box not running yet.
	Note string `json:"note,omitempty"`
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
	out := &statusOutput{Box: box}
	// An orphan has no job and no description: there is no spec to show, and the
	// disk is all there is to look at.
	if job != nil {
		spec, specErr := SpecFromJob(job)
		if specErr != nil {
			return specErr
		}
		out.Spec = &statusSpec{
			Image:      spec.ImageVersion,
			ImageURL:   spec.ImageURL,
			SHA256:     spec.ImageSHA256,
			Platform:   spec.PlatformVersion,
			Arch:       spec.Arch,
			CPUs:       spec.CPUs,
			MemoryMiB:  spec.MemoryMiB,
			DiskGiB:    spec.DiskGiB,
			TaskMemMiB: spec.TaskMemoryMiB(),
			TaskCPUMHz: spec.TaskCPUShares(),
		}
		if keys := executorKeysFromJob(job); len(keys) > 0 {
			items := make(map[string]string, len(keys))
			for _, key := range keys {
				items[key] = ""
			}
			out.Spec.ExecutorFiles = keys
			out.Spec.Executors = executorsOf(executorFilesOf(items))
		}
	}

	if volume := a.volumeFor(ctx, id); volume != nil {
		out.Volume = volume
	}
	out.DiskTerm = a.diskTerm(ctx, box, a.now())
	// What the disk really occupies, as the box driver reports it on the node
	// that holds it: the volume's node is the one that counts, also when the box
	// is stopped and has no allocation.
	nodeID := box.NodeID
	if nodeID == "" && out.Volume != nil {
		nodeID = out.Volume.NodeID
	}
	// One read of the box's node is what the disk's size, qemu's state and the
	// node's own room all come from.
	var capacity *NodeCapacity
	if nodeID != "" {
		if node, err := a.Nomad.NodeInfo(ctx, nodeID); err == nil {
			capacity = BuildCapacity(node, nil)
		}
	}
	if capacity != nil {
		for _, disk := range capacity.Disks {
			if disk.BoxID == box.ID {
				out.Box.DiskUsedBytes = disk.UsedBytes
				out.Box.DiskVirtualBytes = disk.VirtualBytes
			}
		}
		if inst, ok := capacity.instanceState(box.ID); ok {
			out.Box.VMState = inst.QEMUState
			out.VMNote = vmStateSentence(box.ID, inst.QEMUState, capacity.DiskFreeBytes, capacity.DiskFreeKnown)
		}
		if capacity.LowDisk() {
			out.NodeNote = capacity.lowDiskSentence()
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

	// The guest is asked last, and with a bound: a box that is not running still
	// has a status, and a guest that does not answer is a fact about the guest,
	// not a reason to print nothing.
	var silent error
	if box.State == "running" {
		if client, err := a.boxctl(ctx, box); err != nil {
			out.GuestError = err.Error()
		} else {
			err := a.guestCall(ctx, "`boxctl info`", func(callCtx context.Context) error {
				info, err := client.Info(callCtx)
				if err != nil {
					return err
				}
				out.Guest = info
				return nil
			})
			switch {
			case err == nil:
			case isGuestSilent(err):
				silent = err
				out.GuestError = err.Error()
			default:
				out.GuestError = "the guest did not answer `boxctl info`: " + err.Error()
			}
			// A guest that did not answer the first call does not answer the
			// second either, and two bounds would be forty seconds of status.
			if silent == nil {
				err = a.guestCall(ctx, "`boxctl session ls`", func(callCtx context.Context) error {
					sessions, err := client.SessionList(callCtx)
					if err != nil {
						return err
					}
					out.Sessions = sessions
					return nil
				})
				switch {
				case err == nil:
				case isGuestSilent(err):
					silent = err
					out.GuestError = err.Error()
				case out.GuestError == "":
					out.GuestError = "the guest did not answer `boxctl session ls`: " + err.Error()
				}
			}
		}
	} else {
		out.GuestError = fmt.Sprintf("the box is %s, so the guest was not asked", box.State)
	}

	if a.JSON {
		if err := a.printJSON(out); err != nil {
			return err
		}
	} else {
		a.printStatus(out)
	}
	return statusSilentError(id, silent, out)
}

// statusSilentError is the exit code of a status whose guest did not answer:
// none when everything Nomad knows was printed (which is what a status is for),
// and a timeout when the silence was the whole of what the status had to say.
func statusSilentError(id string, silent error, out *statusOutput) error {
	if silent == nil || out.showedNomadFacts() {
		return nil
	}
	return timeoutf("box %s: %v, and Nomad knows nothing else about it", id, silent)
}

// showedNomadFacts reports whether a status showed anything about the box other
// than its job: its allocation, its volume or its task events.
func (out *statusOutput) showedNomadFacts() bool {
	return out.Allocation != nil || out.Volume != nil || len(out.TaskEvents) > 0
}

func (a *App) printStatus(out *statusOutput) {
	box := out.Box
	a.note("box %s (job %s)", box.ID, box.Job)
	// A VM qemu paused is the first thing a status says: Nomad's state below is
	// still "running", and the two together are the whole story.
	if out.VMNote != "" {
		a.note("  PAUSED  %s", out.VMNote)
	}
	if out.NodeNote != "" {
		a.note("  node    %s", out.NodeNote)
	}
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
	if out.DiskTerm != nil {
		a.note("  disk    %s", out.DiskTerm.Note)
	}
	if out.Spec != nil {
		a.note("  spec    %s, task reserves %d MiB / %d MHz",
			describeSpec(BoxSpec{CPUs: out.Spec.CPUs, MemoryMiB: out.Spec.MemoryMiB, DiskGiB: out.Spec.DiskGiB,
				ImageVersion: out.Spec.Image, Arch: out.Spec.Arch}),
			out.Spec.TaskMemMiB, out.Spec.TaskCPUMHz)
	}
	if out.Spec != nil && out.Spec.Platform != "" {
		a.note("  platform %s (read-only mount at /opt/box)", out.Spec.Platform)
	}
	if out.Volume != nil {
		a.note("  volume  %s (%s) at %s", out.Volume.Name, dash(out.Volume.PluginID), dash(out.Volume.HostPath))
	}
	// The disk's real size comes from the driver, which reads it out of the disk
	// file. Nomad's volume record keeps the capacity the plugin reported when the
	// volume was created: a dynamic host volume is never resized through the
	// API, so a `box resize --disk-gib` cannot move that number.
	switch {
	case out.Box.DiskVirtualBytes > 0:
		a.note("  disk    %s%s", humanBytes(out.Box.DiskVirtualBytes), volumeRecordNote(out))
	case out.Volume != nil && out.Volume.CapacityBytes > 0:
		a.note("  disk    %s, as Nomad records it for the volume", humanBytes(out.Volume.CapacityBytes))
	}
	if out.Box.DiskUsedBytes > 0 || out.Box.DiskVirtualBytes > 0 {
		a.note("  usage   %s used of %s (the rest is free inside the box)", humanBytes(out.Box.DiskUsedBytes),
			humanBytes(out.Box.DiskVirtualBytes))
	}
	if out.Spec == nil {
		a.note("  spec    nothing describes this disk: no job and no stored description")
	} else if len(out.Spec.Executors) > 0 {
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
		if u := out.Guest.Usage; u != nil {
			a.note("  usage   %s", u.Line())
			// Work running with no turn in progress is why a box is busy while
			// its sessions are idle; the count and its lines are boxd's.
			if line := u.Leftover.Summary(); line != "" {
				a.note("  leftover %s", line)
			}
		}
		if p := out.Guest.Prepare; p != nil && p.State != "none" {
			switch p.State {
			case "preparing":
				a.note("  prepare preparing: %s (`box prepare %s` waits, --cancel stops it)", prepareDoing(p), out.Box.ID)
			case "failed":
				a.note("  prepare failed in the %s step: %s (`box prepare %s` runs it again, --skip goes without)",
					dash(p.Step), p.Error, out.Box.ID)
			default:
				a.note("  prepare %s%s", p.State, prepareWhere(p))
			}
		}
	} else if out.GuestError != "" {
		a.note("  guest   %s", out.GuestError)
	}
	if len(out.Sessions) > 0 {
		a.note("  sessions:")
		for _, session := range out.Sessions {
			a.note("    %s  %-8s %-24s %-8s turn %d  %s", session.ID, session.Executor, session.Model, session.stateLabel(),
				session.LastTurn, oneLine(session.Title, 60))
		}
	}
}

// diskTerm says when the disk of a box that is not running is deleted, and for a
// box that runs says nothing: a running box is never touched, however long it
// idles.
func (a *App) diskTerm(ctx context.Context, box BoxRecord, now time.Time) *statusDiskTerm {
	switch box.State {
	case "running", "starting", "unknown", "deleted", "gone":
		return nil
	}
	clock, seen, err := a.readBoxClock(ctx, box.ID)
	if err != nil {
		return &statusDiskTerm{Note: "the clock of this box cannot be read, so no pass will delete its disk: " +
			oneLine(err.Error(), 200)}
	}
	// The term is a cluster setting: when it cannot be read, this command says the
	// time is unknown and why, and still shows everything else about the box.
	term := a.readBoxTerm(ctx)
	if !seen {
		if !term.known() {
			return &statusDiskTerm{Note: "no reap pass has seen this box stopped yet; its disk is deleted after the " +
				"cluster's stopped-box term, which cannot be read now: " + term.why}
		}
		return &statusDiskTerm{Note: "no reap pass has seen this box stopped yet; its disk is deleted " +
			termWords(term.term) + " after the first pass that sees it (`box reap` runs every 15 minutes)"}
	}
	if !term.known() {
		return &statusDiskTerm{StoppedAt: clock.at.UTC().Format(time.RFC3339),
			Note: "its disk is deleted after the cluster's stopped-box term, which cannot be read now, so this " +
				"command cannot say when: " + term.why}
	}
	removesAt := clock.at.Add(term.term)
	left := removesAt.Sub(now)
	note := fmt.Sprintf("deleted at %s, %s after it stopped", removesAt.UTC().Format(time.RFC3339), termWords(term.term))
	switch {
	case left <= 0:
		note += "; that time has passed, and the next `box reap` deletes it (`box start " + box.ID + "` runs it again first)"
	default:
		note += fmt.Sprintf(" (in %s); `box start %s` runs it again", humanDuration(left), box.ID)
	}
	return &statusDiskTerm{
		StoppedAt: clock.at.UTC().Format(time.RFC3339),
		RemovesAt: removesAt.UTC().Format(time.RFC3339),
		Note:      note,
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

// volumeRecordNote explains, in one clause, the difference between the disk's
// real size and the capacity Nomad records for its volume.
//
// A dynamic host volume keeps the capacity its plugin reported when the volume
// was created: Nomad's register path never asks the plugin again, and the state
// store writes the CapacityBytes the request carries, so an update cannot resize
// it. A box that grew its disk with `box resize --disk-gib` therefore has a
// volume record that says the old size; the disk in it does not.
func volumeRecordNote(out *statusOutput) string {
	if out.Volume == nil || out.Volume.CapacityBytes <= 0 || out.Box.DiskVirtualBytes <= 0 {
		return ""
	}
	if out.Volume.CapacityBytes == out.Box.DiskVirtualBytes {
		return ""
	}
	return fmt.Sprintf(" (Nomad records %s for the volume: a dynamic host volume keeps the capacity its plugin "+
		"reported when it was created, and no update changes that; the disk itself is the size above)",
		humanBytes(out.Volume.CapacityBytes))
}
