package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/nomad/api"
)

// createOutput is the JSON document of `box create`.
type createOutput struct {
	Box    BoxRecord      `json:"box"`
	Job    string         `json:"job"`
	Volume string         `json:"volume"`
	Image  *ImageArtifact `json:"image"`
	// Platform is the platform bundle the box mounts, when its image takes one.
	Platform *PlatformArtifact `json:"platform,omitempty"`
	Node     string            `json:"node"`
	Warn     string            `json:"warning,omitempty"`
	// Prepare is the box's preparation, when create was given its sources.
	Prepare *PrepareState `json:"prepare,omitempty"`
}

// Defaults of `box create`, also what `box images` suggests. 4096 MiB because a
// guest that runs out of memory does not answer at all: an agent session with
// its tools, plus a build of the box's own work, filled 2048 MiB and the guest
// then stalled - no reply to boxctl, qemu inflating disk clusters for twenty
// minutes while the kernel thrashed. The extra RAM is the cheaper half of that
// failure by far.
const (
	defaultCPUs      = 2
	defaultMemoryMiB = 4096
	defaultDiskGiB   = 8
)

// cmdCreate creates a box: a dynamic host volume holding the disk plus the job
// that runs the VM on it.
func (a *App) cmdCreate(ctx context.Context, args []string) error {
	fs := newFlagSet("create")
	imageVersion := fs.String("image", "", "base image version from the index (required)")
	arch := fs.String("arch", "", "architecture: amd64 or arm64 (default: the node's)")
	cpus := fs.Int("cpus", defaultCPUs, "number of guest vCPUs")
	memoryMiB := fs.Int("memory-mib", defaultMemoryMiB, "guest RAM in MiB")
	diskGiB := fs.Int("disk-gib", defaultDiskGiB, "box disk size in GiB (on a dynamic host volume)")
	nodeName := fs.String("node", "", "pin the box to this node")
	platformVersion := fs.String("platform", "", "platform bundle version to mount "+
		"(default: the newest published for the box's architecture)")
	src := addPrepareFlags(fs)
	prepTimeout := fs.Duration("prepare-timeout", defaultPrepareTimeout, "time limit of the preparation, and how long create waits for it")
	vaultRole := fs.String("vault-role", a.VaultRole(), "the Vault role the box reads its secrets with "+
		"(default: $BOX_VAULT_ROLE, else the cluster's default role)")
	project := fs.String("project", "", "the project this box belongs to: a label `box ls` and `box capacity` show, "+
		"so an agent can tell its boxes from other projects' (default: $BOX_PROJECT, else the name of the git "+
		"repository of this directory)")
	timeout := fs.Duration("timeout", defaultWaitTimeout, "how long to wait for the box to run")
	// --wait is a flag whose value may be left out: "--wait" waits as it always
	// did, "--wait 30m" also waits that long for room when no node has any.
	wait := &optionalDuration{on: true}
	fs.Var(wait, "wait", "wait until the box is running; with a duration, also wait that long for room "+
		"when no node has it (for example --wait 30m)")
	if err := parseFlags(fs, joinOptionalValue(args, "wait", isDurationValue), 1); err != nil {
		return err
	}
	// A bare --wait waits as long as it always did; a duration is how long to keep
	// asking for room.
	waitFor := wait.d
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
	if err := src.check(); err != nil {
		return err
	}
	if src.given() && !wait.on {
		return usagef("box create %s: --repo and --from need the box running, so they do not go with --wait=false; "+
			"`box prepare %s` does it later", id, id)
	}
	if *prepTimeout <= 0 {
		return usagef("--prepare-timeout must be positive, got %s", *prepTimeout)
	}
	for name, value := range map[string]int{"cpus": *cpus, "memory-mib": *memoryMiB, "disk-gib": *diskGiB} {
		if err := requirePositive(name, value); err != nil {
			return err
		}
	}
	// The project is a label, not a protection: it is refused here only because a
	// label that does not say what the caller meant is worse than a refusal.
	label, err := a.ProjectLabel(*project, flagSet(fs, "project"))
	if err != nil {
		return err
	}

	// A box that already exists is never replaced: its disk is the work.
	if existing, _, err := a.findBox(ctx, id); err == nil {
		return errorf("a box %q already exists (it is %s); `box ls` lists the boxes, `box rm %s --yes` deletes it",
			id, existing.State, id)
	} else if e, ok := err.(*Error); !ok || e.Code != ExitNotFound {
		return err
	}

	// The index is where published images are, and the drafts are on the nodes.
	// Either may be absent: a cluster that only bakes images has no index yet.
	index, err := a.loadIndexOptional(ctx)
	if err != nil {
		return err
	}
	drafts, err := a.nodeDrafts(ctx)
	if err != nil && index == nil {
		return wrapf(err, "cannot read the project images of the nodes")
	}
	if index == nil && len(drafts) == 0 {
		// Neither a published image nor a draft: the index is what is missing,
		// and it is the one thing an operator has to set.
		if url, urlErr := a.requireImageIndexURL(); urlErr != nil {
			return urlErr
		} else {
			return errorf("there are no base images yet: the image index %s answers 404. "+
				"Publish an image, or point BOX_IMAGE_INDEX (or --image-index) at an index that exists", url)
		}
	}
	version, err := a.resolveImageVersion(*imageVersion, index, drafts)
	if err != nil {
		return err
	}
	// The placement is the CLI's normal choice each time: a box that does not fit
	// yet is asked for again from scratch, so it takes whatever the cluster has
	// room for then.
	placed, err := a.placeAndWait(ctx, placementArgs{
		ID:              id,
		Index:           index,
		Drafts:          drafts,
		Version:         version,
		Arch:            NormalizeArch(*arch),
		PlatformVersion: *platformVersion,
		NodeName:        *nodeName,
		CPUs:            *cpus,
		MemoryMiB:       *memoryMiB,
		DiskGiB:         *diskGiB,
		DiskGiven:       flagSet(fs, "disk-gib"),
		// The project is used by the hints only: a refusal for lack of room names
		// the idle boxes of the caller's own project, so it can stop one.
		Project: label,
	}, waitFor)
	if err != nil {
		return err
	}
	node, image, bundle, disk := placed.Node, placed.Image, placed.Bundle, placed.Disk
	diskGiB = &disk

	out, err := a.createBox(ctx, boxRequest{
		ID:            id,
		Image:         image,
		Platform:      bundle,
		Node:          node,
		CPUs:          *cpus,
		MemoryMiB:     *memoryMiB,
		DiskGiB:       *diskGiB,
		Project:       label,
		VaultRole:     strings.TrimSpace(*vaultRole),
		ExecutorItems: a.executorItems(ctx),
	}, wait.on, *timeout)
	if err != nil {
		return err
	}
	// A box bigger than a quarter of its node says so, and runs: the note is what
	// keeps a large box from being forgotten on a shared node.
	a.noteShare(node.ShareNote(id, out.Box.CPUs, out.Box.MemoryMiB, out.Box.DiskGiB))
	if !wait.on || !src.given() {
		return a.printCreate(out)
	}
	// The box runs; now its sources, and the box is ready when they are. A
	// preparation that fails keeps the box: it says why, and `box prepare`
	// runs it again.
	if !a.JSON {
		if err := a.printCreate(out); err != nil {
			return err
		}
	}
	opts, err := src.options(*prepTimeout)
	if err != nil {
		return err
	}
	client, _, err := a.boxctlFor(ctx, id)
	if err != nil {
		return wrapKeep(err, "the box %s runs but could not be prepared; `box prepare %s` tries again", id, id)
	}
	prepErr := a.prepareBox(ctx, client, id, opts, *prepTimeout, false)
	if a.JSON {
		if st, err := client.PrepareGet(ctx); err == nil {
			out.Prepare = st
		}
		if err := a.printJSON(out); err != nil {
			return err
		}
	}
	return prepErr
}

// boxPlacement is a decided box: the node it runs on, the image its disk is made
// from, the bundle it mounts and the disk it gets.
type boxPlacement struct {
	Node   *NodeCapacity
	Image  *ImageArtifact
	Bundle *PlatformArtifact
	Disk   int
}

// placementArgs is what one placement attempt works from: the images the cluster
// has and the request the operator wrote. Everything a box needs but the choice
// of node is in it, because a second attempt makes the same choose-it-together
// decision as the first.
type placementArgs struct {
	ID              string
	Index           *ImageIndex
	Drafts          []DraftImage
	Version         string
	Arch            string
	PlatformVersion string
	NodeName        string
	CPUs            int
	MemoryMiB       int
	DiskGiB         int
	// DiskGiven says whether --disk-gib was on the command line: an image's own
	// disk is the floor of a box's disk, and a disk asked for below it is refused.
	DiskGiven bool
	// Project is the project the box would belong to. It decides nothing about
	// the placement: a refusal for lack of room names the idle boxes of this
	// project as the caller's own.
	Project string
}

// placeBox picks where a box goes: the architecture, the image and the node are
// decided together, because each of them rules the others out.
func (a *App) placeBox(ctx context.Context, args placementArgs) (*boxPlacement, error) {
	nodes, err := a.nodeCapacities(ctx)
	if err != nil {
		return nil, err
	}
	requested := PlacementRequest{Arch: args.Arch, CPUs: args.CPUs, MemoryMiB: args.MemoryMiB,
		DiskGiB: args.DiskGiB, Node: args.NodeName}

	// Pick the architecture and the node together: the node decides which
	// architecture is possible, the images decide which one there is.
	candidateArches := ImageArchitectures
	if args.Arch != "" {
		if args.Arch != "amd64" && args.Arch != "arm64" {
			return nil, usagef("--arch must be amd64 or arm64, got %q", args.Arch)
		}
		candidateArches = []string{args.Arch}
	}
	var (
		node    *NodeCapacity
		image   *ImageArtifact
		bundle  *PlatformArtifact
		reasons []string
		codes   []int
		// sawRoom says a candidate was refused for want of room, and blocked says
		// one was refused for what waiting cannot change: an image that is not
		// there, a bundle nothing publishes, a node without the driver.
		sawRoom bool
		blocked bool
	)
	disk := args.DiskGiB
	for _, candidate := range candidateArches {
		artifact, err := a.resolveImage(args.Index, args.Drafts, args.Version, candidate)
		if err != nil {
			// A usage error is about the command line, not about one candidate
			// architecture: it stands whatever the other candidates say.
			if ExitCode(err) == ExitUsage {
				return nil, err
			}
			reasons = append(reasons, err.Error())
			codes = append(codes, ExitCode(err))
			blocked = true
			continue
		}
		if err := artifact.Validate(); err != nil {
			reasons = append(reasons, err.Error())
			codes = append(codes, ExitCode(err))
			blocked = true
			continue
		}
		// A project image's disk is the floor of the box's disk, and a box made
		// from a draft runs on the node that holds it.
		candidateDisk, err := diskForImage(artifact, disk, args.DiskGiven)
		if err != nil {
			if ExitCode(err) == ExitUsage {
				return nil, err
			}
			reasons = append(reasons, err.Error())
			codes = append(codes, ExitCode(err))
			blocked = true
			continue
		}
		// The image's own entry decides whether a bundle is mounted: an image
		// that carries its own boxd keeps running it, and its /opt/box is not
		// hidden by a mount. The bundle belongs to the candidate that is chosen,
		// so it is reset with each candidate: an arm64 image that carries its own
		// boxd must not inherit the amd64 candidate's bundle.
		var candidateBundle *PlatformArtifact
		if artifact.Platform != "mount" {
			if want := strings.TrimSpace(args.PlatformVersion); want != "" {
				return nil, usagef("box create %s: the image %s does not take a platform bundle (its entry has no "+
					"\"platform\": \"mount\"), so --platform does not apply: that image runs the boxd it carries",
					args.ID, artifact.Version)
			}
		} else {
			if args.Index == nil || len(args.Index.Platforms) == 0 {
				return nil, errorf("the image %s needs a platform bundle, but the image index %s lists none yet",
					artifact.Version, a.ImageIndexURL())
			}
			want := strings.TrimSpace(args.PlatformVersion)
			if want == "" {
				want = "latest"
			}
			resolved, err := args.Index.ResolvePlatform(want, candidate)
			if err != nil {
				if want == "latest" && ExitCode(err) == ExitNotFound {
					err = notFoundf("the image %s needs a platform bundle and none is published for architecture %s; published: %s",
						artifact.Version, candidate, strings.Join(args.Index.PlatformVersions(), ", "))
				}
				reasons = append(reasons, err.Error())
				codes = append(codes, ExitCode(err))
				blocked = true
				continue
			}
			if err := resolved.Validate(); err != nil {
				reasons = append(reasons, err.Error())
				codes = append(codes, ExitCode(err))
				blocked = true
				continue
			}
			candidateBundle = resolved
		}
		requested.Arch = candidate
		requested.DiskGiB = candidateDisk
		var chosen *NodeCapacity
		if artifact.IsDraft() {
			// A draft exists on one node, and its image is in that node's cache:
			// the box runs there.
			chosen, err = draftNode(nodes, args.Drafts, artifact.Version, candidate, args.NodeName)
		} else {
			chosen, _, err = ChooseNode(nodes, requested)
		}
		if err != nil {
			if ExitCode(err) == ExitUsage {
				return nil, err
			}
			reasons = append(reasons, err.Error())
			codes = append(codes, ExitCode(err))
			if noRoom(err) {
				sawRoom = true
			}
			continue
		}
		node, image, bundle, disk = chosen, artifact, candidateBundle, candidateDisk
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
		refused := &Error{Code: code, Msg: strings.Join(reasons, "; ")}
		// What waiting cannot change ends the command; a refusal that is only about
		// room keeps its own type, so `--wait D` asks again with the same sentence
		// and the same exit code.
		if sawRoom && !blocked {
			return nil, &noRoomError{cause: refused}
		}
		return nil, refused
	}
	return &boxPlacement{Node: node, Image: image, Bundle: bundle, Disk: disk}, nil
}

// How `box create --wait D` asks again while nothing fits: a node's capacity is
// a moving target (a box finishes, another is created), so the question is
// repeated, and the reason is repeated at a pace a person can read.
const (
	// capacityPollInterval is how often the capacity is read again.
	capacityPollInterval = 30 * time.Second
	// capacityNoteInterval is how often the reason is written to stderr.
	capacityNoteInterval = 5 * time.Minute
)

// placeAndWait places a box, and while the only thing in the way is room it asks
// again: every capacityPollInterval, with a line on stderr at the start and every
// capacityNoteInterval, until the box fits or waitFor is up.
//
// What comes back when the waiting ends is the refusal the placement gave, with
// its own exit code. Nothing is retried that waiting cannot change: a wrong
// architecture, a missing image, an unhealthy driver or a usage error ends the
// command at once.
func (a *App) placeAndWait(ctx context.Context, args placementArgs, waitFor time.Duration) (*boxPlacement, error) {
	placed, err := a.placeBox(ctx, args)
	if err == nil {
		return placed, nil
	}
	if !noRoom(err) {
		return nil, err
	}
	// A refusal for lack of room is the moment the caller decides whether to stop
	// one of its own boxes, so the idle ones are named here -- whether this
	// refusal is the answer or the wait is only starting. It is bounded guest
	// calls in parallel, and it changes neither the sentence nor the exit code.
	a.noteIdleBoxes(ctx, args.Project, args.Arch)
	if waitFor <= 0 {
		return nil, err
	}
	start := a.now()
	deadline := start.Add(waitFor)
	noted := start
	a.noteWait("no room for box %s yet: %s; asking again every %s for up to %s, nothing has been created",
		args.ID, oneLine(err.Error(), 300), span(capacityPollInterval), span(waitFor))
	for {
		if err := a.sleep(ctx, capacityPollInterval); err != nil {
			return nil, wrapKeep(err, "stopped waiting for room for box %s", args.ID)
		}
		placed, err := a.placeBox(ctx, args)
		if err == nil {
			a.noteWait("room for box %s after %s; creating it", args.ID, span(a.now().Sub(start)))
			return placed, nil
		}
		// Something other than room is in the way now: report it as it is.
		if !noRoom(err) {
			return nil, err
		}
		refusal := err
		now := a.now()
		if now.Sub(noted) >= capacityNoteInterval {
			noted = now
			a.noteWait("still no room for box %s after %s: %s", args.ID, span(now.Sub(start)), oneLine(err.Error(), 300))
		}
		if !now.Before(deadline) {
			a.noteWait("no room for box %s in %s; the placement is refused as it was", args.ID, span(waitFor))
			// The refusal is final: the idle boxes are the answer to it.
			a.noteIdleBoxes(ctx, args.Project, args.Arch)
			return nil, refusal
		}
	}
}

// boxRequest is a box ready to be created: the image, the bundle and the node are
// decided, and what is left is its volume, its job and the wait for it to run.
type boxRequest struct {
	ID string
	// Image is the artifact the box's disk is built from: a published image or a
	// draft on the node that holds it.
	Image *ImageArtifact
	// Platform is the bundle the guest mounts, nil for an image that carries its
	// own boxd.
	Platform *PlatformArtifact
	// Node is where the box runs and its disk lives.
	Node      *NodeCapacity
	CPUs      int
	MemoryMiB int
	DiskGiB   int
	// Project is the project label the box carries, empty for a box that belongs
	// to none.
	Project string
	// VaultRole is the Vault role the box reads its secrets with.
	VaultRole string
	// ExecutorItems are the items of the executors variable the job renders.
	ExecutorItems []ExecutorItem
}

// createBox registers the volume and the job of a box and waits for it to run.
//
// It is the second half of `box create`, and the whole of what `box bake --rebuild`
// does for its temporary box: everything here works on a decision already made.
func (a *App) createBox(ctx context.Context, req boxRequest, wait bool, timeout time.Duration) (*createOutput, error) {
	if req.Image == nil {
		return nil, errorf("a box needs an image")
	}
	if req.Node == nil {
		return nil, errorf("a box needs a node")
	}
	id, node, image, bundle := req.ID, req.Node, req.Image, req.Platform
	spec := BoxSpec{
		CPUMHzPerVCPU: int(node.vcpuMHz()),
		ID:            id,
		ImageVersion:  image.Version,
		ImageURL:      image.URL,
		ImageSHA256:   image.SHA256,
		Arch:          image.Arch,
		CPUs:          req.CPUs,
		MemoryMiB:     req.MemoryMiB,
		DiskGiB:       req.DiskGiB,
		NodeID:        node.NodeID,
		Project:       req.Project,
		// A project image carries its recipe: a box made from it passes it on, so
		// baking this box again, or rebuilding it on a newer parent, has the
		// recipe that made its environment.
		Recipe: image.Recipe,
	}
	if bundle != nil {
		spec.PlatformVersion = bundle.Version
		spec.PlatformURL = bundle.URL
		spec.PlatformSHA256 = bundle.SHA256
	}
	spec.ExecutorItems = req.ExecutorItems
	spec.VaultRole = strings.TrimSpace(req.VaultRole)
	a.noteNoVaultRole(spec)
	job, err := RenderJob(spec)
	if err != nil {
		return nil, err
	}
	volume, err := RenderVolume(VolumeSpec{BoxID: id, NodeID: node.NodeID, DiskGiB: req.DiskGiB})
	if err != nil {
		return nil, err
	}
	// The description of the box is the job as JSON, and Nomad's variable size
	// limit is checked before anything is created: a job too large to describe
	// would leave a box nobody can start again.
	specDoc, err := encodeBoxSpec(job, a.now())
	if err != nil {
		return nil, err
	}
	if err := requireBoxSpecFits(id, specDoc); err != nil {
		return nil, err
	}

	created, err := a.Nomad.CreateHostVolume(ctx, volume)
	if err != nil {
		return nil, wrapf(err, "Nomad could not create the box disk %s on node %s",
			BoxVolumeName(id), node.Name)
	}
	out := &createOutput{
		Job:      BoxJobName(id),
		Volume:   created.ID,
		Image:    image,
		Platform: bundle,
		Node:     node.Name,
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
			return nil, wrapKeep(err, "Nomad did not say whether the job %s was registered (%s), so nothing was deleted: "+
				"the box may well be running (`box status %s` shows it, `box rm %s --yes` removes it)",
				BoxJobName(id), oneLine(errText(probe.Err), 200), id, id)
		case !probe.Found:
			// The job is definitively gone: the disk has no box left to serve.
			a.discardVolume(ctx, created)
			return nil, wrapf(err, "Nomad rejected the box job %s%s", BoxJobName(id), registerDenied(err, BoxVolumeName(id)))
		case !probe.IsBox:
			return nil, errorf("the job %s exists but is not a box (it has no meta.box = \"true\"), so the disk %s was kept: "+
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

	// The description is what makes the box survive Nomad's job GC, so a create
	// that cannot store it takes the box away again: a disk with no description
	// is a box nobody can start, and `box create` must not leave one behind.
	if err := a.writeBoxSpec(ctx, id, specDoc); err != nil {
		if cleanupErr := a.discardBox(ctx, id, created); cleanupErr != nil {
			return nil, &Error{Code: ExitCode(err), Msg: err.Error() + "; " + cleanupErr.Error() +
				"; the box was not removed: `box start " + id + "` writes its description, and " +
				"`box rm " + id + " --yes` removes it"}
		}
		return nil, wrapf(err, "box %s was not created: its description could not be stored, so the box and its "+
			"disk were removed again", id)
	}

	if !wait {
		rec, _, err := a.findBox(ctx, id)
		if err != nil {
			return nil, err
		}
		out.Box = rec
		out.Warn = joinNotes(lost, "the box was created and is starting; `box status "+id+"` shows when it is ready")
		return out, nil
	}

	rec, err := a.waitForBoxRunning(ctx, id, evalID, timeout)
	if err != nil {
		// A box that never got an allocation is taken away again: there is
		// nothing to look at and nothing to keep. A box that did start and then
		// failed is kept, because its task events and its log are the evidence.
		if rec.AllocationID == "" {
			if cleanupErr := a.discardBox(ctx, id, created); cleanupErr != nil {
				return nil, &Error{Code: ExitCode(err), Msg: err.Error() + "; " + cleanupErr.Error() +
					"; `box rm " + id + " --yes` removes it"}
			}
			return nil, err
		}
		return nil, wrapKeep(err, "the box %s was kept so you can look at it (`box status %s`, `box logs %s`); `box rm %s --yes` removes it",
			id, id, id, id)
	}
	out.Box = rec
	out.Warn = lost
	if resp != nil && resp.Warnings != "" {
		out.Warn = joinNotes(lost, oneLine(resp.Warnings, 300))
	}
	return out, nil
}

// printCreate writes the result of `box create`.
func (a *App) printCreate(out *createOutput) error {
	if a.JSON {
		return a.printJSON(out)
	}
	box := out.Box
	a.note("created box %s (job %s) on node %s", box.ID, out.Job, out.Node)
	a.note("  image  %s (%s) sha256 %s", out.Image.Version, out.Image.Arch, shortSHA(out.Image.SHA256))
	if out.Platform != nil {
		a.note("  platform %s (boxd %s, sha256 %s)", out.Platform.Version, dash(out.Platform.Boxd),
			shortSHA(out.Platform.SHA256))
	}
	a.note("  size   %d vCPU, %d MiB RAM, %d GiB disk", box.CPUs, box.MemoryMiB, box.DiskGiB)
	a.note("  state  %s%s", box.State, allocationSuffix(box))
	if out.Warn != "" {
		a.note("  note   %s", out.Warn)
	}
	if box.State == "running" {
		a.note("next: box session new %s --executor prime --model <provider>/<model>", box.ID)
	}
	// The same advice `box wait` gives, in the moment the box is made: a box is
	// stopped when its work pauses, and stopping it costs nothing but the wait to
	// start it again.
	a.note("a stopped box holds no CPU and no memory: `box stop %s` gives them back, and `box start %s` "+
		"brings it back with its disk and its sessions", box.ID, box.ID)
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
