package box

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

// A bake turns the disk of a box into an image.
//
// The box was sealed (boxctl seal): the disk holds the installed packages, the
// toolchains and the caches and none of the work. What is left is to make it an
// image: qemu-img converts it to a compressed qcow2 in the image cache, the file
// is named by its own digest -- which is where EnsureImage looks for an image
// this node already has -- and a record beside it says what the image is.
//
// The record is what a later `box create --image <version>-<n>` on this node
// needs, and what another node can publish from: the version, the digest, the
// size, the parent it was grown from and the recipe that would rebuild it.

// bakedDirName is the directory below the image cache that holds the records of
// the images baked on this node. The images themselves sit beside it, named by
// digest (EnsureImage's own layout).
const bakedDirName = "baked"

// BakedRecord is what a bake records about one project image.
type BakedRecord struct {
	// Version is the image's own version, "<name>-<n>".
	Version string `json:"version"`
	// Arch is amd64 or arm64.
	Arch string `json:"arch"`
	// SHA256 is the digest of the image file, which is also its name in the
	// cache: <sha256>.img.
	SHA256 string `json:"sha256"`
	// Size is the compressed size of the file in bytes.
	Size int64 `json:"size"`
	// VirtualBytes is the size of the disk inside it: the smallest disk a box
	// made from this image can have.
	VirtualBytes int64 `json:"virtual_bytes"`
	// Parent is the image the box came from.
	Parent string `json:"parent,omitempty"`
	// Name is the project's name: every version of one project shares it.
	Name string `json:"name"`
	// BuiltAt is when the image was made.
	BuiltAt string `json:"built_at"`
	// Recipe is the script that makes this environment from the parent image,
	// base64; empty when the image has none.
	Recipe string `json:"recipe,omitempty"`
	// Platform says a bundle completes this image ("mount"), as the image's own
	// /etc/box/image.json does.
	Platform string `json:"platform"`
}

// bakedNamePattern is what a version and an architecture may look like. A
// version becomes a file name below the image cache, so a value that could walk
// out of that directory is refused.
var bakedNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]{0,80}[a-z0-9])?$`)

// BakedArchPattern is the architecture of a baked image.
const (
	bakedArchAmd64 = "amd64"
	bakedArchArm64 = "arm64"
)

// ValidateBakedRecord checks that the version and the architecture of a record
// can name a file in the cache.
func ValidateBakedRecord(rec BakedRecord) error {
	if !bakedNamePattern.MatchString(rec.Version) {
		return fmt.Errorf("the image version %q is not usable: use lower-case letters, digits, dots and dashes", rec.Version)
	}
	if rec.Arch != bakedArchAmd64 && rec.Arch != bakedArchArm64 {
		return fmt.Errorf("the image architecture %q is not usable: amd64 or arm64", rec.Arch)
	}
	return nil
}

// BakedDir is where the records of the images baked on this node live.
func (m *Manager) BakedDir() string { return filepath.Join(m.opts.ImageCacheDir, bakedDirName) }

// BakedPath is the record file of one version and architecture.
func (m *Manager) BakedPath(version, arch string) string {
	return filepath.Join(m.BakedDir(), version+"."+arch+".json")
}

// BakedRecords returns the records of every image baked on this node, newest
// first. A record that cannot be read is skipped: one broken file must not hide
// the images beside it.
func (m *Manager) BakedRecords() ([]BakedRecord, error) {
	entries, err := os.ReadDir(m.BakedDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]BakedRecord, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(m.BakedDir(), entry.Name()))
		if err != nil {
			continue
		}
		var rec BakedRecord
		if json.Unmarshal(b, &rec) != nil || rec.Version == "" || rec.SHA256 == "" {
			continue
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Version != out[j].Version {
			return out[i].Version > out[j].Version
		}
		return out[i].Arch < out[j].Arch
	})
	return out, nil
}

// Bake writes the disk of a sealed box into the image cache as a compressed
// qcow2 named by its digest, and records what it is.
//
// progress receives a percentage and a line for the task log (it may be nil). The
// conversion goes to a temporary file that is renamed into place, so a bake that
// is killed leaves no half an image in the cache, and a second bake of the same
// disk produces the same bytes and loses nothing.
func (m *Manager) Bake(ctx context.Context, diskPath string, rec BakedRecord, progress func(percent int, note string)) (BakedRecord, error) {
	if progress == nil {
		progress = func(int, string) {}
	}
	if err := ValidateBakedRecord(rec); err != nil {
		return rec, err
	}
	if rec.Name == "" {
		return rec, fmt.Errorf("the image needs a name")
	}
	if st, err := os.Stat(diskPath); err != nil || !st.Mode().IsRegular() {
		if err == nil {
			err = fmt.Errorf("it is not a file")
		}
		return rec, fmt.Errorf("there is no box disk at %s to bake: %w", diskPath, err)
	}
	if err := m.opts.Identity.EnsureDir(m.BakedDir(), 0o755); err != nil {
		return rec, fmt.Errorf("create %s: %w", m.BakedDir(), err)
	}
	if err := m.opts.Identity.EnsureDir(m.opts.ImageCacheDir, 0o755); err != nil {
		return rec, fmt.Errorf("create the image cache %s: %w", m.opts.ImageCacheDir, err)
	}

	name := m.opts.QEMUImg
	if name == "" {
		name = qemuImgName
	}
	qemuImg, err := exec.LookPath(name)
	if err != nil {
		return rec, &QEMUImgMissing{name: name, err: err}
	}
	tmp, err := os.CreateTemp(m.opts.ImageCacheDir, rec.Version+".*.part")
	if err != nil {
		return rec, fmt.Errorf("create a temporary file for the image: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Close(); err != nil {
		return rec, fmt.Errorf("close %s: %w", tmpName, err)
	}
	// qemu-img writes the file itself, as the runtime user.
	if err := m.opts.Identity.Chown(tmpName); err != nil {
		return rec, fmt.Errorf("give the image to %s: %w", m.opts.Identity.Name, err)
	}

	progress(0, fmt.Sprintf("Compressing the disk of the box into an image (qemu-img convert -c) in %s", m.opts.ImageCacheDir))
	var stderr bytes.Buffer
	cmd := m.childCommand(ctx, qemuImg, "convert", "-c", "-O", "qcow2", diskPath, tmpName)
	cmd.Stderr = &stderr
	// The conversion goes with the group it started: a stopped bake must not
	// leave a converter behind, and Wait must not wait for it either.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	if err := cmd.Run(); err != nil {
		return rec, fmt.Errorf("qemu-img convert %s: %w%s", filepath.Base(diskPath), err, suffix(stderr.String()))
	}
	progress(50, "Compressed the disk; computing the digest of the image")

	sum, size, err := hashFile(tmpName, func(percent int) {
		progress(50+percent/2, fmt.Sprintf("Computing the digest of the image (%d%%)", percent))
	})
	if err != nil {
		return rec, err
	}
	if err := syncFile(tmpName); err != nil {
		return rec, err
	}
	dest := filepath.Join(m.opts.ImageCacheDir, sum+".img")
	if err := os.Rename(tmpName, dest); err != nil {
		return rec, fmt.Errorf("install the image into the cache: %w", err)
	}

	rec.SHA256 = sum
	rec.Size = size
	rec.Platform = "mount"
	if rec.BuiltAt == "" {
		rec.BuiltAt = time.Now().UTC().Format(time.RFC3339)
	}
	if virtual, ok := Qcow2VirtualSize(dest); ok {
		rec.VirtualBytes = virtual
	} else if virtual, ok := Qcow2VirtualSize(diskPath); ok {
		rec.VirtualBytes = virtual
	}
	if err := m.writeBakedRecord(rec); err != nil {
		return rec, err
	}
	progress(100, fmt.Sprintf("The image is %s (%s), sha256 %s", filepath.Base(dest), humanBytes(size), sum))
	return rec, nil
}

// writeBakedRecord writes the record of a baked image and reads it back: a
// record that did not arrive is an image nothing knows about.
func (m *Manager) writeBakedRecord(rec BakedRecord) error {
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	path := m.BakedPath(rec.Version, rec.Arch)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("install %s: %w", path, err)
	}
	back, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s back: %w", path, err)
	}
	var got BakedRecord
	if err := json.Unmarshal(back, &got); err != nil {
		return fmt.Errorf("%s read back is not JSON: %w", path, err)
	}
	if got.SHA256 != rec.SHA256 || got.Version != rec.Version {
		return fmt.Errorf("%s read back names %s %s, wrote %s %s", path, got.Version, got.SHA256, rec.Version, rec.SHA256)
	}
	return nil
}

// BakedImagePath is where the file of a baked record is: the cache names an
// image by its digest, which is what a box created from it downloads nothing for.
func (m *Manager) BakedImagePath(rec BakedRecord) string {
	return filepath.Join(m.opts.ImageCacheDir, rec.SHA256+".img")
}

// hashFile returns the sha256 and the size of a file, reporting how far it got.
func hashFile(path string, progress func(percent int)) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", 0, fmt.Errorf("stat %s: %w", path, err)
	}
	h := sha256.New()
	buf := make([]byte, 1<<20)
	var read int64
	last := -1
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
			read += int64(n)
			if progress != nil && st.Size() > 0 {
				percent := int(read * 100 / st.Size())
				if percent/5 != last/5 {
					last = percent
					progress(percent)
				}
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			return "", 0, fmt.Errorf("read %s: %w", path, rerr)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), read, nil
}

// Qcow2VirtualSize reads the virtual disk size out of a qcow2 header, which is
// how large the disk inside an image is -- and therefore the smallest disk a box
// made from it can have.
//
// Offset 0 is the magic "QFI\xfb", offset 24 is the virtual size as a 64 bit big
// endian number (qcow2 format version 2 and 3). Reading 32 bytes is as cheap as a
// stat, and `qemu-img info` would not be: the driver's fingerprint runs this for
// every box on the node every thirty seconds.
func Qcow2VirtualSize(path string) (int64, bool) {
	file, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer file.Close()
	var header [32]byte
	if _, err := io.ReadFull(file, header[:]); err != nil {
		return 0, false
	}
	if string(header[0:4]) != "QFI\xfb" {
		return 0, false
	}
	if version := binary.BigEndian.Uint32(header[4:8]); version < 2 {
		return 0, false
	}
	size := binary.BigEndian.Uint64(header[24:32])
	if size == 0 || size > 1<<50 {
		return 0, false
	}
	return int64(size), true
}
