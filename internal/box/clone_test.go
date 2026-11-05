package box

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/shady2k/nomad-driver-lima/internal/fakelima"
)

// A bake's clone is a copy of a stopped box with mounts of its own: the platform
// bundle, and two empty directories where boxd waits for the task files and the
// credentials. These tests drive the fake limactl through the real command line.

// cloneTestBox puts a stopped instance called box1 into the fake Lima home,
// with the mounts a box has: the task files and the credentials.
func cloneTestBox(t *testing.T, f *fakelima.Fake, mounts string) {
	t.Helper()
	dir := f.InstanceDir("box1")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "disk"), []byte("BOX-DISK"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "lima.yaml"), []byte(`minimumLimaVersion: "2.0.0"
vmType: qemu
arch: x86_64
cpus: 2
memory: 1024MiB
disk: 8GiB
mountType: 9p
`+mounts), 0o644))
	f.SetState(t, "box1", map[string]any{"status": "Stopped", "cpus": 2})
}

// boxMounts are the mounts a box's own configuration has, which a clone must not.
const boxMounts = `mounts:
  - location: /run/box-files/box1/local
    mountPoint: /etc/box/local
    writable: false
  - location: /run/box-files/box1/secrets
    mountPoint: /run/box/secrets
    writable: false
`

// TestCloneForBakeCopiesTheBoxAndDropsItsMounts: the clone has a disk of its own
// -- a copy, not the box's -- and mounts only the bundle and the stand-ins.
func TestCloneForBakeCopiesTheBoxAndDropsItsMounts(t *testing.T) {
	f := fakelima.New(t)
	m := newTestManager(t, f)
	cloneTestBox(t, f, boxMounts)
	platform := t.TempDir()

	clone, err := m.CloneForBake(context.Background(), "box1", "box1-bake-1", platform)
	require.NoError(t, err)
	require.Equal(t, "box1-bake-1", clone.Name)

	// A copy, not the box's disk: same bytes, another file.
	data, err := os.ReadFile(clone.Disk())
	require.NoError(t, err)
	require.Equal(t, "BOX-DISK", string(data))
	target, err := os.Readlink(clone.Disk())
	require.Error(t, err, "the clone's disk must be a file of its own, not a symlink (%q)", target)

	// The mounts are the bundle and the two stand-ins, and nothing of the box.
	doc, err := readLimaYAML(clone.InstanceDir())
	require.NoError(t, err)
	mounts, _ := doc["mounts"].([]any)
	require.Len(t, mounts, 3)
	byPoint := map[string]string{}
	for _, raw := range mounts {
		entry := raw.(map[string]any)
		byPoint[entry["mountPoint"].(string)] = entry["location"].(string)
	}
	require.Equal(t, platform, byPoint[PlatformMountPoint])
	require.Equal(t, filepath.Join(clone.InstanceDir(), cloneMountsDir, "secrets"), byPoint[DefaultSecretsDirInGuest])
	require.Equal(t, filepath.Join(clone.InstanceDir(), cloneMountsDir, "local"), byPoint["/etc/box/local"])
	for point, location := range byPoint {
		require.NotContains(t, location, "/run/box-files/box1", "the clone mounts %s from the box's own files", point)
	}
	// The stand-ins are there and empty, which is what boxd needs to start.
	for _, part := range []string{"local", "secrets"} {
		entries, err := os.ReadDir(filepath.Join(clone.InstanceDir(), cloneMountsDir, part))
		require.NoError(t, err)
		require.Empty(t, entries)
	}
	// limactl was told to drop the source's mounts altogether.
	require.True(t, f.HasCall(t, "clone --tty=false --mount-none box1 box1-bake-1"))
}

// TestCloneForBakeReplacesACloneDiskThatIsASymlink: the one thing a bake must be
// sure of is that the seal empties a copy. A clone whose disk points at the box's
// own volume disk is replaced by a real copy before anything starts.
func TestCloneForBakeReplacesACloneDiskThatIsASymlink(t *testing.T) {
	f := fakelima.New(t)
	m := newTestManager(t, f)
	cloneTestBox(t, f, boxMounts)
	f.SetMode(t, map[string]any{"clone_symlink_disk": true})

	clone, err := m.CloneForBake(context.Background(), "box1", "box1-bake-1", t.TempDir())
	require.NoError(t, err)
	link, err := os.Readlink(clone.Disk())
	require.Error(t, err, "the clone's disk is still a symlink to %q", link)
	data, err := os.ReadFile(clone.Disk())
	require.NoError(t, err)
	require.Equal(t, "BOX-DISK", string(data))
}

// TestNextBakeCloneNameCountsAboveTheClonesThere: a clone left by a killed bake
// must not have its name taken by the next one.
func TestNextBakeCloneNameCountsAboveTheClonesThere(t *testing.T) {
	require.Equal(t, "box1-bake-1", NextBakeCloneName("box1", nil))
	require.Equal(t, "box1-bake-3", NextBakeCloneName("box1", []string{"box1-bake-1", "box1-bake-2"}))
	// Only the clones of this box count.
	require.Equal(t, "box1-bake-4", NextBakeCloneName("box1", []string{"box1-bake-1", "box1-bake-3", "box2-bake-9"}))
	require.Equal(t, "box1-bake-2", NextBakeCloneName("box1", []string{"box1-bake-1"}))
}

// TestBakeClonesFindsOnlyThisBoxsClones: the leftovers of a killed bake are the
// instances named <box>-bake-<n>, and the next bake removes them.
func TestBakeClonesFindsOnlyThisBoxsClones(t *testing.T) {
	f := fakelima.New(t)
	m := newTestManager(t, f)
	cloneTestBox(t, f, boxMounts)
	for _, name := range []string{"box1-bake-1", "box1-bake-2", "box2-bake-1", "box10-bake-1"} {
		require.NoError(t, os.MkdirAll(f.InstanceDir(name), 0o755))
		f.SetState(t, name, map[string]any{"status": "Stopped"})
	}
	clones, err := m.BakeClones(context.Background(), "box1")
	require.NoError(t, err)
	require.Equal(t, []string{"box1-bake-1", "box1-bake-2"}, clones)
}

// TestDiskUsedIsTheBlocksOfTheFile: what a copy of a disk needs is what the file
// occupies, not the size it may grow to.
func TestDiskUsedIsTheBlocksOfTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk")
	require.NoError(t, os.WriteFile(path, []byte("QFI\xfb and some clusters"), 0o644))
	used, ok := DiskUsed(path)
	require.True(t, ok)
	require.Greater(t, used, int64(0))
	_, ok = DiskUsed(filepath.Join(t.TempDir(), "absent"))
	require.False(t, ok)
}

// TestFreeBytesIsPositive: the room a bake's copy has is measured on the
// filesystem of the Lima home.
func TestFreeBytesIsPositive(t *testing.T) {
	free, err := FreeBytes(t.TempDir())
	require.NoError(t, err)
	require.Greater(t, free, int64(0))
	_, err = FreeBytes(filepath.Join(t.TempDir(), "absent"))
	require.Error(t, err)
}

// TestCloneSealParsesTheSealStream: boxctl streams a line per step and then the
// record; the clone reports the steps and fails on the error document.
func TestCloneSealParsesTheSealStream(t *testing.T) {
	f := fakelima.New(t)
	m := newTestManager(t, f)
	cloneTestBox(t, f, boxMounts)
	clone, err := m.CloneForBake(context.Background(), "box1", "box1-bake-1", t.TempDir())
	require.NoError(t, err)

	var lines []string
	require.NoError(t, clone.Seal(context.Background(), CloneSeal{
		Name: "project", Version: "project-1", Parent: "1.0.0", Arch: "amd64", Recipe: []byte("#!/bin/sh\ntrue\n"),
	}, func(line string) { lines = append(lines, line) }))
	require.Contains(t, strings.Join(lines, "\n"), "guest/no-secrets.sh: 21 check(s) passed")
	require.Contains(t, strings.Join(lines, "\n"), "the workspace: deleted /work")

	// The recipe went in on stdin, to the clone, and the seal got the name.
	seals := f.SealCalls(t)
	require.Len(t, seals, 1)
	require.True(t, strings.HasPrefix(seals[0], "box1-bake-1\t"), "the seal ran in the clone: %q", seals[0])
	require.Contains(t, seals[0], "\t15\t", "the seal got the recipe on stdin: %q", seals[0])

	// A seal that fails reports what boxctl said, in its own words.
	f.SetMode(t, map[string]any{"fail_seal": "guest/no-secrets.sh failed in this box: FAIL /root/.prime"})
	err = clone.Seal(context.Background(), CloneSeal{Name: "project", Arch: "amd64"}, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "guest/no-secrets.sh failed in this box")
}

// TestBakeClonesDeleteAHalfCreatedClone: `limactl` refuses to delete an instance
// directory it cannot read a lima.yaml from -- "in case it is currently being
// cloned into" -- which is what a bake killed halfway through a clone leaves. The
// clone's directory holds a copy of the box's disk, so it goes.
func TestBakeClonesDeleteAHalfCreatedClone(t *testing.T) {
	f := fakelima.New(t)
	m := newTestManager(t, f)
	dir := f.InstanceDir("box1-bake-1")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, cloneMountsDir, "secrets"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "disk"), []byte("HALF A COPY"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(f.Home, ".fake-state.json"), []byte(
		`{"box1-bake-1":{"status":"Broken"}}`), 0o644))

	require.NoError(t, m.BakeCloneOf("box1-bake-1").Delete(context.Background()))
	_, err := os.Stat(dir)
	require.True(t, os.IsNotExist(err), "the half created clone is still there")
}
