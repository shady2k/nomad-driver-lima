package box

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// qemuImgName is the qemu-img binary EnsureImageDisk looks for when the manager
// names none. It comes from PATH the way limactl does.
const qemuImgName = "qemu-img"

// uncompressedSuffix names the uncompressed copy of an image in the cache: it
// sits next to the verified download and carries the same sha256 key.
const uncompressedSuffix = ".uncompressed.qcow2"

// DraftURLPrefix is the url of a project image that was baked on this node: the
// file is in the cache under its digest, and there is nothing to download.
//
// A box created from a draft names it this way (`internal/cli` writes it), and the
// driver treats it as a cache lookup and never as a download: the cache holds
// every image a box on this node is created from, and a url that is not fetchable
// would only report a broken URL instead of the truth, which is that the image is
// no longer in the cache.
const DraftURLPrefix = "file:///box-draft/"

// EnsureImage makes the cloud image available in the cache directory, keyed by
// its sha256. A cached file is re-verified; a new download is written to a
// temporary file, verified, then renamed into place so a half download is never
// used. progress is called with a 0..100 percentage (it may be nil).
func (b *Box) EnsureImage(ctx context.Context, url, wantSHA256 string, progress func(percent int)) (string, error) {
	want := strings.ToLower(strings.TrimSpace(wantSHA256))
	if len(want) != 64 {
		return "", fmt.Errorf("image sha256 must be 64 hex characters, got %q", wantSHA256)
	}
	if _, err := hex.DecodeString(want); err != nil {
		return "", fmt.Errorf("image sha256 %q is not hexadecimal: %w", wantSHA256, err)
	}
	if url == "" {
		return "", fmt.Errorf("image url is required")
	}
	if err := b.m.opts.Identity.EnsureDir(b.m.opts.ImageCacheDir, 0o755); err != nil {
		return "", fmt.Errorf("create image cache %s: %w", b.m.opts.ImageCacheDir, err)
	}
	dest := filepath.Join(b.m.opts.ImageCacheDir, want+".img")

	if ok, err := verifyFile(dest, want); err != nil {
		return "", err
	} else if ok {
		b.event("image", fmt.Sprintf("Using cached image %s (sha256 verified)", filepath.Base(dest)))
		return dest, nil
	}
	if strings.HasPrefix(url, DraftURLPrefix) {
		// A draft is a file this node baked, not a download: a cache that lost it
		// is a bake that has to run again.
		return "", fmt.Errorf("the image %s is a draft of this node and its file is no longer in the image cache %s "+
			"(sha256 %s): `box images` lists the drafts there are, and baking the box again makes a new one",
			strings.TrimPrefix(url, DraftURLPrefix), b.m.opts.ImageCacheDir, want)
	}

	b.event("image", fmt.Sprintf("Downloading box image from %s", url))
	tmp, err := os.CreateTemp(b.m.opts.ImageCacheDir, want+".*.part")
	if err != nil {
		return "", fmt.Errorf("create temporary image file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()
	// limactl, which runs as the runtime user, has to read the image: the
	// download is written as the client user (root on a Linux node), so the file
	// is given to that user before it is filled.
	if err := b.m.opts.Identity.Chown(tmpName); err != nil {
		return "", fmt.Errorf("give the image download to %s: %w", b.m.opts.Identity.Name, err)
	}

	sum, size, err := downloadTo(ctx, url, tmp, progress)
	if err != nil {
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		return "", fmt.Errorf("sync image download: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close image download: %w", err)
	}
	if sum != want {
		return "", fmt.Errorf("image %s has sha256 %s, want %s", url, sum, want)
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return "", fmt.Errorf("install image into cache: %w", err)
	}
	b.event("image", fmt.Sprintf("Downloaded image %s (%s, sha256 verified)", filepath.Base(dest), humanBytes(size)))
	return dest, nil
}

// EnsureImageDisk returns the image a new box disk is built from: an
// uncompressed qcow2 copy of the verified download, made once per image and
// reused by every box after it.
//
// A published image is compressed (images/build.sh converts with -c) to keep the
// download small, and Lima builds a box disk out of it as it is: the disk of a
// box then carries compressed clusters, and qemu inflates them again on every
// read of a guest file. A box of 2 vCPU and 2 GiB that ran a large cgo build was
// out of memory, the guest read its program files over and over, and qemu spent
// about five host cores for twenty minutes inflating cluster after cluster of
// its own disk, reading the host's page cache in 12 KiB pieces. An uncompressed
// copy costs one conversion per image and the disk of every box made from it has
// plain clusters from the first boot.
//
// If qemu-img is not installed the compressed image is handed back and a task
// event says so: a box that starts from a compressed image is what this code did
// before, and a box that does not start at all is worse.
func (b *Box) EnsureImageDisk(ctx context.Context, url, wantSHA256 string, progress func(percent int)) (string, error) {
	imagePath, err := b.EnsureImage(ctx, url, wantSHA256, progress)
	if err != nil {
		return "", err
	}
	dest := uncompressedPath(imagePath)
	if st, err := os.Stat(dest); err == nil && st.Mode().IsRegular() && st.Size() > 0 {
		b.event("image", fmt.Sprintf("Using the uncompressed image %s", filepath.Base(dest)))
		return dest, nil
	}
	if err := b.uncompress(ctx, imagePath, dest); err != nil {
		var missing *QEMUImgMissing
		if errors.As(err, &missing) {
			b.event("image", fmt.Sprintf("Box disk stays compressed: %v. Install qemu-img on the node so a new box disk has plain clusters", err))
		} else {
			b.event("image", fmt.Sprintf("Box disk stays compressed: %v. The conversion runs again for the next box", err))
		}
		return imagePath, nil
	}
	b.event("image", fmt.Sprintf("Uncompressed image %s ready (qemu-img convert, reused by every box on this node)", filepath.Base(dest)))
	return dest, nil
}

// uncompressedPath is where the uncompressed copy of an image lives: next to the
// verified download, under the same sha256 key.
func uncompressedPath(imagePath string) string {
	return strings.TrimSuffix(imagePath, ".img") + uncompressedSuffix
}

// QEMUImgMissing says that the node has no qemu-img, which is a setting an
// operator can change rather than a failure of this box.
type QEMUImgMissing struct {
	name string
	err  error
}

func (e *QEMUImgMissing) Error() string {
	return fmt.Sprintf("%s is not installed (%v)", e.name, e.err)
}

// Err is why qemu-img could not be used.
func (e *QEMUImgMissing) Err() error { return e.err }

func (e *QEMUImgMissing) Unwrap() error { return e.err }

// uncompress writes an uncompressed qcow2 copy of src to dest with qemu-img,
// into a temporary file in the cache that is renamed into place: a half
// conversion is never used, and a second box made from the same image at the
// same moment converts alongside it and loses nothing (both outputs are the same
// bytes).
func (b *Box) uncompress(ctx context.Context, src, dest string) error {
	name := b.m.opts.QEMUImg
	if name == "" {
		name = qemuImgName
	}
	qemuImg, err := exec.LookPath(name)
	if err != nil {
		return &QEMUImgMissing{name: name, err: err}
	}
	tmp, err := os.CreateTemp(b.m.opts.ImageCacheDir, filepath.Base(dest)+".*.part")
	if err != nil {
		return fmt.Errorf("create a temporary file for the uncompressed image: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	// qemu-img writes the file itself, as the runtime user, so the temporary
	// file belongs to that user before it is filled, like the download.
	if err := b.m.opts.Identity.Chown(tmpName); err != nil {
		return fmt.Errorf("give the uncompressed image to %s: %w", b.m.opts.Identity.Name, err)
	}
	b.event("image", fmt.Sprintf("Uncompressing image %s once for every box on this node", filepath.Base(src)))
	// No -c: plain clusters are the point. No preallocation option either, so
	// the copy is as thin as the download.
	cmd := b.m.childCommand(ctx, qemuImg, "convert", "-O", "qcow2", src, tmpName)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("qemu-img convert %s: %w%s", filepath.Base(src), err, suffix(stderr.String()))
	}
	if err := syncFile(tmpName); err != nil {
		return err
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return fmt.Errorf("install the uncompressed image into the cache: %w", err)
	}
	return nil
}

// syncFile flushes a file qemu-img wrote, so a power loss cannot leave a
// renamed but empty image behind.
func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", path, err)
	}
	return nil
}

// verifyFile reports whether path exists and has the expected sha256. A size
// mismatch short-circuits before hashing.
func verifyFile(path, want string) (bool, error) {
	st, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat cached image %s: %w", path, err)
	}
	if st.IsDir() {
		return false, fmt.Errorf("cached image path %s is a directory", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("open cached image %s: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, fmt.Errorf("hash cached image %s: %w", path, err)
	}
	if hex.EncodeToString(h.Sum(nil)) != want {
		return false, nil
	}
	return true, nil
}

type progressWriter struct {
	total int64
	done  int64
	last  int
	cb    func(int)
}

func (w *progressWriter) Write(p []byte) (int, error) {
	w.done += int64(len(p))
	if w.cb != nil && w.total > 0 {
		percent := int(w.done * 100 / w.total)
		if percent/10 != w.last/10 {
			w.last = percent
			w.cb(percent)
		}
	}
	return len(p), nil
}

func downloadTo(ctx context.Context, url string, dst io.Writer, progress func(int)) (string, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", 0, fmt.Errorf("build image request: %w", err)
	}
	client := &http.Client{Timeout: 0}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("download image %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("download image %s: unexpected status %s", url, resp.Status)
	}
	pw := &progressWriter{total: resp.ContentLength, cb: progress}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(dst, h, pw), resp.Body)
	if err != nil {
		return "", 0, fmt.Errorf("download image %s: %w", url, err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
