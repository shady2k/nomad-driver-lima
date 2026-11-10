package cli

import (
	"context"
	"errors"
	"sort"
	"strings"
)

// imagesOutput is the JSON document of `box images`.
type imagesOutput struct {
	IndexURL  string           `json:"index_url"`
	Available bool             `json:"available"`
	UpdatedAt string           `json:"updated_at,omitempty"`
	Images    []*ImageArtifact `json:"images"`
	// Drafts are the project images that exist on one node only: `box bake` made
	// them, and no store has them yet.
	Drafts []DraftImage `json:"drafts,omitempty"`
	// Platforms are the published platform bundles; empty when the index lists
	// none yet.
	Platforms []*PlatformArtifact `json:"platforms,omitempty"`
	Error     string              `json:"error,omitempty"`
}

// cmdImages lists the images a box can be created from: the published base
// images, the published project images, and the drafts the nodes baked.
//
// A missing index is not a failure here: a cluster that bakes images has drafts
// before it has a store.
func (a *App) cmdImages(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "publish" {
		return a.cmdImagesPublish(ctx, args[1:])
	}
	fs := newFlagSet("images")
	if err := parseFlags(fs, args, 0); err != nil {
		return err
	}
	url := a.ImageIndexURL()
	index, indexNote := a.imageIndexOrNote(ctx)
	drafts, draftsErr := a.nodeDrafts(ctx)
	if draftsErr != nil {
		if index == nil {
			return wrapf(draftsErr, "cannot read the project images of the nodes")
		}
		a.note("note: the drafts of the nodes could not be read: %s", draftsErr)
	}

	var (
		bases     []*ImageArtifact
		projects  []*ImageArtifact
		platforms []*PlatformArtifact
		updated   string
	)
	if index != nil {
		updated = index.UpdatedAt
		platforms = index.PlatformArtifacts()
		for _, image := range index.Artifacts() {
			if image.IsProject() {
				projects = append(projects, image)
				continue
			}
			bases = append(bases, image)
		}
	}
	// A published image stays in the cache of the node that baked it, so its
	// draft record outlives the publish: show the image once, as published.
	drafts = prunePublishedDrafts(drafts, append(append([]*ImageArtifact{}, bases...), projects...))
	if a.JSON {
		out := imagesOutput{IndexURL: url, Available: index != nil, UpdatedAt: updated, Drafts: drafts,
			Images: append(append([]*ImageArtifact{}, bases...), projects...), Platforms: platforms, Error: indexNote}
		return a.printJSON(out)
	}
	if indexNote != "" {
		a.note("%s", indexNote)
	}
	if len(bases) == 0 && len(projects) == 0 && len(drafts) == 0 {
		a.note("this cluster has no image yet: `box bake <id> --name NAME --yes` makes a draft on the box's node, " +
			"and publishing an image writes the index")
		a.printPlatforms(platforms, url)
		return nil
	}

	newestBase := ""
	if len(bases) > 0 {
		newestBase = bases[0].Version
		for _, base := range bases[1:] {
			if versionLess(newestBase, base.Version) {
				newestBase = base.Version
			}
		}
	}
	basesVersions := make([]string, 0, len(bases))
	for _, base := range bases {
		basesVersions = append(basesVersions, base.Version)
	}
	rows := make([][]string, 0, len(bases)+len(projects)+len(drafts))
	for _, image := range bases {
		rows = append(rows, []string{
			image.Version, image.Arch, "base", "-", "-",
			humanBytes(image.Size), shortSHA(image.SHA256), "published",
		})
	}
	for _, image := range projects {
		rows = append(rows, []string{
			image.Version, image.Arch, "project", dash(image.Parent), behindText(image.Parent, basesVersions),
			humanBytes(image.Size), shortSHA(image.SHA256), "published",
		})
	}
	for _, draft := range drafts {
		rows = append(rows, []string{
			draft.Version, draft.Arch, "project", dash(draft.Parent), behindText(draft.Parent, basesVersions),
			humanBytes(draft.Size), shortSHA(draft.SHA256), "draft on " + dash(draft.NodeName),
		})
	}
	if hasMismatchedColumns(rows) {
		rows = padToSameLength(rows)
	}
	a.printTable([]string{"VERSION", "ARCH", "KIND", "PARENT", "BEHIND", "SIZE", "SHA256", "WHERE"}, rows)
	a.note("%s, %s and %s from %s",
		describeCount(len(bases), "base image"), describeCount(len(projects), "published project image"),
		describeCount(len(drafts), "draft"), dash(url))
	if newestBase != "" {
		a.note("choose the size for the task: %d vCPU and %d MiB are for light work without a build, and a build "+
			"or a test run needs more memory from the start", defaultCPUs, defaultMemoryMiB)
		a.note("when in doubt take more: a guest out of memory just stops answering (`box top` and `box status` " +
			"go silent); `box resize --memory-mib` grows it between turns, and it restarts with the disk kept")
		a.note("the node is shared, so give back what you do not use: `box rm <id> --yes` a finished box; " +
			"`box stop` is only a pause, and the disk goes when its term ends (24 hours unless the operator set another)")
	}
	if len(drafts) > 0 {
		a.note("a draft is created from on its own node: box create <id> --image %s", drafts[0].Version)
		a.note("publishing a draft makes it available on every node: box images publish %s --yes", drafts[0].Version)
	}
	if len(projects) > 0 || len(drafts) > 0 {
		for _, image := range append(projects, draftArtifacts(drafts)...) {
			if image.Parent != "" && newestBase != "" && versionLess(image.Parent, newestBase) {
				a.note("%s was grown from %s and can be rebuilt on %s: box bake --rebuild %s",
					image.Version, image.Parent, newestBase, image.Name)
				break
			}
		}
	}
	a.note("the disk holds the image, the toolchains and the checkout: %d GiB fits a small repository, a real "+
		"project wants 20-40 GiB;", defaultDiskGiB)
	a.note("the host spends only what the box writes, and box resize --disk-gib grows the disk later (it never shrinks)")
	a.printPlatforms(platforms, url)
	// The hint is the last line, after the bundle table: a reader who looks at the
	// end of the output must see the image to create from, not a bundle version.
	if newestBase != "" {
		a.note("create a box with: box create <id> --image %s --arch %s --cpus %d --memory-mib %d --disk-gib %d",
			newestBase, archOfFirst(bases), defaultCPUs, defaultMemoryMiB, defaultDiskGiB)
	}
	return nil
}

// imageIndexOrNote reads the index and returns what to say when there is none.
func (a *App) imageIndexOrNote(ctx context.Context) (*ImageIndex, string) {
	url, err := a.requireImageIndexURL()
	if err != nil {
		return nil, "the image index is not configured: set BOX_IMAGE_INDEX (or pass --image-index URL) to the " +
			"index.json of a store that publishes box images; only the drafts of the nodes are listed below"
	}
	index, err := LoadImageIndex(ctx, a.Images, url)
	if err != nil {
		if errors.Is(err, ErrIndexMissing) {
			return nil, "no base images are published yet: the image index " + url + " is not published yet " +
				"(it answers 404). Publishing an image writes it; only the drafts of the nodes are listed below"
		}
		return nil, "the image index " + url + " could not be read: " + oneLine(err.Error(), 200)
	}
	return index, ""
}

// draftArtifacts turns drafts into artifacts, for the hints.
func draftArtifacts(drafts []DraftImage) []*ImageArtifact {
	out := make([]*ImageArtifact, 0, len(drafts))
	for i := range drafts {
		out = append(out, drafts[i].Artifact())
	}
	return out
}

// archOfFirst is the architecture of the first artifact, or the node's.
func archOfFirst(images []*ImageArtifact) string {
	if len(images) > 0 && images[0].Arch != "" {
		return images[0].Arch
	}
	return "amd64"
}

// hasMismatchedColumns reports whether the rows disagree on their width, which a
// table cannot print.
func hasMismatchedColumns(rows [][]string) bool {
	if len(rows) == 0 {
		return false
	}
	for _, row := range rows {
		if len(row) != len(rows[0]) {
			return true
		}
	}
	return false
}

// padToSameLength pads every row to the widest one, keeping the columns apart.
func padToSameLength(rows [][]string) [][]string {
	width := 0
	for _, row := range rows {
		if len(row) > width {
			width = len(row)
		}
	}
	out := make([][]string, 0, len(rows))
	for _, row := range rows {
		padded := append([]string(nil), row...)
		for len(padded) < width {
			padded = append(padded, "-")
		}
		out = append(out, padded)
	}
	return out
}

// publishOutput is the JSON document of `box images publish`.
type publishOutput struct {
	Version string `json:"version"`
	Arch    string `json:"arch"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size,omitempty"`
	Store   string `json:"store,omitempty"`
	URL     string `json:"url,omitempty"`
	Index   string `json:"index_url,omitempty"`
	Node    string `json:"node,omitempty"`
}

// cmdImagesPublish publishes a draft into the operator's store.
//
// Publishing distributes an image, so it changes what every node can create a box
// from: an agent asks its owner first, and the command refuses without --yes.
func (a *App) cmdImagesPublish(ctx context.Context, args []string) error {
	fs := newFlagSet("images publish")
	yes := fs.Bool("yes", false, "publish without asking: every node can then create a box from the image")
	timeout := fs.Duration("timeout", DefaultBakeTimeout, "how long to wait for the upload")
	if err := parseFlags(fs, args, 1); err != nil {
		return err
	}
	version := fs.Arg(0)
	if err := ValidateImageVersion(version, ""); err != nil {
		return err
	}
	if !*yes {
		return errorf("publishing %s distributes it: every node of the cluster can create a box from it, and a "+
			"published version never changes. Ask its owner before publishing, then run `box images publish %s --yes`",
			version, version)
	}
	drafts, err := a.nodeDrafts(ctx)
	if err != nil {
		return wrapf(err, "cannot read the project images of the nodes")
	}
	draft, err := findDraft(drafts, version, "")
	if err != nil {
		return err
	}
	store, err := a.Nomad.VariableRead(ctx, boxStoreVariable)
	if err != nil && !isNotFoundErr(err) {
		return wrapf(err, "cannot read the variable %s", boxStoreVariable)
	}
	if store == nil || strings.TrimSpace(store.Items["url"]) == "" {
		return errorf("this cluster has no image store: `box store set --url URL --from-file FILE` writes the "+
			"variable %s, which a publish reads its url and its token from", boxStoreVariable)
	}
	items := storeItems(store)
	job, err := RenderPublishJob(PublishJobSpec{
		Version:    draft.Version,
		Arch:       draft.Arch,
		SHA256:     draft.SHA256,
		NodeID:     draft.NodeID,
		StoreItems: items,
	})
	if err != nil {
		return err
	}
	jobID := derefStr(job.ID, "")
	if !a.JSON {
		a.note("publishing %s (%s, sha256 %s) from node %s", draft.Version, draft.Arch,
			shortSHA(draft.SHA256), dash(draft.NodeName))
	}
	evalID, since, err := a.registerWorkJob(ctx, job)
	if err != nil {
		return wrapf(err, "Nomad rejected the publish job %s", jobID)
	}
	alloc, err := a.waitForJobDone(ctx, jobID, evalID, since, *timeout)
	if err != nil {
		return err
	}
	if purgeErr := a.Nomad.StopJob(ctx, jobID, true); purgeErr != nil && !isNotFoundErr(purgeErr) {
		a.note("note: the publish job %s could not be purged (%s); `nomad job stop -purge %s` removes it",
			jobID, oneLine(purgeErr.Error(), 120), jobID)
	}
	if failed := allocFailureText(alloc); failed != "" && !allocSucceeded(alloc) {
		return errorf("publishing %s failed: %s (`nomad alloc logs` of %s has the rest)", draft.Version, failed, shortID(alloc.ID))
	}
	details, ok := publishResultOf(alloc)
	if !ok {
		return errorf("the publish job %s ended without saying where the image went; `nomad job status %s` shows its "+
			"events", jobID, jobID)
	}
	out := publishOutput{
		Version: draft.Version,
		Arch:    draft.Arch,
		SHA256:  draft.SHA256,
		Size:    draft.Size,
		Store:   strings.TrimRight(store.Items["url"], "/"),
		Node:    alloc.NodeName,
		Index:   details["index"],
	}
	out.URL = out.Store + "/box-base/" + draft.Version + "/" + draft.Arch + "/box-base-" + draft.Version + "-" + draft.Arch + ".qcow2"
	if a.JSON {
		return a.printJSON(out)
	}
	a.note("published %s (%s) to %s", out.Version, out.Arch, out.Store)
	a.note("  sha256  %s", out.SHA256)
	a.note("  image   %s", out.URL)
	a.note("  index   %s", out.Index)
	a.note("")
	a.note("every node can create a box from it now: box create <id> --image %s --arch %s", out.Version, out.Arch)
	return nil
}

// printPlatforms writes the published platform bundles, or one line that says
// there are none yet.
//
// The bundle holds boxd, boxctl and the executors, and a node mounts it into every
// box whose image takes one; an image that carries its own boxd needs no bundle, so
// an index without platforms is a working index.
func (a *App) printPlatforms(platforms []*PlatformArtifact, url string) {
	if len(platforms) == 0 {
		a.note("no platform bundles are published yet: every image runs the boxd it carries.")
		return
	}
	a.note("the rows below are platform bundles (boxd and the executors), mounted into a box by the driver; " +
		"they are not images for --image:")
	rows := make([][]string, 0, len(platforms))
	for _, bundle := range platforms {
		rows = append(rows, []string{
			bundle.Version,
			bundle.Arch,
			humanBytes(bundle.Size),
			shortSHA(bundle.SHA256),
			dash(bundle.Boxd),
			dash(toolVersionsLine(bundle.Tools)),
			dash(bundle.BuiltAt),
		})
	}
	a.printTable([]string{"BUNDLE", "ARCH", "SIZE", "SHA256", "BOXD", "TOOLS", "BUILT"}, rows)
	a.note("%s from %s", describeCount(len(platforms), "platform bundle"), url)
}

// toolVersionsLine renders the tools of a bundle as "name version" pairs, sorted
// by name.
func toolVersionsLine(tools map[string]string) string {
	if len(tools) == 0 {
		return ""
	}
	names := make([]string, 0, len(tools))
	for name := range tools {
		names = append(names, name)
	}
	sort.Strings(names)
	pairs := make([]string, 0, len(names))
	for _, name := range names {
		pairs = append(pairs, name+" "+tools[name])
	}
	return strings.Join(pairs, ", ")
}

// shortSHA shortens a sha256 for a table.
func shortSHA(sum string) string {
	if len(sum) <= 12 {
		if sum == "" {
			return "-"
		}
		return sum
	}
	return sum[:12]
}

// prunePublishedDrafts drops the drafts a store already publishes: an image and
// architecture that is in the index is created from there, and a second row for
// the same pair says the image is in two places at once, which it is not.
func prunePublishedDrafts(drafts []DraftImage, published []*ImageArtifact) []DraftImage {
	if len(drafts) == 0 || len(published) == 0 {
		return drafts
	}
	type key struct{ version, arch string }
	have := make(map[key]bool, len(published))
	for _, artifact := range published {
		if artifact == nil {
			continue
		}
		have[key{artifact.Version, NormalizeArch(artifact.Arch)}] = true
	}
	out := make([]DraftImage, 0, len(drafts))
	for _, draft := range drafts {
		if have[key{draft.Version, NormalizeArch(draft.Arch)}] {
			continue
		}
		out = append(out, draft)
	}
	return out
}
