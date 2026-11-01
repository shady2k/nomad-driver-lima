// Package catalog resolves operator-approved prepared boot artifacts, never OCI images.
package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"go.yaml.in/yaml/v3"
)

type Artifact struct {
	URL              string `json:"url"`
	SHA256           string `json:"sha256"`
	SizeBytes        int64  `json:"size_bytes"`
	VirtualSizeBytes int64  `json:"virtual_size_bytes,omitempty"`
	MinimumDiskGiB   int    `json:"minimum_disk_gib,omitempty"`
	Format           string `json:"format,omitempty"`
}
type Acceptance struct {
	Sealed           bool `json:"sealed"`
	IndependentBoots int  `json:"independent_boots"`
	UniqueIdentities bool `json:"unique_identities"`
}
type Entry struct {
	ID           string            `json:"template_id"`
	Version      string            `json:"version"`
	GuestOS      string            `json:"guest_os"`
	Architecture string            `json:"architecture"`
	VMType       string            `json:"vm_type"`
	Disk         Artifact          `json:"disk"`
	YAML         Artifact          `json:"runtime_yaml"`
	Tools        map[string]string `json:"tools"`
	Acceptance   Acceptance        `json:"acceptance"`
}
type Catalog struct {
	SchemaVersion int     `json:"schema_version"`
	Templates     []Entry `json:"templates"`
}

var id = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,79}$`)
var hash = regexp.MustCompile(`^[a-f0-9]{64}$`)

func Sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func Read(path, expected string) (*Catalog, error) {
	if !hash.MatchString(expected) {
		return nil, fmt.Errorf("catalog checksum required")
	}
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	raw, e := io.ReadAll(io.LimitReader(f, 4<<20+1))
	if e != nil {
		return nil, e
	}
	if len(raw) > 4<<20 || Sum(raw) != expected {
		return nil, fmt.Errorf("catalog checksum mismatch/size limit")
	}
	var c Catalog
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	if e = d.Decode(&c); e != nil {
		return nil, fmt.Errorf("catalog metadata invalid")
	}
	if e = d.Decode(new(any)); e != io.EOF {
		return nil, fmt.Errorf("catalog trailing data")
	}
	if c.SchemaVersion != 1 || len(c.Templates) < 1 || len(c.Templates) > 1024 {
		return nil, fmt.Errorf("catalog version/count invalid")
	}
	seen := map[string]bool{}
	for _, v := range c.Templates {
		if e = v.Validate(); e != nil {
			return nil, e
		}
		k := v.ID + "/" + v.Version + "/" + v.Architecture + "/" + v.VMType
		if seen[k] {
			return nil, fmt.Errorf("duplicate catalog entry")
		}
		seen[k] = true
	}
	return &c, nil
}
func (v Entry) Validate() error {
	if !id.MatchString(v.ID) || !id.MatchString(v.Version) || v.GuestOS != "linux" || (v.Architecture != "amd64" && v.Architecture != "arm64") || (v.VMType != "qemu" && v.VMType != "vz") {
		return fmt.Errorf("catalog identity/platform invalid")
	}
	if !v.Acceptance.Sealed || v.Acceptance.IndependentBoots < 2 || !v.Acceptance.UniqueIdentities {
		return fmt.Errorf("catalog acceptance gate not passed")
	}
	if v.Disk.Format != "raw" && v.Disk.Format != "qcow2" {
		return fmt.Errorf("prepared disk format invalid")
	}
	if v.Disk.MinimumDiskGiB < 1 || v.Disk.VirtualSizeBytes < 1 || int64(v.Disk.MinimumDiskGiB) < (v.Disk.VirtualSizeBytes-1)/(1<<30)+1 {
		return fmt.Errorf("prepared disk minimum below virtual size")
	}
	for i, a := range []Artifact{v.Disk, v.YAML} {
		u, e := url.Parse(a.URL)
		if e != nil || u.User != nil || u.Fragment != "" || !hash.MatchString(a.SHA256) || a.SizeBytes < 1 {
			return fmt.Errorf("catalog artifact metadata invalid")
		}
		if u.Scheme == "file" {
			if u.Host != "" || !filepath.IsAbs(u.Path) || u.RawQuery != "" {
				return fmt.Errorf("catalog file URL invalid")
			}
		} else if u.Scheme != "https" || u.Host == "" {
			return fmt.Errorf("catalog HTTPS/file URL required")
		}
		if i == 1 && a.SizeBytes > 4<<20 {
			return fmt.Errorf("runtime YAML too large")
		}
	}
	return nil
}
func (c *Catalog) Select(id, version, arch, vm string) (Entry, error) {
	for _, e := range c.Templates {
		if e.ID == id && e.Version == version && e.Architecture == arch && e.VMType == vm {
			return e, nil
		}
	}
	return Entry{}, fmt.Errorf("approved template/version/platform unavailable")
}

type Access struct{ UID, GID int }

type Cache struct {
	Runtime  *Access
	Dir      string
	Progress func(string, int64, int64)
}

func verify(path string, a Artifact) bool {
	f, e := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return false
	}
	defer f.Close()
	s, e := f.Stat()
	if e != nil || !s.Mode().IsRegular() || s.Size() != a.SizeBytes {
		return false
	}
	h := sha256.New()
	_, e = io.Copy(h, f)
	return e == nil && hex.EncodeToString(h.Sum(nil)) == a.SHA256
}
func (c Cache) Fetch(ctx context.Context, a Artifact) (string, error) {
	if !hash.MatchString(a.SHA256) || a.SizeBytes < 1 {
		return "", fmt.Errorf("artifact identity required")
	}
	if !filepath.IsAbs(c.Dir) {
		return "", fmt.Errorf("absolute cache directory required")
	}
	if e := c.prepareDirectory(); e != nil {
		return "", e
	}

	dst := filepath.Join(c.Dir, a.SHA256)
	lock, e := os.OpenFile(dst+".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		return "", e
	}
	defer lock.Close()
	for {
		e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if e == nil {
			break
		}
		if e != syscall.EWOULDBLOCK {
			return "", e
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if e := c.checkFile(dst); e != nil {
		return "", e
	}
	if verify(dst, a) {
		if c.Progress != nil {
			c.Progress("cache_verified", a.SizeBytes, a.SizeBytes)
		}
		return dst, nil
	}
	u, e := url.Parse(a.URL)
	if e != nil {
		return "", fmt.Errorf("artifact URL invalid")
	}
	var reader io.ReadCloser
	if u.Scheme == "file" && u.Host == "" && u.RawQuery == "" && u.User == nil && filepath.IsAbs(u.Path) {
		reader, e = os.Open(u.Path)
	} else if u.Scheme == "https" && u.Host != "" && u.User == nil && u.Fragment == "" {
		cl := &http.Client{Timeout: 10 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return fmt.Errorf("artifact redirect refused") }}
		req, _ := http.NewRequestWithContext(ctx, "GET", a.URL, nil)
		var r *http.Response
		r, e = cl.Do(req)
		if e == nil {
			if r.StatusCode != 200 {
				r.Body.Close()
				return "", fmt.Errorf("artifact storage unavailable (HTTP %d)", r.StatusCode)
			}
			reader = r.Body
		}
	} else {
		return "", fmt.Errorf("artifact URL not allowed")
	}
	if e != nil {
		return "", fmt.Errorf("artifact storage unavailable; retry transfer only")
	}
	defer reader.Close()
	tmp, e := os.CreateTemp(c.Dir, ".download-")
	if e != nil {
		return "", e
	}
	name := tmp.Name()
	defer os.Remove(name)
	defer tmp.Close()
	h := sha256.New()
	buf := make([]byte, 1<<20)
	var n int64
	last := time.Time{}
	for {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		k, er := reader.Read(buf)
		if k > 0 {
			n += int64(k)
			if n > a.SizeBytes {
				return "", fmt.Errorf("artifact size mismatch")
			}
			if _, e = tmp.Write(buf[:k]); e != nil {
				return "", e
			}
			h.Write(buf[:k])
			if c.Progress != nil && time.Since(last) > time.Second {
				c.Progress("artifact_fetch", n, a.SizeBytes)
				last = time.Now()
			}
		}
		if er == io.EOF {
			break
		}
		if er != nil {
			return "", fmt.Errorf("artifact transfer incomplete; retryable")
		}
	}
	if n != a.SizeBytes || hex.EncodeToString(h.Sum(nil)) != a.SHA256 {
		return "", fmt.Errorf("artifact checksum/size mismatch")
	}
	if e = tmp.Chmod(0444); e != nil {
		return "", e
	}
	if e = tmp.Sync(); e != nil {
		return "", e
	}
	if e = tmp.Close(); e != nil {
		return "", e
	}
	if e = os.Rename(name, dst); e != nil {
		return "", e
	}
	dir, e := os.Open(c.Dir)
	if e != nil {
		return "", e
	}
	defer dir.Close()
	if e = dir.Sync(); e != nil {
		return "", e
	}
	if c.Progress != nil {
		c.Progress("checksum_verified", n, a.SizeBytes)
	}
	return dst, nil
}

// Runtime YAML is approved configuration, but may not reintroduce boot installs,
// mounts or a second image. The driver supplies a fresh immutable disk reference.
func (c Cache) Materialize(ctx context.Context, v Entry) (string, error) {
	if e := v.Validate(); e != nil {
		return "", e
	}
	yp, e := c.Fetch(ctx, v.YAML)
	if e != nil {
		return "", e
	}
	raw, e := os.ReadFile(yp)
	if e != nil {
		return "", e
	}
	return c.materializeRuntime(ctx, v.Disk, raw, v.Architecture)
}

// MaterializePrivateCopy is separate operator recovery admission, never catalog acceptance.
func (c Cache) MaterializePrivateCopy(ctx context.Context, disk Artifact, architecture string) (string, error) {
	u, e := url.Parse(disk.URL)
	if e != nil || u.Scheme != "file" || u.Host != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !filepath.IsAbs(u.Path) || !hash.MatchString(disk.SHA256) || disk.SizeBytes < 1 || (architecture != "amd64" && architecture != "arm64") {
		return "", fmt.Errorf("exact local private copy required")
	}
	raw := []byte("minimumLimaVersion: 2.2.1\nvmType: qemu\narch: " + map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[architecture] + "\n")
	return c.materializeRuntime(ctx, disk, raw, architecture)
}
func (c Cache) materializeRuntime(ctx context.Context, artifact Artifact, raw []byte, architecture string) (string, error) {
	disk, e := c.Fetch(ctx, artifact)
	if e != nil {
		return "", e
	}
	var y map[string]any
	if yaml.Unmarshal(raw, &y) != nil || y == nil {
		return "", fmt.Errorf("runtime YAML invalid")
	}
	for _, k := range []string{"base", "provision", "mounts", "additionalDisks", "portForwards", "networks", "copyToHost", "hostResolver", "env", "param", "user"} {
		if _, ok := y[k]; ok {
			return "", fmt.Errorf("runtime YAML contains forbidden inherited/host/provision field: %s", k)
		}
	}
	y["images"] = []map[string]any{{"location": disk, "arch": map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[architecture], "digest": "sha256:" + artifact.SHA256}}
	y["mounts"] = []any{}
	y["plain"] = true
	y["containerd"] = map[string]bool{"system": false, "user": false}
	y["ssh"] = map[string]bool{"forwardAgent": false, "loadDotSSHPubKeys": false}
	y["propagateProxyEnv"] = false
	out, e := yaml.Marshal(y)
	if e != nil {
		return "", e
	}
	p := filepath.Join(c.Dir, Sum(out)+".yaml")
	// A non-secret artifact: readable by configured unprivileged Lima user.
	if e = c.checkFile(p); e != nil {
		return "", e
	}
	if verify(p, Artifact{SHA256: Sum(out), SizeBytes: int64(len(out))}) {
		return p, nil
	}
	f, e := os.CreateTemp(c.Dir, ".yaml-")
	if e != nil {
		return "", e
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, e = f.Write(out); e != nil {
		f.Close()
		return "", e
	}
	if e = f.Chmod(0444); e != nil {
		f.Close()
		return "", e
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return "", e
	}
	f.Close()
	if e = os.Rename(tmp, p); e != nil {
		return "", e
	}
	return p, nil
}
