package cli

import (
	"context"
	"errors"
)

// imagesOutput is the JSON document of `box images`.
type imagesOutput struct {
	IndexURL  string           `json:"index_url"`
	Available bool             `json:"available"`
	UpdatedAt string           `json:"updated_at,omitempty"`
	Images    []*ImageArtifact `json:"images"`
	Error     string           `json:"error,omitempty"`
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
			return nil
		}
		return err
	}
	images := index.Artifacts()
	if a.JSON {
		return a.printJSON(imagesOutput{IndexURL: url, Available: true, UpdatedAt: index.UpdatedAt, Images: images})
	}
	if len(images) == 0 {
		a.note("the image index %s lists no images yet.", url)
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
	return nil
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
