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
	// Platforms are the published platform bundles; empty when the index lists
	// none yet.
	Platforms []*PlatformArtifact `json:"platforms,omitempty"`
	Error     string              `json:"error,omitempty"`
}

// cmdImages lists the published base images.
//
// A missing index is not an error: phase 1 publishes it, and until then there
// are simply no images yet.
func (a *App) cmdImages(ctx context.Context, args []string) error {
	fs := newFlagSet("images")
	if err := parseFlags(fs, args, 0); err != nil {
		return err
	}
	url, err := a.requireImageIndexURL()
	if err != nil {
		return err
	}
	index, err := LoadImageIndex(ctx, a.Images, url)
	if err != nil {
		if errors.Is(err, ErrIndexMissing) {
			if a.JSON {
				return a.printJSON(imagesOutput{IndexURL: url, Available: false, Images: []*ImageArtifact{},
					Error: "the image index is not published yet (HTTP 404)"})
			}
			a.note("no base images are published yet: the image index %s answers 404.", url)
			a.note("publishing an image writes it; `box create --image <version>` needs it.")
			a.printPlatforms(nil, url)
			return nil
		}
		return err
	}
	images := index.Artifacts()
	platforms := index.PlatformArtifacts()
	if a.JSON {
		return a.printJSON(imagesOutput{IndexURL: url, Available: true, UpdatedAt: index.UpdatedAt, Images: images,
			Platforms: platforms})
	}
	if len(images) == 0 {
		a.note("the image index %s lists no images yet.", url)
		a.printPlatforms(platforms, url)
		return nil
	}
	rows := make([][]string, 0, len(images))
	for _, image := range images {
		rows = append(rows, []string{
			image.Version,
			image.Arch,
			humanBytes(image.Size),
			shortSHA(image.SHA256),
			dash(image.BuiltAt),
		})
	}
	a.printTable([]string{"VERSION", "ARCH", "SIZE", "SHA256", "BUILT"}, rows)
	a.note("%s from %s", describeCount(len(images), "image"), url)
	a.note("create a box with: box create <id> --image %s --arch %s --cpus %d --memory-mib %d --disk-gib %d",
		images[0].Version, images[0].Arch, defaultCPUs, defaultMemoryMiB, defaultDiskGiB)
	a.note("the disk holds the image, the toolchains and the checkout: %d GiB fits a small repository, a real project wants 20-40 GiB;",
		defaultDiskGiB)
	a.note("the host spends only what the box writes, and box resize --disk-gib grows the disk later (it never shrinks)")
	a.printPlatforms(platforms, url)
	return nil
}

// printPlatforms writes the published platform bundles, or one line that says
// there are none yet.
//
// The bundle holds boxd, boxctl and the executors, and a node mounts it into
// every box whose image takes one; an image that carries its own boxd needs no
// bundle, so an index without platforms is a working index.
func (a *App) printPlatforms(platforms []*PlatformArtifact, url string) {
	if len(platforms) == 0 {
		a.note("no platform bundles are published yet: every image runs the boxd it carries.")
		return
	}
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
	a.printTable([]string{"VERSION", "ARCH", "SIZE", "SHA256", "BOXD", "TOOLS", "BUILT"}, rows)
	a.note("%s from %s", describeCount(len(platforms), "platform bundle"), url)
}

// toolVersionsLine renders the tools of a bundle as "name version" pairs,
// sorted by name.
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
