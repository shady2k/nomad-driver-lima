package box

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

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
