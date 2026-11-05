package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/nomad/api"
)

// `box bake` turns a box into an image, and `box bake --rebuild` replays an
// image's recipe on a newer base image.
//
// A bake does not empty the box: the driver copies the box's disk into a Lima
// clone on the node, seals the clone, compresses it into the image cache and
// deletes it. The box stops while that runs and starts again afterwards, with
// /work and its sessions where they were. So the steps here are: check that the
// seal would run at all (in the box, before anything stops), check that the node
// has room for the copy, stop the box, register the bake job, wait for it, and
// start the box again -- whatever the bake answered.

const (
	// DefaultBakeTimeout is how long `box bake` waits for the bake job. The job
	// boots a clone of the box, seals it and compresses its disk: minutes.
	DefaultBakeTimeout = 30 * time.Minute
	// DefaultRebuildTimeout is how long a rebuild may take: it runs a project's
	// whole recipe in a box and then bakes the result.
	DefaultRebuildTimeout = time.Hour
	// bakeStopTimeout bounds the stop of a box before its bake.
	bakeStopTimeout = 3 * time.Minute
	// bakeRestartTimeout bounds the start of a box a bake stopped.
	bakeRestartTimeout = 10 * time.Minute
)

// bakeOutput is the JSON document of `box bake`.
type bakeOutput struct {
	Box     string `json:"box,omitempty"`
	Version string `json:"version"`
	Arch    string `json:"arch"`
	Name    string `json:"name"`
	Parent  string `json:"parent,omitempty"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
	// VirtualBytes is the size of the disk inside the image.
	VirtualBytes int64  `json:"virtual_bytes,omitempty"`
	Node         string `json:"node,omitempty"`
	// Draft is true for an image that exists on its node only.
	Draft bool `json:"draft"`
	// Restarted is true when the box ran before the bake and runs again after it.
	Restarted bool `json:"restarted,omitempty"`
	// RebuiltFrom names the image a rebuild started from.
	RebuiltFrom string `json:"rebuilt_from,omitempty"`
	// BoxKept is the temporary box a failed rebuild left behind.
	BoxKept string `json:"box_kept,omitempty"`
}

// cmdBake bakes a box into an image, or rebuilds an image on a newer base.
func (a *App) cmdBake(ctx context.Context, args []string) error {
	fs := newFlagSet("bake")
	name := fs.String("name", "", "the image's name: the project it belongs to (its versions are NAME-1, NAME-2, ...)")
	recipe := fs.String("recipe", "", "the script that makes this environment from the parent image (a file on this host)")
	noRecipe := fs.Bool("no-recipe", false, "bake without a recipe: such an image cannot be rebuilt later")
	timeout := fs.Duration("timeout", DefaultBakeTimeout, "how long to wait for the bake")
	rebuild := fs.String("rebuild", "", "rebuild this image from its recipe on a newer base image")
	base := fs.String("base", "", "the base image a rebuild starts from (default: the newest base image)")
	if err := parseFlagsMin(fs, args, 0); err != nil {
		return err
	}
	if *recipe != "" && *noRecipe {
		return usagef("box bake: --recipe and --no-recipe say opposite things, give one")
	}
	if *timeout <= 0 {
		return usagef("--timeout must be positive, got %s", *timeout)
	}
	if *rebuild != "" {
		if fs.NArg() > 0 {
			return usagef("box bake --rebuild takes no box: it makes its own temporary box (unexpected %q)", fs.Arg(0))
		}
		if *name != "" || *recipe != "" || *noRecipe {
			return usagef("box bake --rebuild takes --base and --timeout only: the image names itself, and its " +
				"recipe is the one it carries")
		}
		return a.rebuildImage(ctx, *rebuild, *base, *timeout)
	}
	if fs.NArg() != 1 {
		return usagef("box bake needs a box: box bake <id> --name NAME [--recipe FILE | --no-recipe] [--timeout D]")
	}
	id := fs.Arg(0)
	if strings.TrimSpace(*name) == "" {
		return usagef("box bake %s: --name is required: it is the project the image belongs to, for example --name my-project", id)
	}
	if err := ValidateImageName(*name); err != nil {
		return err
	}
	var recipeData []byte
	if *recipe != "" {
		data, err := os.ReadFile(*recipe)
		if err != nil {
			return wrapf(err, "cannot read the recipe %s", *recipe)
		}
		if len(data) == 0 {
			return usagef("the recipe %s is empty", *recipe)
		}
		if len(data) > maxRecipeBytes {
			return usagef("the recipe %s is %d bytes, more than the %d an image carries; a longer script belongs "+
				"in the project's sources, and the recipe prepares them", *recipe, len(data), maxRecipeBytes)
		}
		recipeData = data
	}
	out, err := a.bakeBox(ctx, id, bakeOptions{
		Name:     *name,
		Recipe:   recipeData,
		NoRecipe: *noRecipe,
		Timeout:  *timeout,
	})
	if err != nil {
		return err
	}
	return a.printBake(out)
}

// maxRecipeBytes is the largest recipe an image carries (boxd.RecipeLimit).
const maxRecipeBytes = 64 << 10

// bakeOptions is one bake of one box.
type bakeOptions struct {
	// Name is the image's name; its version is computed here.
	Name string
	// Recipe is the recipe's script; empty means the image's own recipe, if it has
	// one.
	Recipe   []byte
	NoRecipe bool
	Timeout  time.Duration
	// Temporary is a box the bake made for itself (a rebuild): it is stopped,
	// baked, and left stopped, because the command deletes it.
	Temporary bool
}

// bakeBox bakes a box into an image and gives the box back.
func (a *App) bakeBox(ctx context.Context, id string, opts bakeOptions) (*bakeOutput, error) {
	box, job, err := a.findBox(ctx, id)
	if err != nil {
		return nil, err
	}
	if job == nil {
		return nil, orphanCannot(id, "bake")
	}
	spec, err := SpecFromJob(job)
	if err != nil {
		return nil, err
	}
	// The image decides whether this can work at all: boxd and the executors are
	// not in an image, they come from the node's bundle mounted at /opt/box, so
	// only an image that expects a bundle can be baked.
	if !takesPlatformBundle(spec) {
		return nil, errorf("box %s runs the image %s, which carries its own boxd and takes no platform bundle: "+
			"only an image with \"platform\": \"mount\" can be baked, because boxd and the executors are not in an "+
			"image -- they are the node's bundle (see docs/guest-image.md)", id, dash(spec.ImageVersion))
	}
	recipe, err := a.bakeRecipe(spec, opts)
	if err != nil {
		return nil, err
	}
	versions, err := a.imageVersions(ctx, opts.Name)
	if err != nil {
		return nil, err
	}
	version, err := NextImageVersion(opts.Name, versions)
	if err != nil {
		return nil, err
	}
	wasRunning := box.State == "running"
	// The seal's refusals -- a running turn, a preparation in progress -- are
	// asked of the box before anything is stopped. They cannot be asked of the
	// clone later: its boxd is a fresh daemon, and it would answer at once.
	if wasRunning && !opts.Temporary {
		if err := a.checkSealGate(ctx, id); err != nil {
			return nil, err
		}
	}
	// A clone is a full copy of the disk: the node must have room for it.
	if err := a.checkBakeRoom(ctx, box, spec); err != nil {
		return nil, err
	}
	if wasRunning {
		if !a.JSON {
			a.note("stopping box %s: a bake copies its disk, and a copy of a running box is a disk being written", id)
		}
		if err := a.Nomad.StopJob(ctx, BoxJobName(id), false); err != nil {
			return nil, wrapf(err, "Nomad could not stop the job %s", BoxJobName(id))
		}
		if _, err := a.waitForBoxState(ctx, id, "stopped", bakeStopTimeout); err != nil {
			return nil, wrapKeep(err, "box %s is still running, so nothing was baked; `box status %s` shows what "+
				"Nomad reports, and `box bake %s --name %s` runs once it is stopped", id, id, id, opts.Name)
		}
	}
	out, bakeErr := a.bakeStopped(ctx, box, spec, opts.Name, version, recipe, opts.Timeout)
	if wasRunning && !opts.Temporary {
		// Whatever the bake answered, the box goes back to work: it was running,
		// and its work is still on its disk.
		back, startErr := a.startBoxJob(ctx, id, spec, true, bakeRestartTimeout)
		if startErr != nil {
			msg := fmt.Sprintf("the box %s does not run again: %v. Its disk is untouched, so `box start %s` runs it "+
				"again with /work and its sessions", id, startErr, id)
			if bakeErr != nil {
				return nil, wrapKeep(bakeErr, "%s (the bake failed too: %v)", msg, bakeErr)
			}
			return nil, wrapKeep(startErr, "%s", "")
		}
		_ = back
		if out != nil {
			out.Box = id
			out.Restarted = true
		}
		if !a.JSON {
			a.note("box %s runs again on the same disk: /work and the sessions are still there", id)
		}
	}
	if bakeErr != nil {
		return nil, bakeErr
	}
	if out == nil {
		return nil, errorf("the bake of box %s reported no image", id)
	}
	return out, nil
}

// bakeStopped registers the bake job, waits for it and returns what it made.
func (a *App) bakeStopped(ctx context.Context, box BoxRecord, spec BoxSpec, name, version string, recipe []byte, timeout time.Duration) (*bakeOutput, error) {
	id := box.ID
	return a.runBakeJob(ctx, bakeJobSpecFor(id, spec, name, version, recipe), timeout)
}

// takesPlatformBundle reports whether a box's image expects a platform bundle.
//
// The box's own job says it: the CLI adds a platform block only for an image whose
// index entry carries "platform": "mount", so an image that carried its own boxd
// has none, and the box is exactly the box it was before bundles existed.
func takesPlatformBundle(spec BoxSpec) bool {
	return spec.PlatformURL != "" && spec.PlatformSHA256 != ""
}

// bakeRecipe is the recipe the image will carry: the one given, or the one the
// box's own image carries. --no-recipe means none.
func (a *App) bakeRecipe(spec BoxSpec, opts bakeOptions) ([]byte, error) {
	if opts.NoRecipe {
		return nil, nil
	}
	if len(opts.Recipe) > 0 {
		return opts.Recipe, nil
	}
	if spec.Recipe != "" {
		script, err := decodeRecipe(spec.Recipe)
		if err != nil {
			return nil, err
		}
		a.note("the recipe comes from the image the box was created from; --recipe FILE replaces it")
		return []byte(script), nil
	}
	a.note("this image has no recipe: it can be created from, but not rebuilt on a newer base image")
	return nil, nil
}

// imageVersions lists every published version and every draft, which is what a
// new version's number is counted from.
func (a *App) imageVersions(ctx context.Context, name string) ([]string, error) {
	var out []string
	index, err := a.loadIndexOptional(ctx)
	if err != nil {
		return nil, err
	}
	if index != nil {
		out = append(out, index.Versions()...)
	}
	drafts, err := a.nodeDrafts(ctx)
	if err != nil {
		return nil, err
	}
	for _, draft := range drafts {
		out = append(out, draft.Version)
	}
	return out, nil
}

// loadIndexOptional reads the image index, and returns nil when the store has not
// published one: that is a store without images, not a failure of the command.
func (a *App) loadIndexOptional(ctx context.Context) (*ImageIndex, error) {
	url, err := a.requireImageIndexURL()
	if err != nil {
		// A coordinator without an index can still bake and create from drafts.
		return nil, nil
	}
	index, err := LoadImageIndex(ctx, a.Images, url)
	if err != nil {
		if errors.Is(err, ErrIndexMissing) {
			return nil, nil
		}
		return nil, err
	}
	return index, nil
}

// checkSealGate asks the box whether the seal would run, through a boxctl call
// that does not change anything. A box with a turn running or a preparation in
// progress is refused here, before it is stopped: the clone the seal runs in has
// a fresh boxd that knows nothing of that turn.
func (a *App) checkSealGate(ctx context.Context, id string) error {
	client, _, err := a.boxctlFor(ctx, id)
	if err != nil {
		return wrapKeep(err, "box %s cannot be baked: the seal's refusals are checked inside the box first, and the "+
			"guest did not answer; nothing was stopped and nothing was baked", id)
	}
	info, err := client.Info(ctx)
	if err != nil {
		return wrapKeep(err, "box %s cannot be baked: boxctl info says: %v; nothing was stopped and nothing was baked", id, err)
	}
	if info.Seal == nil {
		// A boxd that does not report the gate: the seal inside the clone refuses
		// what it knows of, and the box is only stopped for the bake.
		return nil
	}
	if !info.Seal.OK {
		return errorf("box %s is busy: %s. Nothing was stopped and nothing was baked: `box bake %s --name ...` runs "+
			"once the box is idle", id, info.Seal.Reason, id)
	}
	return nil
}

// checkBakeRoom refuses a bake whose clone would not fit: a clone is a full copy
// of the box's disk, and both live on the node's filesystem.
func (a *App) checkBakeRoom(ctx context.Context, box BoxRecord, spec BoxSpec) error {
	if box.NodeID == "" {
		return nil
	}
	nodes, err := a.nodeCapacities(ctx)
	if err != nil {
		return nil // an unreadable node is not proof of a missing disk
	}
	for _, node := range nodes {
		if node.NodeID != box.NodeID {
			continue
		}
		used := int64(0)
		for _, disk := range node.Disks {
			if disk.BoxID == box.ID {
				used = disk.UsedBytes
			}
		}
		if used == 0 || node.DiskFreeBytes == 0 {
			return nil // the driver reports neither yet: the bake checks the room itself
		}
		if node.DiskFreeBytes < used {
			return errorf("node %s has %s free where a bake writes the copy of the box's disk, and the copy of box "+
				"%s's disk needs %s: a bake clones the disk before it seals it. Free that much space on the node "+
				"(`box rm <id> --yes` deletes the disk of a box you no longer need), or bake a box with a smaller "+
				"disk", node.Name, humanBytes(node.DiskFreeBytes), box.ID, humanBytes(used))
		}
		return nil
	}
	return nil
}

// bakeJobSpecFor is the job spec of a box's bake.
func bakeJobSpecFor(id string, spec BoxSpec, name, version string, recipe []byte) BakeJobSpec {
	return BakeJobSpec{
		BoxID:   id,
		Name:    name,
		Version: version,
		Parent:  spec.ImageVersion,
		Arch:    spec.Arch,
		Recipe:  encodeRecipe(recipe),
		// The clone mounts the bundle the box mounts: its boxd, its boxctl and its
		// guest/no-secrets.sh are the seal's.
		PlatformVersion: spec.PlatformVersion,
		PlatformURL:     spec.PlatformURL,
		PlatformSHA256:  spec.PlatformSHA256,
		// The bake boots a VM as big as the box in its own task, and then
		// compresses a disk with one core: both have to be reserved, or the client
		// kills the task for the memory it takes.
		CPUShares: spec.TaskCPUShares() + bakeCPUShares,
		MemoryMiB: spec.TaskMemoryMiB(),
	}
}

// runBakeJob registers the bake job, waits for it and reads what it made.
//
// The job is purged afterwards: its one allocation is the evidence of the bake,
// and the image's record on the node is what says the bake happened. The driver's
// events are printed as they arrive, because the seal's steps are what a person
// watches.
func (a *App) runBakeJob(ctx context.Context, jobSpec BakeJobSpec, timeout time.Duration) (*bakeOutput, error) {
	job, err := RenderBakeJob(jobSpec)
	if err != nil {
		return nil, err
	}
	jobID := derefStr(job.ID, "")
	evalID, since, err := a.registerWorkJob(ctx, job)
	if err != nil {
		return nil, wrapf(err, "Nomad rejected the bake job %s%s", jobID, registerDenied(err, BoxVolumeName(jobSpec.BoxID)))
	}
	if !a.JSON {
		a.note("baking %s on the node that holds its disk (job %s)", jobSpec.Version, jobID)
	}
	// The task's own log has the whole bake -- a seal prints more lines than
	// Nomad keeps as task events -- so it is followed here and printed as it
	// comes.
	logCtx, stopLog := context.WithCancel(ctx)
	defer stopLog()
	logDone := make(chan struct{})
	go func() {
		defer close(logDone)
		a.followWorkLog(logCtx, jobID, since, timeout)
	}()
	alloc, err := a.waitForJobDone(ctx, jobID, evalID, since, timeout)
	// The follower is waited for before this command returns: a goroutine that
	// printed after the command did would write into a terminal (or a test's
	// buffer) nobody owns any more. The stream ends by itself once the task is
	// done; a stream that does not is cut.
	select {
	case <-logDone:
	case <-time.After(10 * time.Second):
		stopLog()
		<-logDone
	}
	if err != nil {
		return nil, err
	}
	// The bake is done: the job has nothing left to say.
	if purgeErr := a.Nomad.StopJob(ctx, jobID, true); purgeErr != nil && !isNotFoundErr(purgeErr) {
		a.note("note: the bake job %s could not be purged (%s); `nomad job stop -purge %s` removes it",
			jobID, oneLine(purgeErr.Error(), 120), jobID)
	}
	if failed := allocFailureText(alloc); failed != "" && !allocSucceeded(alloc) {
		return nil, errorf("the bake of %s failed: %s (`nomad alloc logs` of %s has the rest; the box's own disk is "+
			"untouched)", jobSpec.Version, failed, shortID(alloc.ID))
	}
	res, ok := bakeResultOf(alloc)
	if !ok {
		return nil, errorf("the bake job %s ended without reporting what it made; `box images` lists the drafts of "+
			"the nodes, and the node's image cache has a record under baked/", jobID)
	}
	return &bakeOutput{
		Version:      res.Version,
		Arch:         res.Arch,
		Name:         res.Name,
		Parent:       res.Parent,
		SHA256:       res.SHA256,
		Size:         res.Size,
		VirtualBytes: res.VirtualBytes,
		Node:         alloc.NodeName,
		Draft:        true,
	}, nil
}

// followWorkLog prints a bake job's task log while it runs.
//
// The log is the whole of what the driver did -- every step and every line of the
// seal -- which Nomad's ten task events are too few to hold. A job whose
// allocation is not placed yet has no log: this waits for it, and gives up when
// the wait does.
func (a *App) followWorkLog(ctx context.Context, jobID string, since uint64, timeout time.Duration) {
	if a.JSON || a.ErrOut == nil {
		return
	}
	deadline := a.now().Add(timeout)
	var alloc *api.Allocation
	for {
		byJob, err := a.jobAllocations(ctx)
		if err == nil {
			if newest := latestAllocation(allocsSince(byJob[jobID], since)); newest != nil {
				if full, err := a.Nomad.Allocation(ctx, newest.ID); err == nil {
					alloc = full
					break
				}
			}
		}
		if !a.now().Before(deadline) {
			return
		}
		if err := a.sleep(ctx, waitPollInterval); err != nil {
			return
		}
	}
	// The steps of a bake are what a person follows while it runs; the task's
	// own exit is what the command reports, so a stream that ends early is not
	// an error here and is not picked up again.
	var offset int64
	_ = a.streamLogOnce(ctx, alloc, "stdout", true, &logWriter{a: a}, &offset, nil)
}

// allocSucceeded reports whether an allocation's task exited 0.
func allocSucceeded(alloc *api.Allocation) bool {
	if alloc == nil {
		return false
	}
	state, ok := alloc.TaskStates[boxTaskName]
	if !ok || state == nil {
		return false
	}
	return state.State == "dead" && !state.Failed
}

// registerWorkJob registers a batch job under a name that comes back: a bake or a
// publish of the same box. What the last one left is purged first, because Nomad
// starts nothing for a job it already ran with the same content, and its failed
// allocation would answer for this run. The index it returns tells this run's
// allocations from any older one Nomad still lists.
func (a *App) registerWorkJob(ctx context.Context, job *api.Job) (string, uint64, error) {
	jobID := derefStr(job.ID, "")
	if err := a.Nomad.StopJob(ctx, jobID, true); err != nil && !isNotFoundErr(err) {
		return "", 0, wrapf(err, "the last job %s could not be purged", jobID)
	}
	resp, err := a.Nomad.RegisterJob(ctx, job)
	if err != nil {
		return "", 0, err
	}
	if resp == nil {
		return "", 0, nil
	}
	return resp.EvalID, resp.JobModifyIndex, nil
}

// allocsSince keeps the allocations made after the job's registration at index
// since (0: all of them).
func allocsSince(allocs []*api.AllocationListStub, since uint64) []*api.AllocationListStub {
	if since == 0 {
		return allocs
	}
	kept := make([]*api.AllocationListStub, 0, len(allocs))
	for _, alloc := range allocs {
		if alloc != nil && alloc.CreateIndex >= since {
			kept = append(kept, alloc)
		}
	}
	return kept
}

// waitForJobDone waits until a batch job has no live allocation, and returns its
// newest allocation with the task events that say how it ended.
func (a *App) waitForJobDone(ctx context.Context, jobID, evalID string, since uint64, timeout time.Duration) (*api.Allocation, error) {
	deadline := a.now().Add(timeout)
	last := "Nomad has not placed it yet"
	for {
		byJob, err := a.jobAllocations(ctx)
		if err != nil {
			return nil, wrapf(err, "cannot list the allocations of %s", jobID)
		}
		newest := latestAllocation(allocsSince(byJob[jobID], since))
		switch {
		case newest == nil:
			if blocker := a.evalBlocker(ctx, evalID); blocker != "" {
				last = blocker
			}
		case isTerminalAlloc(newest.ClientStatus):
			alloc, err := a.Nomad.Allocation(ctx, newest.ID)
			if err != nil {
				return nil, wrapf(err, "cannot read the allocation %s of %s", newest.ID, jobID)
			}
			return alloc, nil
		default:
			last = fmt.Sprintf("the allocation %s is %s", shortID(newest.ID), newest.ClientStatus)
		}
		if !a.now().Before(deadline) {
			return nil, timeoutf("the job %s did not finish within %s (%s); `nomad job status %s` shows where it is, "+
				"and `box images` lists a draft as soon as the node has it", jobID, timeout, last, jobID)
		}
		if err := a.sleep(ctx, waitPollInterval); err != nil {
			return nil, wrapf(err, "stopped waiting for %s", jobID)
		}
	}
}

// rebuildImage replays an image's recipe on a newer base image.
//
// The recipe is what makes a project's environment from a base image, so a newer
// base is a rebuild: a temporary box is created from the new base image, the recipe
// runs in it, and the result is baked as the next version of the same name. The
// temporary box is deleted; on a failure it is kept, because what it ran is the
// only way to see why.
func (a *App) rebuildImage(ctx context.Context, named, base string, timeout time.Duration) error {
	source, err := a.latestProjectImage(ctx, named)
	if err != nil {
		return err
	}
	script, err := decodeRecipe(source.Recipe)
	if err != nil {
		return err
	}
	if strings.TrimSpace(script) == "" {
		return errorf("the image %s carries no recipe, and a rebuild replays the recipe that made the environment: "+
			"bake an image from a box with --recipe FILE instead, then rebuild that one", source.Version)
	}
	if source.Name == "" {
		return errorf("the image %s does not say which project it belongs to; `box images` shows the name of every project image", source.Version)
	}
	parent, bundle, err := a.baseImageFor(ctx, base, source.Arch)
	if err != nil {
		return err
	}
	boxID := source.Name + "-rebuild"
	if err := ValidateBoxID(boxID); err != nil {
		return usagef("the project name %q makes the box id %q, which is not usable: use a name of at most %d characters",
			source.Name, boxID, 40-len("-rebuild"))
	}
	if existing, _, err := a.findBox(ctx, boxID); err == nil {
		return errorf("the box %q already exists (it is %s): a rebuild needs that name for its temporary box, "+
			"`box rm %s --yes` deletes it", boxID, existing.State, boxID)
	} else if e, ok := err.(*Error); !ok || e.Code != ExitNotFound {
		return err
	}
	// The disk has to hold the parent image's disk, and never less than the
	// source image had: the recipe installs on top of what the parent is.
	diskGiB := int64(defaultDiskGiB)
	for _, want := range []int64{gibOf(source.VirtualBytes), gibOf(parent.VirtualBytes)} {
		if want > diskGiB {
			diskGiB = want
		}
	}
	nodes, err := a.nodeCapacities(ctx)
	if err != nil {
		return err
	}
	node, _, err := ChooseNode(nodes, PlacementRequest{Arch: parent.Arch, CPUs: defaultCPUs,
		MemoryMiB: defaultMemoryMiB, DiskGiB: int(diskGiB)})
	if err != nil {
		return wrapf(err, "a rebuild needs a node with room for a %d GiB box", diskGiB)
	}
	version, err := NextImageVersion(source.Name, mustVersions(a.imageVersions(ctx, source.Name)))
	if err != nil {
		return err
	}
	if !a.JSON {
		a.note("rebuilding %s: a temporary box %s from the base image %s, then its recipe", source.Version, boxID, parent.Version)
	}
	created, err := a.createBox(ctx, boxRequest{
		ID:            boxID,
		Image:         parent,
		Platform:      bundle,
		Node:          node,
		CPUs:          defaultCPUs,
		MemoryMiB:     defaultMemoryMiB,
		DiskGiB:       int(diskGiB),
		VaultRole:     a.VaultRole(),
		ExecutorItems: a.executorItems(ctx),
	}, true, timeout)
	if err != nil {
		return err
	}
	keep := func(cause error) error {
		return wrapKeep(cause, "the temporary box %s was kept: its task events and the log of what it ran are the way to "+
			"see why (`box logs %s`); `box rm %s --yes` deletes it", boxID, boxID, boxID)
	}
	spec, err := a.specOfBox(ctx, boxID)
	if err != nil {
		return keep(err)
	}
	if !takesPlatformBundle(spec) {
		return keep(errorf("the box %s was created from %s, which takes no platform bundle, so it cannot be sealed",
			boxID, parent.Version))
	}
	if !a.JSON {
		a.note("running the recipe of %s in %s (up to %s)", source.Version, boxID, timeout)
	}
	if err := a.runScriptInBox(ctx, boxID, []byte(script), timeout); err != nil {
		return keep(err)
	}
	// The rebuilt image carries the recipe that made it, so it can be rebuilt
	// again on an even newer parent.
	baked, err := a.bakeBox(ctx, boxID, bakeOptions{
		Name:      source.Name,
		Recipe:    []byte(script),
		Timeout:   timeout,
		Temporary: true,
	})
	if err != nil {
		return keep(err)
	}
	// The version was counted before the recipe ran, so the box's bake counts it
	// again; keep the number this command promised.
	if baked.Version != version {
		a.note("note: the rebuilt image is %s, not %s: another version appeared while the recipe ran", baked.Version, version)
	}
	baked.RebuiltFrom = source.Version
	// The temporary box has done its work: its disk is the image now.
	if err := a.deleteBox(ctx, created.Box, bakeStopTimeout); err != nil {
		a.note("note: the temporary box %s stayed behind (%s); `box rm %s --yes` deletes it", boxID,
			oneLine(err.Error(), 160), boxID)
	}
	return a.printBake(baked)
}

// specOfBox reads the spec of a registered box.
func (a *App) specOfBox(ctx context.Context, id string) (BoxSpec, error) {
	_, job, err := a.findBox(ctx, id)
	if err != nil {
		return BoxSpec{}, err
	}
	return SpecFromJob(job)
}

// mustVersions keeps what imageVersions returned and reports a failed read as no
// versions at all: a version number that skips one is not worth failing a bake for.
func mustVersions(versions []string, err error) []string {
	if err != nil {
		return nil
	}
	return versions
}

// latestProjectImage finds an image of a project: the version named, or the newest
// version of the name among the published images and the drafts.
func (a *App) latestProjectImage(ctx context.Context, named string) (*ImageArtifact, error) {
	named = strings.TrimSpace(named)
	if named == "" {
		return nil, usagef("box bake --rebuild needs the image's name or version, for example --rebuild my-project")
	}
	byVersion := map[string]*ImageArtifact{}
	add := func(artifact *ImageArtifact) {
		if artifact == nil || artifact.Version == "" {
			return
		}
		if existing, ok := byVersion[artifact.Version]; ok && existing.IsDraft() && !artifact.IsDraft() {
			// A published image is the one every node can use.
			byVersion[artifact.Version] = artifact
			return
		}
		if _, ok := byVersion[artifact.Version]; !ok {
			byVersion[artifact.Version] = artifact
		}
	}
	index, err := a.loadIndexOptional(ctx)
	if err != nil {
		return nil, err
	}
	if index != nil {
		for _, version := range index.Versions() {
			for _, arch := range ImageArchitectures {
				artifact := index.Images[version][arch]
				if artifact == nil || !artifact.IsProject() {
					continue
				}
				if version != named && artifact.Name != named {
					continue
				}
				add(artifact)
			}
		}
	}
	drafts, err := a.nodeDrafts(ctx)
	if err != nil {
		return nil, err
	}
	for i := range drafts {
		draft := drafts[i].Artifact()
		if draft.Version == named || draft.Name == named {
			add(draft)
		}
	}
	if len(byVersion) == 0 {
		return nil, notFoundf("no image is named %q: `box images` lists the published images and the drafts of the nodes, "+
			"and `box bake <id> --name %s` makes the first one", named, named)
	}
	versions := make([]string, 0, len(byVersion))
	for version := range byVersion {
		versions = append(versions, version)
	}
	sort.Slice(versions, func(i, j int) bool { return versionLess(versions[j], versions[i]) })
	return byVersion[versions[0]], nil
}

// baseImageFor finds the base image a rebuild starts from, and the platform bundle
// its box mounts.
func (a *App) baseImageFor(ctx context.Context, base, arch string) (*ImageArtifact, *PlatformArtifact, error) {
	index, err := a.loadIndexOptional(ctx)
	if err != nil {
		return nil, nil, err
	}
	if index == nil {
		return nil, nil, errorf("a rebuild needs a base image, and this cluster has no image index: set BOX_IMAGE_INDEX " +
			"(or --image-index) to the index.json of a store that publishes base images")
	}
	want := strings.TrimSpace(base)
	if want == "" {
		want = "latest"
	}
	artifact, err := index.Resolve(want, arch)
	if err != nil {
		return nil, nil, wrapf(err, "cannot find the base image a rebuild starts from")
	}
	if err := artifact.Validate(); err != nil {
		return nil, nil, err
	}
	if artifact.Platform != "mount" {
		return nil, nil, errorf("the base image %s carries its own boxd and takes no platform bundle, so a box made from it "+
			"cannot be baked; a rebuild needs a base image with \"platform\": \"mount\"", artifact.Version)
	}
	bundle, err := index.ResolvePlatform("latest", arch)
	if err != nil {
		return nil, nil, wrapf(err, "the base image %s needs a platform bundle for %s, and the index lists none",
			artifact.Version, arch)
	}
	if err := bundle.Validate(); err != nil {
		return nil, nil, err
	}
	return artifact, bundle, nil
}

// runScriptInBox runs a shell script in a box with `box run`'s machinery: a session
// of its own, the box's shell, its output on this terminal, and its exit code.
//
// The script goes in on stdin, so a recipe of any length runs as it is.
func (a *App) runScriptInBox(ctx context.Context, id string, script []byte, limit time.Duration) error {
	box, _, err := a.findBox(ctx, id)
	if err != nil {
		return err
	}
	box, err = a.waitRunning(ctx, box, guestWaitTimeout)
	if err != nil {
		return err
	}
	alloc, err := a.runningAlloc(ctx, box)
	if err != nil {
		return err
	}
	tr, err := a.guestTransport(ctx, alloc)
	if err != nil {
		return err
	}
	pidFile := fmt.Sprintf("/tmp/box-run-%d-%d.pid", os.Getpid(), time.Now().UnixNano())
	command := []string{"setsid", "-w", "sh", "-c", runWrapper, pidFile, "bash", "-s"}
	runCtx := ctx
	if limit > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, limit)
		defer cancel()
	}
	code, err := tr.Exec(runCtx, ExecOpts{Command: command, Stdin: bytes.NewReader(script),
		Stdout: a.ErrOut, Stderr: a.ErrOut})
	if runCtx.Err() != nil {
		stopped := a.stopGuestRun(tr, pidFile)
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return timeoutf("the recipe in box %s ran out of its --timeout %s; %s", id, limit, stopped)
		}
		return errorf("the recipe in box %s was interrupted; %s", id, stopped)
	}
	if err != nil {
		return wrapf(err, "cannot run the recipe in box %s", id)
	}
	if code != 0 {
		return &Error{Code: ExitError, Msg: fmt.Sprintf("the recipe in box %s exited %d", id, code)}
	}
	return nil
}

// printBake writes the result of `box bake`.
func (a *App) printBake(out *bakeOutput) error {
	if a.JSON {
		return a.printJSON(out)
	}
	where := "a draft on node " + dash(out.Node)
	a.note("baked the image %s (%s): %s", out.Version, out.Arch, where)
	a.note("  sha256  %s", out.SHA256)
	a.note("  size    %s compressed, %s of disk inside", humanBytes(out.Size), diskSizeOf(out.VirtualBytes))
	if out.Parent != "" {
		a.note("  parent  %s", out.Parent)
	}
	if out.RebuiltFrom != "" {
		a.note("  from    %s (rebuilt on %s)", out.RebuiltFrom, dash(out.Parent))
	}
	if out.Box != "" && out.Restarted {
		a.note("  box     %s keeps its work: it runs again on the same disk", out.Box)
	}
	a.note("")
	a.note("next:")
	disk := "8"
	if out.VirtualBytes > 0 {
		disk = fmt.Sprintf("%d", gibOf(out.VirtualBytes))
	}
	a.note("  box create <id> --image %s --arch %s --disk-gib %s   (on node %s, no download)",
		out.Version, out.Arch, disk, dash(out.Node))
	a.note("  box images publish %s --yes                   (to make the image available on every node)", out.Version)
	return nil
}

// diskSizeOf renders the size of a disk inside an image.
func diskSizeOf(bytes int64) string {
	if bytes <= 0 {
		return "-"
	}
	return fmt.Sprintf("%d GiB", gibOf(bytes))
}

// gibOf rounds a byte count up to whole GiB, which is the unit a disk has.
func gibOf(bytes int64) int64 {
	const gib = int64(1) << 30
	if bytes <= 0 {
		return 0
	}
	return (bytes + gib - 1) / gib
}
