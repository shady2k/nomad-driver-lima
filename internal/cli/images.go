package cli

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ErrIndexMissing reports that the image index has not been published yet.
//
// Phase 1 creates the index; until then the URL answers 404. That is not a
// failure of the cluster, so the callers say so in plain words.
var ErrIndexMissing = errors.New("the image index is not published yet")

// ImageArchitectures are the architectures an image is published for, in the
// spelling of the image index.
var ImageArchitectures = []string{"amd64", "arm64"}

// ImageArtifact is one published image: one version for one architecture.
type ImageArtifact struct {
	Version string `json:"version"`
	Arch    string `json:"arch"`
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
	BuiltAt string `json:"built_at,omitempty"`
}

// ImageIndex is the parsed index.json: every version, every architecture.
type ImageIndex struct {
	Schema    int
	UpdatedAt string
	// Images maps a version to its artifacts per architecture.
	Images map[string]map[string]*ImageArtifact
}

// ImageSource fetches the index document. The integer is the HTTP status.
type ImageSource interface {
	Fetch(ctx context.Context, url string) ([]byte, int, error)
}

// HTTPImageSource fetches the index over HTTP(S).
type HTTPImageSource struct {
	// Client is the HTTP client; nil means a 30 second timeout client.
	Client *http.Client
}

// Fetch implements ImageSource.
func (h *HTTPImageSource) Fetch(ctx context.Context, url string) ([]byte, int, error) {
	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

// LoadImageIndex fetches and parses the index. A missing index returns
// ErrIndexMissing; any other failure is wrapped.
func LoadImageIndex(ctx context.Context, src ImageSource, url string) (*ImageIndex, error) {
	if src == nil {
		return nil, errorf("no image source is configured")
	}
	body, status, err := src.Fetch(ctx, url)
	if err != nil {
		return nil, wrapf(err, "cannot read the image index %s", url)
	}
	switch status {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, fmt.Errorf("%w (HTTP 404 from %s)", ErrIndexMissing, url)
	default:
		return nil, errorf("cannot read the image index %s: the server answered HTTP %d", url, status)
	}
	index, err := ParseImageIndex(body)
	if err != nil {
		return nil, wrapf(err, "the image index %s is not readable", url)
	}
	return index, nil
}

// ParseImageIndex parses the index document.
//
// The canonical shape (docs/guest-image.md) is
//
//	{"schema": 1, "images": {"<version>": {"<arch>": {"url","sha256","size",...}}}}
//
// A list shape and an explicit "arches" nesting are accepted too, so a
// differently shaped but equivalent file still works.
func ParseImageIndex(body []byte) (*ImageIndex, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("not a JSON object: %w", err)
	}
	index := &ImageIndex{Images: map[string]map[string]*ImageArtifact{}}
	if raw, ok := doc["schema"]; ok {
		_ = json.Unmarshal(raw, &index.Schema)
	}
	if raw, ok := doc["updated_at"]; ok {
		_ = json.Unmarshal(raw, &index.UpdatedAt)
	}

	raw, ok := doc["images"]
	if !ok {
		raw, ok = doc["versions"]
	}
	if !ok {
		return nil, errors.New(`no "images" object`)
	}

	// {"version": {...}} | {"version": {"arches": {"arch": {...}}}} | [{"version": "...", "arches": {...}}]
	var byVersion map[string]json.RawMessage
	if err := json.Unmarshal(raw, &byVersion); err != nil {
		var list []map[string]json.RawMessage
		if err2 := json.Unmarshal(raw, &list); err2 != nil {
			return nil, fmt.Errorf(`"images" is neither an object of versions nor a list: %w`, err)
		}
		byVersion = map[string]json.RawMessage{}
		for _, entry := range list {
			var version string
			_ = json.Unmarshal(entry["version"], &version)
			if version == "" {
				continue
			}
			rest := map[string]json.RawMessage{}
			for k, v := range entry {
				if k != "version" {
					rest[k] = v
				}
			}
			merged, err := json.Marshal(rest)
			if err != nil {
				return nil, err
			}
			byVersion[version] = merged
		}
	}

	for version, versionRaw := range byVersion {
		arches, err := parseArches(versionRaw)
		if err != nil {
			return nil, fmt.Errorf("version %q: %w", version, err)
		}
		for arch, artifact := range arches {
			artifact.Version = version
			artifact.Arch = arch
			index.Images[version] = ensureArches(index.Images, version)
			index.Images[version][arch] = artifact
		}
	}
	return index, nil
}

func ensureArches(m map[string]map[string]*ImageArtifact, version string) map[string]*ImageArtifact {
	if m[version] == nil {
		m[version] = map[string]*ImageArtifact{}
	}
	return m[version]
}

// parseArches reads either an arch map or an object with an "arches" map.
func parseArches(raw json.RawMessage) (map[string]*ImageArtifact, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("is not a JSON object: %w", err)
	}
	if nested, ok := obj["arches"]; ok {
		var inner map[string]json.RawMessage
		if err := json.Unmarshal(nested, &inner); err == nil {
			obj = inner
		}
	}
	out := map[string]*ImageArtifact{}
	for arch, artifactRaw := range obj {
		var artifact ImageArtifact
		if err := json.Unmarshal(artifactRaw, &artifact); err != nil {
			// A key that is not an artifact (for example "built_at" next to
			// the arches) is skipped, not an error.
			continue
		}
		if artifact.URL == "" && artifact.SHA256 == "" {
			continue
		}
		arch = NormalizeArch(arch)
		out[arch] = &artifact
	}
	if len(out) == 0 {
		return nil, errors.New("lists no image for any architecture")
	}
	return out, nil
}

// Versions returns the versions in the index, newest first.
func (i *ImageIndex) Versions() []string {
	out := make([]string, 0, len(i.Images))
	for v := range i.Images {
		out = append(out, v)
	}
	sort.Slice(out, func(a, b int) bool { return versionLess(out[b], out[a]) })
	return out
}

// Artifacts returns every artifact, newest version first, amd64 before arm64.
func (i *ImageIndex) Artifacts() []*ImageArtifact {
	var out []*ImageArtifact
	for _, version := range i.Versions() {
		for _, arch := range ImageArchitectures {
			if a := i.Images[version][arch]; a != nil {
				out = append(out, a)
			}
		}
		for arch, a := range i.Images[version] {
			if arch != "amd64" && arch != "arm64" {
				out = append(out, a)
			}
		}
	}
	return out
}

// Resolve returns the artifact of one version and architecture.
//
// The version "latest" means the newest version that has this architecture.
func (i *ImageIndex) Resolve(version, arch string) (*ImageArtifact, error) {
	arch = NormalizeArch(arch)
	if version == "latest" || version == "" {
		for _, v := range i.Versions() {
			if a := i.Images[v][arch]; a != nil {
				return a, nil
			}
		}
		if len(i.Images) == 0 {
			return nil, errorf("the image index lists no images yet")
		}
		return nil, notFoundf("no image is published for architecture %s; published versions: %s",
			arch, strings.Join(i.Versions(), ", "))
	}

	arches, ok := i.Images[version]
	if !ok {
		return nil, notFoundf("there is no image version %q; published versions: %s",
			version, strings.Join(i.Versions(), ", "))
	}
	artifact, ok := arches[arch]
	if !ok {
		have := make([]string, 0, len(arches))
		for a := range arches {
			have = append(have, a)
		}
		sort.Strings(have)
		return nil, notFoundf("image %s is not published for architecture %s; it has: %s",
			version, arch, strings.Join(have, ", "))
	}
	return artifact, nil
}

// Validate checks that an artifact can be used to create a box.
func (a *ImageArtifact) Validate() error {
	if a.URL == "" {
		return errorf("the image %s/%s has no url in the image index", a.Version, a.Arch)
	}
	sum := strings.TrimSpace(a.SHA256)
	if len(sum) >= len("sha256:") && strings.EqualFold(sum[:len("sha256:")], "sha256:") {
		sum = sum[len("sha256:"):]
	}
	sum = strings.ToLower(sum)
	if sum == "" {
		return errorf("the image %s/%s has no sha256 in the image index", a.Version, a.Arch)
	}
	if len(sum) != 64 {
		return errorf("the image %s/%s has a sha256 of %d characters, want 64", a.Version, a.Arch, len(sum))
	}
	if _, err := hex.DecodeString(sum); err != nil {
		return errorf("the image %s/%s has a sha256 that is not hexadecimal", a.Version, a.Arch)
	}
	a.SHA256 = sum
	return nil
}

// NormalizeArch maps the spellings of an architecture onto amd64 and arm64.
func NormalizeArch(arch string) string {
	switch strings.ToLower(strings.TrimSpace(arch)) {
	case "amd64", "x86_64", "x86-64", "x64":
		return "amd64"
	case "arm64", "aarch64", "arm64v8":
		return "arm64"
	default:
		return strings.ToLower(strings.TrimSpace(arch))
	}
}

// versionLess orders version strings: numeric parts compare as numbers, so
// 1.10.0 comes after 1.9.0.
func versionLess(a, b string) bool {
	as, bs := versionParts(a), versionParts(b)
	for i := 0; i < len(as) && i < len(bs); i++ {
		if as[i] == bs[i] {
			continue
		}
		an, aerr := strconv.Atoi(as[i])
		bn, berr := strconv.Atoi(bs[i])
		if aerr == nil && berr == nil {
			return an < bn
		}
		if aerr == nil {
			return true
		}
		if berr == nil {
			return false
		}
		return as[i] < bs[i]
	}
	return len(as) < len(bs)
}

func versionParts(v string) []string {
	return strings.FieldsFunc(v, func(r rune) bool { return r == '.' || r == '-' || r == '_' })
}
