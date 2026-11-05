package driver

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/hashicorp/nomad/client/allocdir"
	"github.com/hashicorp/nomad/plugins/drivers"

	"github.com/shady2k/nomad-driver-lima/internal/box"
)

// taskFilesDigest fingerprints the files of a task's local and secrets
// directories.
//
// The driver copies those files into the directories the guest mounts. Nomad
// re-renders a template whose Nomad Variable changed (change_mode = "noop", so a
// credential rotation never restarts the VM), and the driver then has to notice
// and refresh the guest's copy.
//
// The digest is the content of every file, not its size and modification time: a
// re-render that writes the same bytes is not a change, and a driver update that
// re-attaches to a running box must not read every file as new and interrupt the
// work in it. A file rewritten with the same bytes therefore stages nothing and
// reloads nothing.
func taskFilesDigest(tc *drivers.TaskConfig) string {
	if tc == nil {
		return ""
	}
	return dirsDigest(tc.TaskDir().LocalDir, tc.TaskDir().SecretsDir)
}

// stagedFilesDigest fingerprints the files the guest is mounted now: the box's
// host directories, which StageTaskFiles filled. It is the same digest over the
// same layout as taskFilesDigest, so a recovered task compares what is there
// with what Nomad rendered, and stages nothing when they agree.
func stagedFilesDigest(b *box.Box) string {
	if b == nil {
		return ""
	}
	local, secrets := b.TaskFilesDirs()
	return dirsDigest(local, secrets)
}

// dirsDigest hashes every regular file below the given roots by the name of the
// root it is in, its path below that root and the sha256 of its content.
func dirsDigest(roots ...string) string {
	h := sha256.New()
	for _, root := range roots {
		if root == "" {
			continue
		}
		var entries []string
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !d.Type().IsRegular() {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			sum := sha256.Sum256(data)
			entries = append(entries, fmt.Sprintf("%s\x00%s\x00%s", filepath.Base(root), rel, hex.EncodeToString(sum[:])))
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
// running box (see docs/design-decisions.md). A re-render that changed no byte
// does nothing here at all: no file is written and boxd is not called, so a
// driver update cannot restart an executor under a running turn.
func (d *Driver) refreshTaskFiles(h *taskHandle, b *box.Box) {
	if h == nil || b == nil {
		return
	}
	digest := taskFilesDigest(h.tc)
	if digest == "" {
		return
	}
	// The digest of what is there is taken before the comparison, and it is the
	// answer to "does the guest have these files": a task recovered after a
	// driver restart carries no record of them, and a Nomad re-render that wrote
	// the same bytes must not stage anything or restart an executor.
	if stagedFilesDigest(b) == digest {
		return
	}
	n, err := b.StageTaskFiles(h.tc.TaskDir().LocalDir, h.tc.TaskDir().SecretsDir, taskPrivateDir(h.tc))
	if err != nil {
		d.emit(h.tc, "files", fmt.Sprintf("Box %s: the task files changed, but they could not be put where the guest mounts them: %v", h.boxID, err))
		return
	}
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

// taskPrivateDir is the task's private directory on the host. TaskConfig.TaskDir
// leaves its PrivateDir empty, although Nomad makes the directory, so a host that
// stages there has to name it itself.
func taskPrivateDir(tc *drivers.TaskConfig) string {
	return filepath.Join(tc.TaskDir().Dir, allocdir.TaskPrivate)
}
