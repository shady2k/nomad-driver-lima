package driver

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"

	"github.com/hashicorp/nomad/plugins/drivers"

	"github.com/shady2k/nomad-driver-lima/internal/box"
)

// taskFilesDigest fingerprints the files Nomad rendered into a task's local and
// secrets directories.
//
// The driver copies those files into the guest when the task starts. Nomad
// re-renders a template whose Nomad Variable changed (change_mode = "noop", so a
// credential rotation never restarts the VM), and the driver then has to notice
// and refresh the guest's tmpfs copy. A digest of path, size and modification time
// is cheap enough to compute on a timer and exact enough to catch a rotation.
func taskFilesDigest(tc *drivers.TaskConfig) string {
	if tc == nil {
		return ""
	}
	h := sha256.New()
	for _, root := range []string{tc.TaskDir().LocalDir, tc.TaskDir().SecretsDir} {
		if root == "" {
			continue
		}
		var entries []string
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !d.Type().IsRegular() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return nil
			}
			entries = append(entries, fmt.Sprintf("%s\x00%s\x00%d\x00%d", filepath.Base(root), rel, info.Size(), info.ModTime().UnixNano()))
			return nil
		})
		sort.Strings(entries)
		for _, entry := range entries {
			h.Write([]byte(entry))
			var size [8]byte
			binary.BigEndian.PutUint64(size[:], uint64(len(entry)))
			h.Write(size[:])
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// refreshTaskFiles puts the task files in place again when Nomad re-rendered
// them, and tells boxd so a running box picks up new executor credentials.
//
// The box is not restarted: this is what makes a credential rotation reach a
// running box (see docs/design-decisions.md).
func (d *Driver) refreshTaskFiles(h *taskHandle, b *box.Box) {
	if h == nil || b == nil {
		return
	}
	digest := taskFilesDigest(h.tc)
	if digest == "" || digest == h.filesDigest() {
		return
	}
	n, err := b.StageTaskFiles(h.tc.TaskDir().LocalDir, h.tc.TaskDir().SecretsDir, h.tc.TaskDir().PrivateDir)
	if err != nil {
		d.emit(h.tc, "files", fmt.Sprintf("Box %s: the task files changed, but they could not be put where the guest mounts them: %v", h.boxID, err))
		return
	}
	h.setFilesDigest(digest)
	d.emit(h.tc, "files", fmt.Sprintf("Box %s: Nomad re-rendered the task files (%d file(s)); the guest has them again", h.boxID, n))
	if _, stderr, err := d.boxm.Lima().Shell(d.ctx, h.boxID, []string{"boxctl", "reload"}); err != nil {
		d.emit(h.tc, "reload", fmt.Sprintf("Box %s: boxd could not reload the executor credentials (%v%s)", h.boxID, err, suffixStderr(stderr)))
		return
	}
	d.emit(h.tc, "reload", fmt.Sprintf("Box %s: boxd reloaded the executor credentials; a running turn keeps its executor until it ends", h.boxID))
}

// suffixStderr renders a child's stderr for a message.
func suffixStderr(stderr string) string {
	if stderr == "" {
		return ""
	}
	return ": " + stderr
}
