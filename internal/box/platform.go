package box

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/shady2k/nomad-driver-lima/internal/limactl"
)

// The platform bundle is the fast-moving part of a box: boxd, boxctl and the
// executors, versioned as one archive and mounted read-only into every guest at
// /opt/box (the way Lima itself delivers lima-guestagent through cidata instead
// of baking it into the image). The image keeps the OS and the toolchains, so a
// boxd or executor change needs no new image and no rebuilt derived image.
//
// On the host one bundle is one unpacked directory per sha256:
//
//	<ImageCacheDir>/platform/<sha256>/manifest.json
//	<ImageCacheDir>/platform/<sha256>/bin/...
//
// The directory is read-only for the guest and never changes: a new bundle is a
// new directory. A marker file, written after the whole archive was unpacked,
// says the directory is complete, so a start after a crash reuses it instead of
// mounting half an archive.

// platformDirName is the cache subdirectory the bundles live in.
const platformDirName = "platform"

// platformMarker is the file written last into an unpacked bundle. Its content
// is the sha256 of the archive, so a directory is only ever reused for the
// bundle it was unpacked from.
const platformMarker = ".box-platform-complete"

// PlatformSpec is the platform bundle a box mounts at /opt/box. The zero value
// (nil in Spec) means the box runs the boxd its image carries.
type PlatformSpec struct {
	// Version names the bundle for messages ("1.2.0").
	Version string
	// URL and SHA256 locate the archive; the sha256 is verified before the
	// unpack and is the cache key.
	URL    string
	SHA256 string
}

// String names the bundle in a message: its version, or the start of its digest
// when a caller left the version out.
func (p PlatformSpec) String() string {
	if v := strings.TrimSpace(p.Version); v != "" {
		return v
	}
	sum := strings.ToLower(strings.TrimSpace(p.SHA256))
	if len(sum) > 12 {
		sum = sum[:12]
	}
	return "sha256:" + sum
}

// PlatformMountPoint is where the guest sees the bundle: read-only, on every
// boot, from the first second of the VM.
const PlatformMountPoint = "/opt/box"

// NormalizeSHA256 validates a sha256 (with or without a "sha256:" prefix) and
// returns it lower-cased. The driver validates a task's platform block with it,
// so a bad digest is a task configuration error, not a failed download.
func NormalizeSHA256(want string) (string, error) {
	sum, err := normalizeSHA(want)
	if err != nil {
		return "", err
	}
	return sum, nil
}

// normalizeSHA validates a sha256 and returns it lower-cased.
func normalizeSHA(want string) (string, error) {
	sum := strings.ToLower(strings.TrimSpace(want))
	if len(sum) == len("sha256:")+64 && strings.HasPrefix(sum, "sha256:") {
		sum = sum[len("sha256:"):]
	}
	if len(sum) != 64 {
		return "", fmt.Errorf("sha256 must be 64 hex characters, got %q", want)
	}
	if _, err := hex.DecodeString(sum); err != nil {
		return "", fmt.Errorf("sha256 %q is not hexadecimal: %w", want, err)
	}
	return sum, nil
}

// PlatformDir is the host directory a bundle is unpacked into. The sha256 is
// the key, so a changed bundle never shares a directory with the one it
// replaces.
func (m *Manager) PlatformDir(sha256 string) string {
	sum := strings.ToLower(strings.TrimSpace(sha256))
	sum = strings.TrimPrefix(sum, "sha256:")
	return filepath.Join(m.opts.ImageCacheDir, platformDirName, sum)
}

// EnsurePlatform makes the bundle available in the cache and returns its
// directory. A cached, complete directory is reused without a download; a new
// archive is downloaded to a temporary file, verified by sha256, unpacked into a
// temporary directory and renamed into place, so a half download or a half
// unpack is never mounted. The directory belongs to the runtime user, which runs
// qemu and the lima hostagent: it has to read every file of it.
func (b *Box) EnsurePlatform(ctx context.Context, spec PlatformSpec) (string, error) {
	want, err := normalizeSHA(spec.SHA256)
	if err != nil {
		return "", fmt.Errorf("platform %s: %w", spec.String(), err)
	}
	if spec.URL == "" {
		return "", fmt.Errorf("platform %s has no url", spec.String())
	}
	root := filepath.Join(b.m.opts.ImageCacheDir, platformDirName)
	if err := b.m.opts.Identity.EnsureDir(root, 0o755); err != nil {
		return "", fmt.Errorf("create platform cache %s: %w", root, err)
	}
	dest := b.m.PlatformDir(want)

	if ok, err := platformComplete(dest, want); err != nil {
		return "", err
	} else if ok {
		b.event("platform", fmt.Sprintf("Using cached platform bundle %s", spec.String()))
		return dest, nil
	}

	b.event("platform", fmt.Sprintf("Downloading platform bundle %s from %s", spec.String(), spec.URL))
	tmp, err := os.CreateTemp(root, want+".*.part")
	if err != nil {
		return "", fmt.Errorf("create temporary platform archive: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()
	sum, size, err := downloadTo(ctx, spec.URL, tmp, func(percent int) {
		b.event("platform", fmt.Sprintf("Platform bundle download %d%%", percent))
	})
	if err != nil {
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		return "", fmt.Errorf("sync platform archive: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close platform archive: %w", err)
	}
	if sum != want {
		return "", fmt.Errorf("platform bundle %s has sha256 %s, want %s", spec.URL, sum, want)
	}

	if err := b.unpackPlatform(tmpName, dest, want); err != nil {
		return "", fmt.Errorf("platform bundle %s: %w", spec.String(), err)
	}
	b.event("platform", fmt.Sprintf("Unpacked platform bundle %s into %s (%s, sha256 verified)", spec.String(), dest, humanBytes(size)))
	return dest, nil
}

// platformComplete reports whether dir holds a finished unpack of the bundle
// with this sha256.
func platformComplete(dir, want string) (bool, error) {
	st, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat platform bundle %s: %w", dir, err)
	}
	if !st.IsDir() {
		return false, fmt.Errorf("platform bundle path %s is not a directory", dir)
	}
	data, err := os.ReadFile(filepath.Join(dir, platformMarker))
	if err != nil {
		if os.IsNotExist(err) {
			// No marker: an unpack that never finished (or an older driver). It
			// is replaced by the unpack below.
			return false, nil
		}
		return false, fmt.Errorf("read platform marker %s: %w", dir, err)
	}
	if strings.TrimSpace(string(data)) != want {
		return false, fmt.Errorf("platform bundle %s carries the marker of sha256 %q, want %s", dir, strings.TrimSpace(string(data)), want)
	}
	if _, err := os.Stat(filepath.Join(dir, "manifest.json")); err != nil {
		return false, fmt.Errorf("platform bundle %s has no manifest.json: %w", dir, err)
	}
	return true, nil
}

// unpackPlatform unpacks the archive into a temporary directory next to dest and
// renames it into place. An entry that would escape the bundle (a ".." path, an
// absolute path, an absolute symlink, a symlink that points outside, a device or
// a fifo) is refused: the guest reads this tree, and a path outside it is a path
// into the node.
func (b *Box) unpackPlatform(archive, dest, want string) error {
	parent := filepath.Dir(dest)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(parent, "."+filepath.Base(dest)+".unpack-*")
	if err != nil {
		return fmt.Errorf("create temporary directory for the unpack: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(stage)
		}
	}()

	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("the archive is not gzip: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	var files int
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read the archive: %w", err)
		}
		name, err := bundleEntryPath(hdr.Name)
		if err != nil {
			return err
		}
		target := filepath.Join(stage, filepath.FromSlash(name))
		// Each link is checked on its own below, but a chain of them can still
		// leave the bundle, so no entry may go through or replace a link.
		if err := bundleEntryPlain(stage, name); err != nil {
			return err
		}
		mode := hdr.FileInfo().Mode()
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := writeBundleFile(target, tr, mode); err != nil {
				return err
			}
			files++
			continue
		case tar.TypeSymlink:
			// A relative link that stays inside the bundle is the shape the
			// contract asks for (bin/boxctl -> boxd, bin/<tool> -> ../tools/...).
			if err := bundleLinkTarget(name, hdr.Linkname); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
			continue
		case tar.TypeLink:
			if err := bundleLinkTarget(name, hdr.Linkname); err != nil {
				return fmt.Errorf("hard link %s: %w", hdr.Name, err)
			}
			linkName, err := bundleEntryPath(hdr.Linkname)
			if err != nil {
				return err
			}
			if err := bundleEntryPlain(stage, linkName); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Link(filepath.Join(stage, filepath.FromSlash(linkName)), target); err != nil {
				return fmt.Errorf("hard link %s: %w", hdr.Name, err)
			}
			files++
			continue
		default:
			return fmt.Errorf("the archive holds the entry %q of type %q: a bundle may only hold files, directories and symlinks", hdr.Name, string(hdr.Typeflag))
		}
	}
	if files == 0 {
		return fmt.Errorf("the archive holds no files")
	}
	if _, err := os.Stat(filepath.Join(stage, "manifest.json")); err != nil {
		return fmt.Errorf("the bundle has no manifest.json: %w", err)
	}
	// The marker goes in last: its presence is the proof that every entry of the
	// archive is there.
	if err := os.WriteFile(filepath.Join(stage, platformMarker), []byte(want+"\n"), 0o444); err != nil {
		return fmt.Errorf("write the platform marker: %w", err)
	}
	// MkdirTemp makes its directory 0700, and a directory the archive only
	// implies (a file "bin/boxd" without a "bin/" entry) is created with the
	// process umask, which on a hardened node is 077. The bundle is a system
	// directory of the guest (/opt/box), so every directory of it is made
	// traversable: a bundle a node cannot read is a box that does not boot.
	if err := makeBundleDirsReadable(stage); err != nil {
		return err
	}
	if err := b.m.opts.Identity.ChownTree(stage); err != nil {
		return err
	}
	// Two boxes may start on one new bundle at once. The one that comes second
	// keeps the directory of the first, whose VM may already run from it.
	b.m.platformMu.Lock()
	defer b.m.platformMu.Unlock()
	if ok, err := platformComplete(dest, want); err != nil {
		return err
	} else if ok {
		return nil
	}
	// A directory without a marker is a half unpack of an older attempt; it must
	// not hide the one that is complete now.
	if _, err := os.Lstat(dest); err == nil {
		if err := os.RemoveAll(dest); err != nil {
			return fmt.Errorf("replace the incomplete platform directory %s: %w", dest, err)
		}
	}
	if err := os.Rename(stage, dest); err != nil {
		return fmt.Errorf("put the platform bundle in place: %w", err)
	}
	cleanup = false
	return nil
}

// bundleEntryPath cleans a tar entry name and refuses anything that is not a
// plain path below the bundle root.
func bundleEntryPath(name string) (string, error) {
	cleaned := path.Clean(strings.TrimPrefix(name, "./"))
	if cleaned == "." || cleaned == "" {
		return "", nil
	}
	if path.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("the archive holds the entry %q, which escapes the bundle", name)
	}
	return cleaned, nil
}

// bundleEntryPlain refuses an entry whose path meets a link already unpacked
// below root, at any level.
func bundleEntryPlain(root, name string) error {
	current := root
	for _, part := range strings.Split(name, "/") {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		st, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("the archive holds the entry %q, which goes through or replaces a symlink", name)
		}
	}
	return nil
}

// bundleLinkTarget checks a symbolic or hard link: it must be relative and it
// must stay inside the bundle.
func bundleLinkTarget(name, link string) error {
	if link == "" {
		return fmt.Errorf("the entry %q is a link without a target", name)
	}
	if path.IsAbs(link) {
		return fmt.Errorf("the entry %q is an absolute symlink to %q: a bundle may only hold relative symlinks", name, link)
	}
	joined := path.Join(path.Dir(path.Clean(strings.TrimPrefix(name, "./"))), link)
	if joined == ".." || strings.HasPrefix(joined, "../") {
		return fmt.Errorf("the entry %q links to %q, which points outside the bundle", name, link)
	}
	return nil
}

// writeBundleFile writes one regular file of the archive with its mode.
func writeBundleFile(target string, r io.Reader, mode os.FileMode) error {
	perm := mode.Perm()
	if perm == 0 {
		perm = 0o644
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Chmod(target, perm)
}

// readPlatformManifest reads the manifest of an unpacked bundle: the version of
// the bundle, the boxd it holds and the tools.
//
// It is what a bundle directory says about itself, so a message can name the
// bundle that is really mounted.
type PlatformManifest struct {
	Schema  int               `json:"schema"`
	Version string            `json:"version"`
	Arch    string            `json:"arch"`
	Boxd    string            `json:"boxd"`
	Tools   map[string]string `json:"tools"`
	BuiltAt string            `json:"built_at"`
}

// ReadPlatformManifest reads <dir>/manifest.json.
func ReadPlatformManifest(dir string) (*PlatformManifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	var m PlatformManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse %s/manifest.json: %w", dir, err)
	}
	return &m, nil
}

// PlatformTools names the tools of a manifest in a stable order.
func (m *PlatformManifest) PlatformTools() []string {
	names := make([]string, 0, len(m.Tools))
	for name := range m.Tools {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, strings.TrimSpace(name+" "+m.Tools[name]))
	}
	return out
}

// instanceLimaYAML returns the path of an instance's configuration file.
func (m *Manager) instanceLimaYAML(boxID string) string {
	return filepath.Join(m.lima.InstanceDir(boxID), "lima.yaml")
}

// instancePlatformMount returns the host directory an instance currently mounts
// at /opt/box, or "" when it mounts none.
//
// The instance's own lima.yaml is the only record of what a VM was started with,
// so it is what a changed platform block is compared against.
func (m *Manager) instancePlatformMount(inst *limactl.Instance) string {
	if inst == nil {
		return ""
	}
	data, err := os.ReadFile(m.instanceLimaYAML(inst.Name))
	if err != nil {
		return ""
	}
	var doc limaYAML
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return ""
	}
	for _, mnt := range doc.Mounts {
		if filepath.Clean(mnt.MountPoint) == PlatformMountPoint {
			return mnt.Location
		}
	}
	return ""
}

// applyPlatformMount puts the platform mount of this box into the instance's
// configuration, replacing the mount of another bundle (an upgrade) and leaving
// every other field of the file as Lima wrote it.
//
// A new instance gets its configuration from renderLimaYAML. This is the path of
// an instance that already exists: `box upgrade` re-registers the job with
// another platform block, and the driver makes that block the instance's
// configuration before it starts the VM. Lima reads lima.yaml when it boots the
// instance and `limactl edit` only takes cpu, memory and disk, so the file is
// written here.
func (b *Box) applyPlatformMount(inst *limactl.Instance, platformDir string) error {
	if inst == nil {
		return nil
	}
	path := b.m.instanceLimaYAML(inst.Name)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// An instance directory Lima did not create (or one whose
			// configuration is gone) has nowhere to record a mount. That is not a
			// reason to fail a start the box would otherwise survive, so it is
			// said in a task event instead.
			if platformDir != "" {
				b.event("platform", fmt.Sprintf("Box %s has no lima configuration at %s: the platform bundle could not be recorded as its /opt/box mount", b.spec.BoxID, path))
			}
			return nil
		}
		return fmt.Errorf("read the lima configuration %s: %w", path, err)
	}
	// The file is edited as a document, not re-rendered from the spec: Lima adds
	// fields of its own when it creates an instance (the local ssh port, the
	// image digest, the host resolver), and losing one of them breaks a boot.
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parse the lima configuration %s: %w", path, err)
	}
	mounts, _ := doc["mounts"].([]any)
	current := ""
	kept := make([]any, 0, len(mounts)+1)
	for _, raw := range mounts {
		entry, ok := raw.(map[string]any)
		if ok {
			if point, ok := entry["mountPoint"].(string); ok && filepath.Clean(point) == PlatformMountPoint {
				if location, ok := entry["location"].(string); ok {
					current = location
				}
				continue
			}
		}
		kept = append(kept, raw)
	}
	if current == platformDir {
		return nil
	}
	if platformDir != "" {
		kept = append(kept, b.m.platformMountDocument(platformDir))
	}
	doc["mounts"] = kept
	if len(kept) > 0 {
		doc["mountType"] = b.m.mountType()
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("render the lima configuration %s: %w", path, err)
	}
	// The file is replaced, not truncated: Lima writes lima.yaml read-only, so a
	// driver that runs as the file's own user (no privileges to drop) can only
	// replace it in its directory, and a crash never leaves half a file.
	tmp := path + ".box-new"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return fmt.Errorf("write the lima configuration %s: %w", tmp, err)
	}
	if err := b.m.opts.Identity.Chown(tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("replace the lima configuration %s: %w", path, err)
	}
	if platformDir == "" {
		b.event("platform", fmt.Sprintf("Box %s no longer takes a platform bundle: its /opt/box mount is gone from %s", b.spec.BoxID, path))
	} else {
		name := shortDigest(platformDir)
		if b.spec.Platform != nil {
			name = b.spec.Platform.String()
		}
		b.event("platform", fmt.Sprintf("Box %s takes platform bundle %s into its lima configuration: it is mounted at /opt/box at the next start", b.spec.BoxID, name))
	}
	return nil
}

// mountType is Lima's mount implementation for the driver's VM type.
func (m *Manager) mountType() string {
	if m.opts.VMType == VMTypeVZ {
		return "virtiofs"
	}
	return "9p"
}

// platformMountDocument is the mount entry of the platform bundle as it goes
// into lima.yaml.
func (m *Manager) platformMountDocument(platformDir string) map[string]any {
	entry := map[string]any{
		"location":   platformDir,
		"mountPoint": PlatformMountPoint,
		"writable":   false,
	}
	if m.opts.VMType == VMTypeQEMU {
		entry["9p"] = map[string]any{"cache": CacheFscache}
	}
	return entry
}

// platformMessage says which bundle a box mounts and what it holds: the
// manifest of the unpacked directory is what the guest really sees, so it is
// what the task event names.
func platformMessage(spec *PlatformSpec, dir string) string {
	name := "the platform bundle"
	if spec != nil {
		name = "platform bundle " + spec.String()
	}
	details := ""
	if manifest, err := ReadPlatformManifest(dir); err == nil {
		if v := strings.TrimSpace(manifest.Version); v != "" {
			name = "platform bundle " + v
		}
		var parts []string
		if b := strings.TrimSpace(manifest.Boxd); b != "" {
			parts = append(parts, "boxd "+b)
		}
		if tools := manifest.PlatformTools(); len(tools) > 0 {
			parts = append(parts, "tools "+strings.Join(tools, ", "))
		}
		if spec != nil && strings.TrimSpace(spec.Version) != "" && manifest.Version != "" && spec.Version != manifest.Version {
			// The block and the archive disagree; say both, so a wrong index
			// entry is visible in the task events and not only in the guest.
			name = fmt.Sprintf("platform bundle %s (the task names version %s)", manifest.Version, spec.Version)
		}
		if len(parts) > 0 {
			details = " (" + strings.Join(parts, "; ") + ")"
		}
	}
	return fmt.Sprintf("Mounted %s read-only at %s%s", name, PlatformMountPoint, details)
}

// platformDiff names a change of the mounted bundle for the driver's messages.
// Both sides are cache directories named after a sha256.
func platformDiff(current, wanted string) string {
	switch {
	case current != "" && wanted != "":
		return fmt.Sprintf("platform %s -> %s", shortDigest(current), shortDigest(wanted))
	case wanted != "":
		return fmt.Sprintf("platform mount added (%s)", shortDigest(wanted))
	default:
		return "platform mount removed"
	}
}

// shortDigest shortens the sha256 directory name of a bundle for a message.
func shortDigest(dir string) string {
	name := filepath.Base(filepath.Clean(dir))
	if len(name) > 12 {
		name = name[:12]
	}
	return "sha256:" + name
}

// makeBundleDirsReadable gives every directory below root the mode 0755.
//
// The files keep the mode the archive gives them; only the directories are
// normalised, because their mode depends on the umask of whichever process
// unpacked the archive.
func makeBundleDirsReadable(root string) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if err := os.Chmod(path, 0o755); err != nil {
			return fmt.Errorf("set the mode of %s: %w", path, err)
		}
		return nil
	})
}
