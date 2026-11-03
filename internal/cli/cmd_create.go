package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hashicorp/nomad/api"
)

// createOutput is the JSON document of `box create`.
type createOutput struct {
	Box    BoxRecord      `json:"box"`
	Job    string         `json:"job"`
	Volume string         `json:"volume"`
	Image  *ImageArtifact `json:"image"`
	Node   string         `json:"node"`
	Warn   string         `json:"warning,omitempty"`
}

// cmdCreate creates a box: a dynamic host volume holding the disk plus the job
// that runs the VM on it.
func (a *App) cmdCreate(ctx context.Context, args []string) error {
	fs := newFlagSet("create")
	imageVersion := fs.String("image", "", "base image version from the index (required)")
	arch := fs.String("arch", "", "architecture: amd64 or arm64 (default: the node's)")
	cpus := fs.Int("cpus", 2, "number of guest vCPUs")
	memoryMiB := fs.Int("memory-mib", 1024, "guest RAM in MiB")
	diskGiB := fs.Int("disk-gib", 8, "box disk size in GiB (on a dynamic host volume)")
	nodeName := fs.String("node", "", "pin the box to this node")
	vaultRole := fs.String("vault-role", a.VaultRole(), "the Vault role the box reads its secrets with "+
		"(default: $BOX_VAULT_ROLE, else the cluster's default role)")
	timeout := fs.Duration("timeout", defaultWaitTimeout, "how long to wait for the box to run")
	wait := fs.Bool("wait", true, "wait until the box is running")
	if err := parseFlags(fs, args, 1); err != nil {
		return err
	}
	id := fs.Arg(0)
	if err := ValidateBoxID(id); err != nil {
		return err
	}
	if strings.TrimSpace(*imageVersion) == "" {
		return usagef("box create %s: --image is required; `box images` lists the published versions", id)
	}
	if *timeout <= 0 {
		return usagef("--timeout must be positive, got %s", *timeout)
	}
	for name, value := range map[string]int{"cpus": *cpus, "memory-mib": *memoryMiB, "disk-gib": *diskGiB} {
		if err := requirePositive(name, value); err != nil {
			return err
		}
	}

	// A box that already exists is never replaced: its disk is the work.
	if existing, _, err := a.findBox(ctx, id); err == nil {
		return errorf("a box %q already exists (it is %s); `box ls` lists the boxes, `box rm %s --yes` deletes it",
			id, existing.State, id)
	} else if e, ok := err.(*Error); !ok || e.Code != ExitNotFound {
		return err
	}

	indexURL, err := a.requireImageIndexURL()
	if err != nil {
		return err
	}
	index, err := LoadImageIndex(ctx, a.Images, indexURL)
	if err != nil {
		if errors.Is(err, ErrIndexMissing) {
			return errorf("there are no base images yet: the image index %s answers 404. "+
				"Publish an image, or point BOX_IMAGE_INDEX (or --image-index) at an index that exists", indexURL)
		}
		return err
	}
	nodes, err := a.nodeCapacities(ctx)
	if err != nil {
		return err
	}

	wantArch := NormalizeArch(*arch)
	requested := PlacementRequest{Arch: wantArch, CPUs: *cpus, MemoryMiB: *memoryMiB, DiskGiB: *diskGiB, Node: *nodeName}

	// Pick the architecture and the node together: the node decides which
	// architecture is possible, the index decides which one has an image.
	candidateArches := ImageArchitectures
	if wantArch != "" {
		if wantArch != "amd64" && wantArch != "arm64" {
			return usagef("--arch must be amd64 or arm64, got %q", *arch)
		}
		candidateArches = []string{wantArch}
	}
	var (
		node    *NodeCapacity
		image   *ImageArtifact
		reasons []string
		codes   []int
	)
	for _, candidate := range candidateArches {
		artifact, err := index.Resolve(*imageVersion, candidate)
		if err != nil {
			reasons = append(reasons, err.Error())
			codes = append(codes, ExitCode(err))
			continue
		}
		if err := artifact.Validate(); err != nil {
			reasons = append(reasons, err.Error())
			codes = append(codes, ExitCode(err))
			continue
		}
		requested.Arch = candidate
		chosen, _, err := ChooseNode(nodes, requested)
		if err != nil {
			reasons = append(reasons, err.Error())
			codes = append(codes, ExitCode(err))
			continue
		}
		node, image = chosen, artifact
		break
	}
	if node == nil || image == nil {
		// Keep the most specific exit code: "no such image version" is a
		// not-found, a cluster without room is an error.
		code := ExitError
		if len(codes) > 0 {
			code = codes[0]
			for _, other := range codes {
				if other != code {
					code = ExitError
					break
				}
			}
		}
		return &Error{Code: code, Msg: strings.Join(reasons, "; ")}
	}

	spec := BoxSpec{
		ID:           id,
		ImageVersion: image.Version,
		ImageURL:     image.URL,
		ImageSHA256:  image.SHA256,
		Arch:         image.Arch,
		CPUs:         *cpus,
		MemoryMiB:    *memoryMiB,
		DiskGiB:      *diskGiB,
		NodeID:       node.NodeID,
	}
	spec.ExecutorItems = a.executorItems(ctx)
	spec.VaultRole = strings.TrimSpace(*vaultRole)
	job, err := RenderJob(spec)
	if err != nil {
		return err
	}
	volume, err := RenderVolume(VolumeSpec{BoxID: id, NodeID: node.NodeID, DiskGiB: *diskGiB})
	if err != nil {
		return err
	}

	created, err := a.Nomad.CreateHostVolume(ctx, volume)
	if err != nil {
		return wrapf(err, "Nomad could not create the box disk %s on node %s",
			BoxVolumeName(id), node.Name)
	}
	repo := &createOutput{
		Job:    BoxJobName(id),
		Volume: created.ID,
		Image:  image,
		Node:   node.Name,
	}

	resp, err := a.Nomad.RegisterJob(ctx, job)
	lost := ""
	if err != nil {
		// An error does not prove the server did not take the job: the response
		// can be lost after the job was written. Ask Nomad what it holds before
		// anything is deleted -- otherwise a running box loses its disk.
		probe := a.probeBoxJob(ctx, id)
		switch {
		case !probe.Known:
			return wrapKeep(err, "Nomad did not say whether the job %s was registered (%s), so nothing was deleted: "+
				"the box may well be running (`box status %s` shows it, `box rm %s --yes` removes it)",
				BoxJobName(id), oneLine(errText(probe.Err), 200), id, id)
		case !probe.Found:
			// The job is definitively gone: the disk has no box left to serve.
			a.discardVolume(ctx, created)
			return wrapf(err, "Nomad rejected the box job %s", BoxJobName(id))
		case !probe.IsBox:
			return errorf("the job %s exists but is not a box (it has no meta.box = \"true\"), so the disk %s was kept: "+
				"`nomad job status %s` shows what job that name belongs to", BoxJobName(id), created.ID, BoxJobName(id))
		}
		// The job exists: the registration did take, so the box and its disk
		// stay. Report what Nomad says instead of pretending the create failed.
		resp = nil
		lost = fmt.Sprintf("registered, state %s: Nomad answered %q, but the job %s exists, so the box and the disk %s were kept",
			probe.State, oneLine(err.Error(), 160), BoxJobName(id), created.ID)
	}
	evalID := ""
	if resp != nil {
		evalID = resp.EvalID
	}

	if !*wait {
		rec, _, err := a.findBox(ctx, id)
		if err != nil {
			return err
		}
		repo.Box = rec
		repo.Warn = joinNotes(lost, "the box was created and is starting; `box status "+id+"` shows when it is ready")
		return a.printCreate(repo)
	}

	rec, err := a.waitForBoxRunning(ctx, id, evalID, *timeout)
	if err != nil {
		// A box that never got an allocation is taken away again: there is
		// nothing to look at and nothing to keep. A box that did start and then
		// failed is kept, because its task events and its log are the evidence.
		if rec.AllocationID == "" {
			if cleanupErr := a.discardBox(ctx, id, created); cleanupErr != nil {
				return &Error{Code: ExitCode(err), Msg: err.Error() + "; " + cleanupErr.Error() +
					"; `box rm " + id + " --yes` removes it"}
			}
			return err
		}
		return wrapKeep(err, "the box %s was kept so you can look at it (`box status %s`, `box logs %s`); `box rm %s --yes` removes it",
			id, id, id, id)
	}
	repo.Box = rec
	repo.Warn = lost
	if resp != nil && resp.Warnings != "" {
		repo.Warn = joinNotes(lost, oneLine(resp.Warnings, 300))
	}
	return a.printCreate(repo)
}

// printCreate writes the result of `box create`.
func (a *App) printCreate(out *createOutput) error {
	if a.JSON {
		return a.printJSON(out)
	}
	box := out.Box
	a.note("created box %s (job %s) on node %s", box.ID, out.Job, out.Node)
	a.note("  image  %s (%s) sha256 %s", out.Image.Version, out.Image.Arch, shortSHA(out.Image.SHA256))
	a.note("  size   %d vCPU, %d MiB RAM, %d GiB disk", box.CPUs, box.MemoryMiB, box.DiskGiB)
	a.note("  state  %s%s", box.State, allocationSuffix(box))
	if out.Warn != "" {
		a.note("  note   %s", out.Warn)
	}
	if box.State == "running" {
		a.note("next: box session new %s --executor prime --model <provider>/<model>", box.ID)
	}
	return nil
}

func allocationSuffix(box BoxRecord) string {
	if box.AllocationID == "" {
		return ""
	}
	return " (allocation " + shortID(box.AllocationID) + ")"
}

// discardVolume deletes a volume that was created but never used.
func (a *App) discardVolume(ctx context.Context, volume *api.HostVolume) {
	if volume == nil || volume.ID == "" {
		return
	}
	_ = a.Nomad.DeleteHostVolume(ctx, volume.ID, true)
}

// discardBox removes a box whose creation failed before it ever ran.
//
// It deletes the job first and the disk only once Nomad says the job is gone:
// a disk deleted under a job that still exists is a box that can never start
// again. The returned error explains what was left behind and why.
func (a *App) discardBox(ctx context.Context, id string, volume *api.HostVolume) error {
	if err := a.Nomad.StopJob(ctx, BoxJobName(id), true); err != nil && !isNotFoundErr(err) {
		return wrapf(err, "Nomad could not delete the job %s, so the disk %s was kept", BoxJobName(id), volumeNameOf(volume, id))
	}
	probe := a.probeBoxJob(ctx, id)
	switch {
	case !probe.Known:
		return wrapf(probe.Err, "Nomad did not say whether the job %s is gone, so the disk %s was kept",
			BoxJobName(id), volumeNameOf(volume, id))
	case probe.Found:
		return errorf("the job %s is still there (state %s, %d live allocation(s)), so the disk %s was kept",
			BoxJobName(id), probe.State, probe.Live, volumeNameOf(volume, id))
	}
	if volume != nil && volume.ID != "" {
		if err := a.Nomad.DeleteHostVolume(ctx, volume.ID, true); err != nil && !isNotFoundErr(err) {
			return wrapf(err, "the disk %s on node %s could not be deleted", volume.Name, volume.NodeID)
		}
		return nil
	}
	// Fall back to the name, in case the volume record was not kept.
	vols, err := a.Nomad.HostVolumes(ctx)
	if err != nil {
		return wrapf(err, "cannot list the volumes, so the disk of %s was kept", BoxJobName(id))
	}
	for _, stub := range vols {
		if stub == nil || stub.Name != BoxVolumeName(id) {
			continue
		}
		if err := a.Nomad.DeleteHostVolume(ctx, stub.ID, true); err != nil && !isNotFoundErr(err) {
			return wrapf(err, "Nomad could not delete the volume %s on node %s", stub.Name, stub.NodeID)
		}
	}
	return nil
}

// volumeNameOf names the disk of a box, with or without the volume record.
func volumeNameOf(volume *api.HostVolume, id string) string {
	if volume != nil && volume.Name != "" {
		return volume.Name
	}
	return BoxVolumeName(id)
}

// boxJobPresence is what Nomad reports about a box job after a step failed.
//
// Known is false when Nomad could not be asked at all: nothing may be deleted
// then, because "no answer" is not "no job".
type boxJobPresence struct {
	Known bool
	Found bool
	// IsBox is whether the job carries meta.box = "true".
	IsBox bool
	// State is the box state to report, "" when the job does not exist.
	State string
	// Live is the number of allocations that are not terminal.
	Live int
	// Rec is the box record, filled in when the job exists.
	Rec BoxRecord
	// Err is why Nomad could not be asked.
	Err error
}

// probeBoxJob reads the current state of a box job from Nomad and changes
// nothing. It is the reconciliation every cleanup path goes through.
func (a *App) probeBoxJob(ctx context.Context, id string) boxJobPresence {
	job, err := a.Nomad.Job(ctx, BoxJobName(id))
	if err != nil {
		if isNotFoundErr(err) {
			return boxJobPresence{Known: true}
		}
		return boxJobPresence{Err: err}
	}
	probe := boxJobPresence{Known: true, Found: true, IsBox: IsBoxJob(job)}
	byJob, err := a.jobAllocations(ctx)
	if err != nil {
		// The job is there; only its allocations are unknown.
		probe.State = "unknown"
		if probe.IsBox {
			probe.Rec = boxRecord(job, nil, a.now())
			probe.Rec.State = probe.State
		}
		return probe
	}
	allocs := byJob[BoxJobName(id)]
	for _, stub := range allocs {
		if stub != nil && !isTerminalAlloc(stub.ClientStatus) {
			probe.Live++
		}
	}
	if !probe.IsBox {
		probe.State = "not a box"
		return probe
	}
	probe.Rec = boxRecord(job, allocs, a.now())
	probe.State = probe.Rec.State
	if probe.State == "stopped" && len(allocs) == 0 && (job.Stop == nil || !*job.Stop) {
		// The job is registered and Nomad has not placed an allocation yet.
		probe.State = "starting"
	}
	return probe
}

// errText is the message of an error, empty for nil, so it can go into a
// format string without a nil check.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
