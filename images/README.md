# The box base image: recipe and build automation

This directory is the recipe for the box guest image -- Ubuntu 24.04 (noble)
with a toolchain a box can use -- and the automation that builds, verifies and
publishes it.

The repository keeps the recipe, never the binary image: anyone builds an image
for their own infrastructure, and image versions are published separately, by
the operator, to their own registry or store. `docs/guest-image.md` is the
contract an image has to meet, and what to do if you would rather bring your own
image than build one from this recipe.

| file | what it is |
| --- | --- |
| `provision.sh` | the whole guest recipe; runs as root inside a Lima instance of the cloud image |
| `box-build.yaml` | the Lima template `build.sh` starts: qemu, no host mounts, the recipe as a system provision |
| `boxd-source.sh` | resolves the `boxd` the image carries, from a release, a commit, or this checkout, and records which |
| `build.sh` | `images/build.sh <arch> <version>` builds `out/box-base-<version>-<arch>.qcow2`, its `.sha256` and its manifest |
| `verify.sh` | `images/verify.sh <arch> <version>` boots a local (`IMAGE_PATH`) or published (`BOX_STORE_URL`) image and checks it |
| `guest-checks.sh` | the checks `verify.sh` runs inside the guest: one PASS/FAIL line each, expected against actual |
| `box-boot.yaml` | the Lima template `verify.sh` boots the image with: the same shape the driver writes |
| `publish.sh` | `images/publish.sh <arch> <version>` uploads the image, its `.sha256` and manifest to your store, verifies them, then updates the store's `index.json` under a lock |
| `publish_test.py` | `python3 images/publish_test.py` runs `publish.sh` against a local fake store; nothing is published, and CI runs it |

## Build, verify, publish

```sh
images/build.sh amd64 1.0.0                     # -> out/box-base-1.0.0-amd64.qcow2 + .sha256 + .json
IMAGE_PATH=out/box-base-1.0.0-amd64.qcow2 OUT=out \
  images/verify.sh amd64 1.0.0                  # boots it and runs the guest checks

export BOX_STORE_URL=https://store.example.com/box-images   # your store
export BOX_STORE_TOKEN_FILE=/run/secrets/store-token       # or BOX_STORE_TOKEN
images/publish.sh amd64 1.0.0
BOX_STORE_URL=$BOX_STORE_URL images/verify.sh amd64 1.0.0   # the published copy
```

Nothing here publishes on its own, and nothing in CI uploads anywhere: a store
URL and its token are the operator's, and how the token is filled (a vault, a
secret manager, a mounted file) is theirs too.

## Which boxd the image carries

One source, recorded in the build manifest and in the image at
`/etc/box/image.json` (kind, tag or ref, resolved commit, binary sha256, version):

| setting | source |
| --- | --- |
| `BOXD_RELEASE=vX.Y.Z` | `boxd_linux_<arch>` and `SHA256SUMS` from that release of `BOXD_REPO` (default `shady2k/nomad-driver-lima`); the digest is checked and nothing is built |
| `BOXD_COMMIT=<sha or ref>` | `cmd/boxd` built from exactly that commit, with the version stamping of `deploy/build.sh` |
| neither | `cmd/boxd` from this checkout's HEAD commit; a tree with uncommitted changes under `cmd/` or `internal/` is refused unless `BOXD_ALLOW_DIRTY=1` says otherwise, and the image is then recorded as dirty |

`images/boxd-source.sh <arch> <dir>` does that resolution alone, if you want the
binary without a full image build.

## Deployment settings, not repository settings

Every source has a public upstream default written down in `provision.sh`
(Ubuntu's archive, nodejs.org, go.dev, PyPI, GitHub, Docker Hub) and a
`BOX_MIRROR_*` variable that moves it for a build: `BOX_MIRROR_APT`,
`BOX_MIRROR_APT_SECURITY`, `BOX_MIRROR_NPM`, `BOX_MIRROR_PYPI`, `BOX_MIRROR_GO`,
`BOX_MIRROR_GO_DIST`, `BOX_MIRROR_GITHUB`, `BOX_MIRROR_NODE`,
`BOX_MIRROR_DOCKER` and `BOX_MIRROR_CLOUDIMAGE`. A mirror serves the same bytes
or the build stops: every version is pinned and every archive is checked against
its sha256. `BOX_STORE_URL`, `BOX_STORE_TOKEN`, `BOX_STORE_TOKEN_FILE`,
`BOX_STORE_TOKEN_HEADER` and `BOX_STORE_TOKEN_SCHEME` are the same kind of
setting for the store. This repository names no private host, mirror or token.

`BOX_MIRROR_GO` has one effect inside the box: the public Go proxy serves the
checksum database and a mirror usually does not, so `GOSUMDB=off` exactly when a
Go mirror is configured.

## What the image is

- apt from the archive and the security archive only (suites `noble` and
  `noble-security`; cloud-init is told to keep those sources), docker with buildx
  and compose, git, curl, jq, ripgrep, build-essential, python3, uv, Node 24 LTS,
  Go stable, prime-agent and opencode -- each version pinned in `provision.sh`.
- `boxd` and `boxctl` in `/usr/local/bin` through `guest/boxd-install.sh`, with
  `boxd.service` enabled for the first boot.
- no secret, no ssh host key, no authorized key, no machine-id, no shell history:
  cloud-init gives every box its own, and `guest/no-secrets.sh` is the check that
  proves it, both before the disk is captured and in a booted box.
- 8 GiB virtual disk by default (`DISK`), which is the floor for a box's
  `disk_gib`: Lima grows a disk and refuses to shrink one.

An arm64 build on an x86_64 host runs under TCG and needs a lima that carries the
aarch64 guest agent (nixpkgs `lima-additional-guestagents`); on an arm64 host
neither is involved.
