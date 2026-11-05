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
// A bake is three steps, in this order and for these reasons: the box is sealed
// from inside (boxctl seal deletes the work, the sessions and the credentials and
// proves the result), it is stopped (a seal leaves a box that must not be started
// again), and the driver bakes its disk into the node's image cache as a draft
// (the disk only exists on the node). The box is not deleted: its disk is the
// image now, and `box rm` is the next step.

const (
	// DefaultBakeTimeout is how long `box bake` waits for the bake job. A disk of
	// tens of gigabytes takes minutes to compress, and a node may be busy.
	DefaultBakeTimeout = 30 * time.Minute
	// DefaultRebuildTimeout is how long a rebuild may take: it runs a project's
	// whole recipe in a box and then bakes the result.
	DefaultRebuildTimeout = time.Hour
	// bakeStopTimeout bounds the stop of a sealed box before its bake.
	bakeStopTimeout = 3 * time.Minute
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
	// RebuiltFrom names the image a rebuild started from.
	RebuiltFrom string `json:"rebuilt_from,omitempty"`
	// BoxKept is the temporary box a failed rebuild left behind.
	BoxKept string `json:"box_kept,omitempty"`
}

// cmdBake seals a box into an image, or rebuilds an image on a newer base.
func (a *App) cmdBake(ctx context.Context, args []string) error {
	fs := newFlagSet("bake")
	name := fs.String("name", "", "the image's name: the project it belongs to (its versions are NAME-1, NAME-2, ...)")
	recipe := fs.String("recipe", "", "the script that makes this environment from the parent image (a file on this host)")
	noRecipe := fs.Bool("no-recipe", false, "bake without a recipe: such an image cannot be rebuilt later")
	yes := fs.Bool("yes", false, "seal the box without asking: the seal deletes /work and every session")
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
		return a.rebuildImage(ctx, *rebuild, *base, *yes, *timeout)
	}
	if fs.NArg() != 1 {
		return usagef("box bake needs a box: box bake <id> --name NAME [--recipe FILE | --no-recipe] [--yes] [--timeout D]")
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
		Yes:      *yes,
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
	Yes      bool
	Timeout  time.Duration
}

// bakeBox seals a box and bakes it into an image.
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
	if box.State != "running" {
		return nil, errorf("box %s is %s: the seal runs inside the box and deletes its work and its sessions, "+
			"so it has to be running (`box start %s` brings it back on the same disk)", id, box.State, id)
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
	if !opts.Yes {
		return nil, errorf("baking box %s seals it: the seal deletes /work and every session in it, and the box "+
			"cannot be sealed twice. Push the work out of the box first, then bake it with --yes "+
			"(the image is named %s, from the image %s)", id, version, dash(spec.ImageVersion))
	}
	return a.bakeSealed(ctx, box, spec, opts.Name, version, recipe, opts.Timeout)
}

// bakeSealed seals a running box, stops it and bakes its disk.
//
// It is the second half of every bake: `box bake <id>` and `box bake --rebuild`
// both reach it with a box that should become an image.
func (a *App) bakeSealed(ctx context.Context, box BoxRecord, spec BoxSpec, name, version string, recipe []byte, timeout time.Duration) (*bakeOutput, error) {
	id := box.ID
	if _, err := a.sealBox(ctx, box, spec, name, version, recipe); err != nil {
		return nil, err
	}
	// The sealed box is stopped, not deleted: its disk is the image now.
	if err := a.Nomad.StopJob(ctx, BoxJobName(id), false); err != nil {
		return nil, wrapf(err, "the box %s is sealed but Nomad could not stop it; a sealed box must not keep running "+
			"(`box stop %s`, then `box bake %s --name %s --yes` bakes it again)", id, id, id, name)
	}
	if _, err := a.waitForBoxState(ctx, id, "stopped", bakeStopTimeout); err != nil {
		return nil, wrapKeep(err, "the box %s is sealed and its image is not made yet; `box stop %s` then "+
			"`box bake %s --name %s --yes` finishes it", id, id, id, name)
	}
	out, err := a.runBakeJob(ctx, bakeJobSpecFor(id, spec, name, version, recipe), timeout)
	if err != nil {
		return nil, err
	}
	out.Box = id
	return out, nil
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

// sealBox runs boxctl seal in the box and reports its steps.
func (a *App) sealBox(ctx context.Context, box BoxRecord, spec BoxSpec, name, version string, recipe []byte) (*SealResult, error) {
	client, _, err := a.boxctlFor(ctx, box.ID)
	if err != nil {
		return nil, wrapKeep(err, "box %s cannot be sealed: the seal runs inside it (`box start %s` brings it back)", box.ID, box.ID)
	}
	if !a.JSON {
		a.note("sealing box %s into the image %s", box.ID, version)
	}
	res, err := client.Seal(ctx, SealOptions{
		Name:    name,
		Version: version,
		Parent:  spec.ImageVersion,
		Arch:    spec.Arch,
		Recipe:  recipe,
		Say: func(line string) {
			a.note("  %s", line)
		},
	})
	if err != nil {
		// The seal's own exit code is the command's: a failure here is the
		// failure boxctl reported, with its own words.
		return nil, &Error{Code: ExitCode(err), Msg: fmt.Sprintf("the seal of box %s failed", box.ID), Err: err}
	}
	return res, nil
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
	}
}

// runBakeJob registers the bake job, waits for it and reads what it made.
//
// The job is purged afterwards: its one allocation is the evidence of the bake,
// and the image's record on the node is what says the bake happened.
func (a *App) runBakeJob(ctx context.Context, jobSpec BakeJobSpec, timeout time.Duration) (*bakeOutput, error) {
	job, err := RenderBakeJob(jobSpec)
	if err != nil {
		return nil, err
	}
	jobID := derefStr(job.ID, "")
	resp, err := a.Nomad.RegisterJob(ctx, job)
	if err != nil {
		return nil, wrapf(err, "Nomad rejected the bake job %s%s", jobID, registerDenied(err, BoxVolumeName(jobSpec.BoxID)))
	}
	evalID := ""
	if resp != nil {
		evalID = resp.EvalID
	}
	if !a.JSON {
		a.note("baking %s on the node that holds its disk (job %s)", jobSpec.Version, jobID)
	}
	alloc, err := a.waitForJobDone(ctx, jobID, evalID, timeout)
	if err != nil {
		return nil, err
	}
	// The bake is done: the job has nothing left to say.
	if purgeErr := a.Nomad.StopJob(ctx, jobID, true); purgeErr != nil && !isNotFoundErr(purgeErr) {
		a.note("note: the bake job %s could not be purged (%s); `nomad job stop -purge %s` removes it",
			jobID, oneLine(purgeErr.Error(), 120), jobID)
	}
	if failed := allocFailureText(alloc); failed != "" && !allocSucceeded(alloc) {
		return nil, errorf("the bake of %s failed: %s (`nomad alloc logs` of %s has the rest; the box is still there)",
			jobSpec.Version, failed, shortID(alloc.ID))
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

// waitForJobDone waits until a batch job has no live allocation, and returns its
// newest allocation with the task events that say how it ended.
func (a *App) waitForJobDone(ctx context.Context, jobID, evalID string, timeout time.Duration) (*api.Allocation, error) {
	deadline := a.now().Add(timeout)
	last := "Nomad has not placed it yet"
	for {
		byJob, err := a.jobAllocations(ctx)
		if err != nil {
			return nil, wrapf(err, "cannot list the allocations of %s", jobID)
		}
		newest := latestAllocation(byJob[jobID])
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
func (a *App) rebuildImage(ctx context.Context, named, base string, yes bool, timeout time.Duration) error {
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
	if yes {
		a.note("note: a rebuild needs no --yes: the only box it seals is its own temporary one")
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
	baked, err := a.bakeSealed(ctx, created.Box, spec, source.Name, version, []byte(script), timeout)
	if err != nil {
		return keep(err)
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
		if draft == draft && (draft.Version == named || draft.Name == named) {
			add(draft)
		}
	}
	if len(byVersion) == 0 {
		return nil, notFoundf("no image is named %q: `box images` lists the published images and the drafts of the nodes, "+
			"and `box bake <id> --name %s --yes` makes the first one", named, named)
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
	if out.Box != "" {
		a.note("  box     %s is sealed and stopped; its disk is the image", out.Box)
	}
	a.note("")
	a.note("next:")
	disk := "8"
	if out.VirtualBytes > 0 {
		disk = fmt.Sprintf("%d", gibOf(out.VirtualBytes))
	}
	a.note("  box create <id> --image %s --arch %s --disk-gib %s   (on node %s, no download)",
		out.Version, out.Arch, disk, dash(out.Node))
	if out.Box != "" {
		a.note("  box rm %s --yes                              (the sealed box: starting it gives an empty, "+
			"clean machine, not this work)", out.Box)
	}
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
