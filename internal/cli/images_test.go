package cli

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseImageIndexCanonical(t *testing.T) {
	index, err := ParseImageIndex([]byte(indexJSON))
	require.NoError(t, err)
	require.Equal(t, 1, index.Schema)
	require.Equal(t, "2026-11-01T10:00:00Z", index.UpdatedAt)
	require.Equal(t, []string{"1.0.0"}, index.Versions())

	artifact, err := index.Resolve("1.0.0", "amd64")
	require.NoError(t, err)
	require.Equal(t, "1.0.0", artifact.Version)
	require.Equal(t, "amd64", artifact.Arch)
	require.Equal(t, int64(1056047104), artifact.Size)
	require.Contains(t, artifact.URL, "box-base-1.0.0-amd64.qcow2")

	// amd64 comes before arm64 in the listing.
	artifacts := index.Artifacts()
	require.Len(t, artifacts, 2)
	require.Equal(t, "amd64", artifacts[0].Arch)
	require.Equal(t, "arm64", artifacts[1].Arch)
}

func TestParseImageIndexShapes(t *testing.T) {
	listShape := `{"images": [{"version": "2.0.0", "arches": {"x86_64": {"url": "u", "sha256": "s", "size": 1}}}]}`
	index, err := ParseImageIndex([]byte(listShape))
	require.NoError(t, err)
	artifact, err := index.Resolve("2.0.0", "amd64")
	require.NoError(t, err)
	require.Equal(t, "u", artifact.URL)

	dotShape := `{"versions": {"0.9": {"amd64": {"url": "u2", "sha256": "s2"}}}}`
	index, err = ParseImageIndex([]byte(dotShape))
	require.NoError(t, err)
	artifact, err = index.Resolve("0.9", "amd64")
	require.NoError(t, err)
	require.Equal(t, "u2", artifact.URL)

	withNoise := `{"images": {"1.1": {"built_at": "2026-01-01", "amd64": {"url": "u3", "sha256": "s3"}}}}`
	index, err = ParseImageIndex([]byte(withNoise))
	require.NoError(t, err)
	_, err = index.Resolve("1.1", "amd64")
	require.NoError(t, err)

	_, err = ParseImageIndex([]byte("not json"))
	require.Error(t, err)
	_, err = ParseImageIndex([]byte(`{"schema": 1}`))
	require.Error(t, err)
	require.Contains(t, err.Error(), `no "images" object`)
}

func TestImageIndexVersionOrder(t *testing.T) {
	body := `{"images": {
	  "1.9.0": {"amd64": {"url": "u", "sha256": "s"}},
	  "1.10.0": {"amd64": {"url": "u", "sha256": "s"}},
	  "1.2.0": {"amd64": {"url": "u", "sha256": "s"}}
	}}`
	index, err := ParseImageIndex([]byte(body))
	require.NoError(t, err)
	require.Equal(t, []string{"1.10.0", "1.9.0", "1.2.0"}, index.Versions())
}

func TestImageIndexResolveErrors(t *testing.T) {
	index, err := ParseImageIndex([]byte(indexJSON))
	require.NoError(t, err)

	_, err = index.Resolve("9.9.9", "amd64")
	require.Error(t, err)
	require.Equal(t, ExitNotFound, ExitCode(err))
	require.Contains(t, err.Error(), "published versions: 1.0.0")

	_, err = index.Resolve("1.0.0", "riscv64")
	require.Error(t, err)
	require.Equal(t, ExitNotFound, ExitCode(err))
	require.Contains(t, err.Error(), "it has: amd64, arm64")

	// "latest" resolves to the newest version that has the architecture.
	latest, err := index.Resolve("latest", "arm64")
	require.NoError(t, err)
	require.Equal(t, "1.0.0", latest.Version)
	require.Equal(t, "arm64", latest.Arch)
}

func TestArtifactValidate(t *testing.T) {
	digest := strings.Repeat("a1b2c3d4", 8)
	good := &ImageArtifact{Version: "1", Arch: "amd64", URL: "u", SHA256: "SHA256:" + strings.ToUpper(digest)}
	require.NoError(t, good.Validate())
	require.Equal(t, digest, good.SHA256, "the prefix and the case are normalised")

	require.Error(t, (&ImageArtifact{URL: "u"}).Validate())
	require.Error(t, (&ImageArtifact{SHA256: good.SHA256}).Validate())
	require.Error(t, (&ImageArtifact{URL: "u", SHA256: "abc"}).Validate())
	require.Error(t, (&ImageArtifact{URL: "u", SHA256: "zz" + good.SHA256[2:]}).Validate())
}

func TestLoadImageIndex(t *testing.T) {
	ctx := context.Background()

	index, err := LoadImageIndex(ctx, &fakeImages{body: indexJSON, status: 200}, "http://x/index.json")
	require.NoError(t, err)
	require.Len(t, index.Versions(), 1)

	_, err = LoadImageIndex(ctx, &fakeImages{status: http.StatusNotFound}, "http://x/index.json")
	require.True(t, errors.Is(err, ErrIndexMissing))
	require.Contains(t, err.Error(), "HTTP 404")

	_, err = LoadImageIndex(ctx, &fakeImages{status: http.StatusInternalServerError}, "http://x/index.json")
	require.Error(t, err)
	require.Contains(t, err.Error(), "HTTP 500")

	_, err = LoadImageIndex(ctx, &fakeImages{err: errors.New("no route to host")}, "http://x/index.json")
	require.Error(t, err)
	require.Contains(t, err.Error(), "no route to host")
}

func TestNormalizeArch(t *testing.T) {
	require.Equal(t, "amd64", NormalizeArch("x86_64"))
	require.Equal(t, "amd64", NormalizeArch("AMD64"))
	require.Equal(t, "arm64", NormalizeArch("aarch64"))
	require.Equal(t, "arm64", NormalizeArch(" arm64 "))
	require.Equal(t, "riscv64", NormalizeArch("riscv64"))
}
