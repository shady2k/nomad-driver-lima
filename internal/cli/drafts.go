package cli

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/shady2k/nomad-driver-lima/internal/box"
)

// A project image is an image an agent made from a box: it takes a base image,
// installs what its project needs, and the sealed disk becomes an image with the
// dependencies and the caches and without the code, the sessions or the
// credentials.
//
// Such an image starts as a draft: the node that baked it keeps the file in its
// image cache, named by digest, and its record beside it. The driver publishes
// that record as node attributes (driver.box.baked.<version>.<field>), which is
// how a coordinator sees the drafts of every node without touching a disk. A draft
// is created from on its own node, and publishing it into the operator's store is
// what makes it available to every node.

// DraftURLPrefix is the url a draft carries in a job. It is never fetched: the box
// driver finds the image in the node's cache by its digest, and a box asked for a
// draft on a node that no longer has it fails and says so.
const DraftURLPrefix = box.DraftURLPrefix

// draftAttributePrefix is the prefix of the driver's per-image attributes
// (internal/driver/imagework.go).
const draftAttributePrefix = "driver.box.baked."

// imageNamePattern is what a project's name may look like.
var imageNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// numberPattern matches a name that looks like a base image's version: a number,
// or a semver-shaped string.
var numberPattern = regexp.MustCompile(`^v?[0-9]+(\.[0-9]+)*(-[0-9A-Za-z.-]+)?$`)

// ValidateImageName refuses a name that is not lower-case letters, digits and
// dashes, and one that looks like a base image's version: a project image's
// version is <name>-<number>, and the two must not be confused.
func ValidateImageName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return usagef("an image needs a name: --name gives the project it belongs to, for example --name my-project")
	}
	if numberPattern.MatchString(name) {
		return usagef("the image name %q looks like a base image's version; name the project instead, "+
			"for example --name project (its image is then project-1)", name)
	}
	if !imageNamePattern.MatchString(name) || len(name) > 63 {
		return usagef("the image name %q is not usable: use 1 to 63 lower-case letters, digits and dashes, "+
			"starting and ending with a letter or a digit", name)
	}
	return nil
}

// ValidateImageVersion checks a version of a project image: <name>-<number>.
func ValidateImageVersion(version, name string) error {
	if version == "" {
		return usagef("an image version is required")
	}
	gotName, n, ok := ParseImageVersion(version)
	if !ok {
		return usagef("the image version %q is not a project image's version: they are <name>-<number>, "+
			"for example project-1", version)
	}
	if name != "" && gotName != name {
		return usagef("the image version %q belongs to %q, not to %q", version, gotName, name)
	}
	if n < 1 {
		return usagef("the image version %q has a number below 1", version)
	}
	return nil
}

// ParseImageVersion splits a project image's version into its name and its number:
// "project-3" is the project "project", version 3.
func ParseImageVersion(version string) (name string, n int, ok bool) {
	i := strings.LastIndex(version, "-")
	if i <= 0 || i == len(version)-1 {
		return "", 0, false
	}
	name, digits := version[:i], version[i+1:]
	if !imageNamePattern.MatchString(name) {
		return "", 0, false
	}
	value, err := strconv.Atoi(digits)
	if err != nil || value < 0 {
		return "", 0, false
	}
	return name, value, true
}

// IsProjectImage reports whether a version names a project image.
func IsProjectImage(version string) bool {
	_, _, ok := ParseImageVersion(version)
	return ok
}

// NextImageVersion is the version a new bake of name gets: NAME-N, with N one
// above the highest N among the published versions of that name and the drafts on
// the nodes.
func NextImageVersion(name string, versions []string) (string, error) {
	highest := 0
	for _, version := range versions {
		gotName, n, ok := ParseImageVersion(version)
		if !ok || gotName != name {
			continue
		}
		if n > highest {
			highest = n
		}
	}
	return name + "-" + strconv.Itoa(highest+1), nil
}

// DraftImage is a project image that exists on one node: the node baked it into
// its image cache, and the driver's fingerprint says what it is.
type DraftImage struct {
	Version      string `json:"version"`
	Arch         string `json:"arch"`
	Name         string `json:"name"`
	Parent       string `json:"parent,omitempty"`
	SHA256       string `json:"sha256"`
	Size         int64  `json:"size"`
	VirtualBytes int64  `json:"virtual_bytes,omitempty"`
	BuiltAt      string `json:"built_at,omitempty"`
	Recipe       string `json:"recipe,omitempty"`
	Platform     string `json:"platform,omitempty"`
	// NodeID and NodeName are the node that holds the file.
	NodeID   string `json:"node_id"`
	NodeName string `json:"node_name,omitempty"`
}

// URL is the url a job created from this draft carries. It is never fetched: the
// cache of the node holds the image, and its digest is the record's.
func (d DraftImage) URL() string { return DraftURLPrefix + d.Version }

// Artifact turns a draft into the artifact `box create` works from: the same
// fields an index entry has, so a draft and a published project image take one
// path.
func (d DraftImage) Artifact() *ImageArtifact {
	return &ImageArtifact{
		Version:      d.Version,
		Arch:         d.Arch,
		URL:          d.URL(),
		SHA256:       d.SHA256,
		Size:         d.Size,
		BuiltAt:      d.BuiltAt,
		Platform:     d.Platform,
		Kind:         "project",
		Name:         d.Name,
		Parent:       d.Parent,
		Recipe:       d.Recipe,
		VirtualBytes: d.VirtualBytes,
		NodeID:       d.NodeID,
		NodeName:     d.NodeName,
	}
}

// draftsFromAttributes reads the drafts a node published: one attribute per field
// of the record, under driver.box.baked.<version>.<field>.
func draftsFromAttributes(nodeID, nodeName string, attrs map[string]string) []DraftImage {
	byVersion := map[string]*DraftImage{}
	var order []string
	for key, value := range attrs {
		if !strings.HasPrefix(key, draftAttributePrefix) {
			continue
		}
		rest := strings.TrimPrefix(key, draftAttributePrefix)
		// The version may hold dashes but never a dot, and a field never holds
		// one: the last dot is where the field starts.
		dot := strings.LastIndex(rest, ".")
		if dot <= 0 || dot == len(rest)-1 {
			continue
		}
		version, field := rest[:dot], rest[dot+1:]
		// An empty value is the driver clearing the key of an image that is gone.
		if field == "sha256" && value == "" {
			delete(byVersion, version)
			continue
		}
		draft, ok := byVersion[version]
		if !ok {
			draft = &DraftImage{Version: version, NodeID: nodeID, NodeName: nodeName}
			byVersion[version] = draft
			order = append(order, version)
		}
		switch field {
		case "arch":
			draft.Arch = NormalizeArch(value)
		case "name":
			draft.Name = value
		case "parent":
			draft.Parent = value
		case "sha256":
			draft.SHA256 = value
		case "size":
			draft.Size, _ = strconv.ParseInt(value, 10, 64)
		case "virtual_bytes":
			draft.VirtualBytes, _ = strconv.ParseInt(value, 10, 64)
		case "built_at":
			draft.BuiltAt = value
		case "recipe":
			draft.Recipe = value
		case "platform":
			draft.Platform = value
		}
	}
	sort.Strings(order)
	out := make([]DraftImage, 0, len(order))
	for _, version := range order {
		draft := byVersion[version]
		if draft.Name == "" || draft.SHA256 == "" || draft.Arch == "" {
			// Half a record is no draft: a reader that offered it would fail at
			// the create.
			continue
		}
		out = append(out, *draft)
	}
	return out
}

// nodeDrafts reads the project images every node baked.
func (a *App) nodeDrafts(ctx context.Context) ([]DraftImage, error) {
	stubs, err := a.Nomad.NodeList(ctx)
	if err != nil {
		return nil, wrapf(err, "cannot list the nodes")
	}
	var out []DraftImage
	for _, stub := range stubs {
		if stub == nil {
			continue
		}
		node, err := a.Nomad.NodeInfo(ctx, stub.ID)
		if err != nil {
			// A node that cannot be described is one node's drafts, not a
			// failure of the command.
			continue
		}
		out = append(out, draftsFromAttributes(stub.ID, stub.Name, nodeAttributes(node))...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Version != out[j].Version {
			return versionLess(out[j].Version, out[i].Version)
		}
		return out[i].Arch < out[j].Arch
	})
	return out, nil
}

// findDraft returns the draft of one version and architecture, or a not-found
// error that names what there is.
func findDraft(drafts []DraftImage, version, arch string) (*DraftImage, error) {
	arch = NormalizeArch(arch)
	have := make([]string, 0, len(drafts))
	for _, draft := range drafts {
		have = append(have, draft.Version+"/"+draft.Arch+" on "+draft.NodeName)
	}
	sort.Strings(have)
	for i := range drafts {
		if drafts[i].Version != version {
			continue
		}
		if arch == "" || drafts[i].Arch == arch {
			return &drafts[i], nil
		}
	}
	if len(have) == 0 {
		return nil, notFoundf("no node holds a draft of %s; `box bake` makes one, and `box images` lists every draft", version)
	}
	return nil, notFoundf("no node holds a draft of %s for architecture %s; the drafts are: %s",
		version, arch, strings.Join(have, ", "))
}

// flagSet reports whether a flag was given on the command line, so that "the
// default" and "the value the operator asked for" can be told apart.
func flagSet(fs *flag.FlagSet, name string) bool {
	given := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			given = true
		}
	})
	return given
}

// resolveImageVersion turns --image into the version to create from: an exact
// version of a published image or a draft, or a project's name, which means its
// newest version.
func (a *App) resolveImageVersion(requested string, index *ImageIndex, drafts []DraftImage) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return "", usagef("--image is required; `box images` lists the published images and the drafts of the nodes")
	}
	if index != nil {
		if _, ok := index.Images[requested]; ok {
			return requested, nil
		}
	}
	for _, draft := range drafts {
		if draft.Version == requested {
			return requested, nil
		}
	}
	// A project's name is its newest version: `--image my-project` is
	// `--image my-project-3` when 3 is the newest.
	if imageNamePattern.MatchString(requested) && !numberPattern.MatchString(requested) &&
		!strings.Contains(requested, ".") && requested != "latest" {
		if version, ok := newestProjectVersion(requested, index, drafts); ok {
			return version, nil
		}
	}
	return requested, nil
}

// newestProjectVersion finds the newest version of one project among the published
// images and the drafts.
func newestProjectVersion(name string, index *ImageIndex, drafts []DraftImage) (string, bool) {
	newest := ""
	consider := func(version, project string) {
		if project != name {
			return
		}
		if newest == "" || versionLess(newest, version) {
			newest = version
		}
	}
	if index != nil {
		for _, version := range index.Versions() {
			for _, arch := range ImageArchitectures {
				if artifact := index.Images[version][arch]; artifact != nil {
					consider(version, artifact.Name)
					if !artifact.IsProject() {
						// An index entry without a name still says it is a project
						// by its version.
						consider(version, name)
					}
				}
			}
		}
	}
	for _, draft := range drafts {
		consider(draft.Version, draft.Name)
	}
	return newest, newest != ""
}

// resolveImage finds the artifact of a version for one architecture: published in
// the image index, or a draft on a node.
func (a *App) resolveImage(index *ImageIndex, drafts []DraftImage, version, arch string) (*ImageArtifact, error) {
	var indexErr error
	if index != nil {
		if artifact, err := index.Resolve(version, arch); err == nil {
			return artifact, nil
		} else {
			indexErr = err
		}
	}
	draft, draftErr := findDraft(drafts, version, arch)
	if draftErr == nil {
		return draft.Artifact(), nil
	}
	if indexErr != nil {
		return nil, indexErr
	}
	return nil, draftErr
}

// diskForImage is the disk a box made from an image gets: the image's disk is the
// floor, because Lima grows a disk and never shrinks one.
func diskForImage(artifact *ImageArtifact, requested int, given bool) (int, error) {
	floor := int(gibOf(artifact.VirtualBytes))
	if floor <= 0 {
		return requested, nil
	}
	if given && requested < floor {
		return 0, usagef("--disk-gib %d is smaller than the disk inside the image %s, which is %d GiB: the box needs "+
			"at least that much, and Lima never shrinks a disk", requested, artifact.Version, floor)
	}
	if requested < floor {
		return floor, nil
	}
	return requested, nil
}

// draftNode is the node that holds a draft: a box made from a draft runs there,
// because the image is in that node's cache.
func draftNode(nodes []*NodeCapacity, drafts []DraftImage, version, arch, wantName string) (*NodeCapacity, error) {
	draft, err := findDraft(drafts, version, arch)
	if err != nil {
		return nil, err
	}
	var node *NodeCapacity
	for _, candidate := range nodes {
		if candidate.NodeID == draft.NodeID {
			node = candidate
			break
		}
	}
	if node == nil {
		return nil, notFoundf("the draft %s is on node %s, which Nomad does not list: a draft is created from on the "+
			"node that holds it, or published first (`box images publish %s --yes`)",
			version, dash(draft.NodeName), version)
	}
	if wantName != "" && wantName != node.Name && wantName != node.NodeID {
		return nil, usagef("the draft %s is on node %s, and --node %s was asked for: a draft is created from on the node "+
			"that holds it", version, node.Name, wantName)
	}
	if kind, problem := node.nodeRefusal(PlacementRequest{Arch: arch}); problem != "" {
		refused := errorf("the node %s holds the draft %s but cannot run the box: %s", node.Name, version, problem)
		if kind == refusalRoom {
			// A draft runs on the node that holds it, so a full node is the one
			// thing that changes by itself: `--wait` asks again.
			return nil, &noRoomError{cause: refused}
		}
		return nil, refused
	}
	return node, nil
}

// decodeRecipe turns an image's base64 recipe into its script.
func decodeRecipe(encoded string) (string, error) {
	if strings.TrimSpace(encoded) == "" {
		return "", nil
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", usagef("the image's recipe is not base64: %v", err)
	}
	return string(data), nil
}

// encodeRecipe turns a recipe's script into the base64 an image carries.
func encodeRecipe(script []byte) string { return base64.StdEncoding.EncodeToString(script) }

// behindText says how far a project image is behind the newest base image:
// "parent 1.0.8, newest base 1.0.12" is an image that can be rebuilt.
func behindText(parent string, bases []string) string {
	if parent == "" {
		return "-"
	}
	if len(bases) == 0 {
		return "parent " + parent
	}
	newest := bases[0]
	for _, base := range bases[1:] {
		if versionLess(newest, base) {
			newest = base
		}
	}
	if newest == parent {
		return "parent " + parent + " (newest base)"
	}
	if versionLess(parent, newest) {
		return fmt.Sprintf("parent %s, newest base %s", parent, newest)
	}
	return "parent " + parent
}
