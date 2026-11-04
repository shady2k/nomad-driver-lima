package box

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/shady2k/nomad-driver-lima/internal/fakelima"
)

// fakeQEMUImg writes a qemu-img that records its arguments next to itself and
// copies its input to its output, which is what the conversion promises. fail
// makes it exit non-zero after writing half of the output.
func fakeQEMUImg(t *testing.T, dir string, fail bool) (string, string) {
	t.Helper()
	path := filepath.Join(dir, "qemu-img")
	log := filepath.Join(dir, "qemu-img.log")
	body := "#!/bin/sh\n"
	body += "printf '%s\\n' \"$*\" >> \"$(dirname \"$0\")/qemu-img.log\"\n"
	if fail {
		body += "printf 'half an image' > \"$5\"\nexit 1\n"
	} else {
		body += "printf 'uncompressed:' > \"$5\"\ncat \"$4\" >> \"$5\"\n"
	}
	require.NoError(t, os.WriteFile(path, []byte(body), 0o755))
	return path, log
}

// TestEnsureImageDiskConvertsOnce is what makes a box disk uncompressed: the
// published image is compressed, so the first box converts the image into the
// cache and every box after it reuses that copy.
func TestEnsureImageDiskConvertsOnce(t *testing.T) {
	f := fakelima.New(t)
	m := newTestManager(t, f)
	binDir := t.TempDir()
	qemuImg, calls := fakeQEMUImg(t, binDir, false)
	m.opts.QEMUImg = qemuImg
	vol := volumeDir(t)
	var hits int64
	payload := "COMPRESSED-IMAGE"
	sum := sha256.Sum256([]byte(payload))
	url := imageURL(t, payload, &hits)
	spec := testSpec(t, vol)
	spec.ImageURL = url
	spec.ImageSHA256 = hex.EncodeToString(sum[:])
	rep, events := collectEvents()

	b, err := m.Box(spec, rep)
	require.NoError(t, err)
	diskImage, err := b.EnsureImageDisk(context.Background(), spec.ImageURL, spec.ImageSHA256, nil)
	require.NoError(t, err)

	want := filepath.Join(m.opts.ImageCacheDir, spec.ImageSHA256+uncompressedSuffix)
	require.Equal(t, want, diskImage)
	require.Equal(t, "uncompressed:COMPRESSED-IMAGE", readFile(t, want))

	// The conversion is not compressed: no -c, and no preallocation option.
	recorded := readFile(t, calls)
	require.Contains(t, recorded, "convert -O qcow2 ")
	require.NotContains(t, recorded, " -c ")
	parts := strings.Fields(strings.TrimSpace(recorded))
	require.Equal(t, "convert", parts[0])
	require.Equal(t, []string{"-O", "qcow2"}, parts[1:3])
	require.Equal(t, filepath.Join(m.opts.ImageCacheDir, spec.ImageSHA256+".img"), parts[3])
	require.True(t, strings.HasPrefix(parts[4], want+"."), "the conversion writes a temporary file that is renamed into the cache: %s", parts[4])
	require.NotContains(t, strings.Join(*events, "\n"), "stays compressed")

	// A second box made from the same image reuses the copy: one conversion, one
	// download.
	second, err := m.Box(spec, rep)
	require.NoError(t, err)
	again, err := second.EnsureImageDisk(context.Background(), spec.ImageURL, spec.ImageSHA256, nil)
	require.NoError(t, err)
	require.Equal(t, want, again)
	require.Equal(t, 1, strings.Count(readFile(t, calls), "convert"), "a cached conversion must not run again")
	require.Equal(t, int64(1), atomic.LoadInt64(&hits))
	require.Contains(t, strings.Join(*events, "\n"), "reused by every box on this node")

	// Nothing of the temporary file stays behind.
	entries, err := os.ReadDir(m.opts.ImageCacheDir)
	require.NoError(t, err)
	for _, entry := range entries {
		require.NotContains(t, entry.Name(), ".part", "a finished conversion leaves no temporary file")
	}
}

// TestEnsureImageDiskWithoutQEMUImg keeps a node without qemu-img working: the
// box starts from the compressed image, and the task event says why.
func TestEnsureImageDiskWithoutQEMUImg(t *testing.T) {
	f := fakelima.New(t)
	m := newTestManager(t, f)
	m.opts.QEMUImg = filepath.Join(t.TempDir(), "no-such-qemu-img")
	vol := volumeDir(t)
	spec := testSpec(t, vol)
	rep, events := collectEvents()

	b, err := m.Box(spec, rep)
	require.NoError(t, err)
	got, err := b.EnsureImageDisk(context.Background(), spec.ImageURL, spec.ImageSHA256, nil)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(m.opts.ImageCacheDir, spec.ImageSHA256+".img"), got)
	require.Contains(t, strings.Join(*events, "\n"), "Box disk stays compressed")
	require.Contains(t, strings.Join(*events, "\n"), "no-such-qemu-img is not installed")
}

// TestEnsureImageDiskKeepsAHalfConversionOutOfTheCache: a qemu-img that fails
// must leave nothing a later box could boot from.
func TestEnsureImageDiskKeepsAHalfConversionOutOfTheCache(t *testing.T) {
	f := fakelima.New(t)
	m := newTestManager(t, f)
	qemuImg, _ := fakeQEMUImg(t, t.TempDir(), true)
	m.opts.QEMUImg = qemuImg
	vol := volumeDir(t)
	spec := testSpec(t, vol)
	rep, events := collectEvents()

	b, err := m.Box(spec, rep)
	require.NoError(t, err)
	got, err := b.EnsureImageDisk(context.Background(), spec.ImageURL, spec.ImageSHA256, nil)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(m.opts.ImageCacheDir, spec.ImageSHA256+".img"), got)
	require.NoFileExists(t, filepath.Join(m.opts.ImageCacheDir, spec.ImageSHA256+uncompressedSuffix))
	require.Contains(t, strings.Join(*events, "\n"), "runs again for the next box")
	entries, err := os.ReadDir(m.opts.ImageCacheDir)
	require.NoError(t, err)
	for _, entry := range entries {
		require.NotContains(t, entry.Name(), ".part")
	}
}

// TestUncompressGivesPlainClusters runs the conversion with a real qemu-img,
// when the host has one: a compressed image goes in, and the box disk is built
// from an image whose clusters are not compressed any more.
func TestUncompressGivesPlainClusters(t *testing.T) {
	qemuImg, err := exec.LookPath("qemu-img")
	if err != nil {
		t.Skip("qemu-img is not installed on this host")
	}
	f := fakelima.New(t)
	m := newTestManager(t, f)
	m.opts.QEMUImg = qemuImg

	// A raw image with 4 MiB of compressible text at the start and the rest
	// never written: zlib packs the text into a few hundred KiB, and the file
	// stays sparse, which is what the thinness check below needs.
	raw := filepath.Join(t.TempDir(), "plain.raw")
	file, err := os.Create(raw)
	require.NoError(t, err)
	line := []byte("the quick brown fox jumps over the lazy dog 0123456789\n")
	for i := 0; i < 1<<16; i++ { // 4 MiB of text
		_, err := file.Write(line)
		require.NoError(t, err)
	}
	require.NoError(t, file.Truncate(64<<20))
	require.NoError(t, file.Close())
	src := filepath.Join(m.opts.ImageCacheDir, strings.Repeat("a", 64)+".img")
	require.NoError(t, os.MkdirAll(m.opts.ImageCacheDir, 0o755))
	out, err := exec.Command(qemuImg, "convert", "-c", "-O", "qcow2", raw, src).CombinedOutput()
	require.NoError(t, err, string(out))
	require.True(t, hasCompressedClusters(t, qemuImg, src), "the test needs a compressed input image")

	dest := filepath.Join(m.opts.ImageCacheDir, strings.Repeat("a", 64)+uncompressedSuffix)
	b, err := m.Box(Spec{BoxID: "box1", VolumeDir: t.TempDir(), ImageURL: "http://example.com/i.img",
		ImageSHA256: strings.Repeat("a", 64), CPUs: 1, MemoryMiB: 512, DiskGiB: 1}, nil)
	require.NoError(t, err)
	require.NoError(t, b.uncompress(context.Background(), src, dest))
	require.FileExists(t, dest)
	require.False(t, hasCompressedClusters(t, qemuImg, dest), "a box disk must not be built from compressed clusters")

	// Thin: the copy allocates what the data needs, not the virtual size.
	info, err := exec.Command(qemuImg, "info", "--output=json", dest).Output()
	require.NoError(t, err)
	var doc struct {
		VirtualSize int64 `json:"virtual-size"`
		ActualSize  int64 `json:"actual-size"`
	}
	require.NoError(t, json.Unmarshal(info, &doc))
	require.Less(t, doc.ActualSize, doc.VirtualSize/2, "an uncompressed copy stays thin: %d of %d bytes allocated", doc.ActualSize, doc.VirtualSize)
}

// hasCompressedClusters reports whether qemu-img finds a compressed cluster in
// the image.
func hasCompressedClusters(t *testing.T, qemuImg, path string) bool {
	t.Helper()
	out, err := exec.Command(qemuImg, "map", "--output=json", path).Output()
	require.NoError(t, err)
	var regions []struct {
		Compressed bool `json:"compressed"`
	}
	require.NoError(t, json.Unmarshal(out, &regions))
	for _, region := range regions {
		if region.Compressed {
			return true
		}
	}
	return false
}
