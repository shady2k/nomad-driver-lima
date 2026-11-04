# The box image and the platform bundle: recipes and build automation

This directory is the recipe for the box guest image -- Ubuntu 24.04 (noble)
with a toolchain a box can use -- and for the platform bundle that completes it:
the versioned tree the node mounts at `/opt/box`, which holds `boxd`, the
executors and the operator's add-ons. It also holds the automation that builds,
verifies and publishes both.

The repository keeps the recipes, never the image or the bundle: anyone builds
them for their own infrastructure, and versions are published separately, by the
operator, to their own registry or store. `docs/guest-image.md` is the contract
both have to meet, and what to do if you would rather bring your own image than
build one from this recipe.

| file | what it is |
| --- | --- |
| `provision.sh` | the whole guest recipe; runs as root inside a Lima instance of the cloud image |
| `box-build.yaml` | the Lima template `build.sh` starts: qemu, no host mounts, the recipe as a system provision |
| `boxd-source.sh` | resolves the `boxd` a bundle carries (and the image's stub unit is printed from), from a release, a commit, or this checkout, and records which |
| `pins.sh` | the versions and digests of the executors a bundle carries: the one place they are pinned |
| `platform.sh` | `images/platform.sh --version V --arch amd64\|arm64 --boxd PATH [--addons DIR]` builds `box-platform-<version>-linux-<arch>.tar.gz`, its `.sha256`, and prints the index entry |
| `build.sh` | `images/build.sh <arch> <version>` builds `out/box-base-<version>-<arch>.qcow2`, its `.sha256` and its manifest |
| `addons-pack.sh` | `images/addons-pack.sh SRC_DIR OUT.tar` packs extra files into the deterministic tar `platform.sh` installs at `/opt/box/addons` |
| `verify.sh` | `images/verify.sh <arch> <version>` boots a local (`IMAGE_PATH`) or published (`BOX_STORE_URL`) image with a platform bundle (`PLATFORM_ARCHIVE`) and checks it |
| `guest-checks.sh` | the checks `verify.sh` runs inside the guest: one PASS/FAIL line each, expected against actual |
| `box-boot.yaml` | the Lima template `verify.sh` boots the image with: the same shape the driver writes, the bundle's read-only mount included |
| `publish.sh` | `images/publish.sh [--platform ARCHIVE] <arch> <version>` uploads an image or a bundle, its `.sha256` and its manifest to your store, verifies them, then updates the store's `index.json` under a lock |
| `publish_test.py` | `python3 images/publish_test.py` runs `publish.sh` against a local fake store, images and bundles alike; nothing is published, and CI runs it |
| `platform_test.sh` | `images/platform_test.sh` builds bundles from fixtures served over 127.0.0.1 and checks the archive, the manifest, determinism, an arm64 build on an amd64 host and the refusals; CI runs it |
| `addons_pack_test.sh` | `images/addons_pack_test.sh` tests `addons-pack.sh`: the archive's paths, modes, owner and mtime, the JSON, and the refusals; CI runs it |

## Build, verify, publish

```sh
# the bundle: boxd of one recorded source, the executors images/pins.sh pins
images/boxd-source.sh amd64 /tmp/boxd
images/platform.sh --version 1.0.0 --arch amd64 --boxd /tmp/boxd/boxd --out out
# -> out/box-platform-1.0.0-linux-amd64.tar.gz + .sha256, and one JSON line

images/build.sh amd64 1.0.0                     # -> out/box-base-1.0.0-amd64.qcow2 + .sha256 + .json
PLATFORM_ARCHIVE=out/box-platform-1.0.0-linux-amd64.tar.gz \
IMAGE_PATH=out/box-base-1.0.0-amd64.qcow2 OUT=out \
  images/verify.sh amd64 1.0.0                  # boots it with the bundle mounted and runs the guest checks

export BOX_STORE_URL=https://store.example.com/box-images   # your store
export BOX_STORE_TOKEN_FILE=/run/secrets/store-token       # or BOX_STORE_TOKEN
images/publish.sh --platform out/box-platform-1.0.0-linux-amd64.tar.gz amd64 1.0.0
images/publish.sh amd64 1.0.0
BOX_STORE_URL=$BOX_STORE_URL images/verify.sh amd64 1.0.0   # the published copy
```

Nothing here publishes on its own, and nothing in CI uploads anywhere: a store
URL and its token are the operator's, and how the token is filled (a vault, a
secret manager, a mounted file) is theirs too.

## What the image is, and what the bundle is

The image is the OS and the toolchain: apt, docker with buildx and compose, git,
curl, jq, ripgrep, build-essential, python3, uv, Node 24 LTS and Go, each version
pinned in `provision.sh`. It carries **no boxd, no executor and no add-on**. What
it has for the platform is:

- an empty `/opt/box` (the mount point) and `/etc/profile.d/box-platform.sh`,
  which puts `/opt/box/bin` on the `PATH` of an interactive shell
  (`/etc/environment` does the same for the sessions sshd starts, which is how
  the driver reaches `boxctl`);
- `boxd.service`, enabled, running `/opt/box/bin/boxd` and requiring `/opt/box`
  as a mount. The unit is boxd's own -- `images/build.sh` copies the boxd of the
  recorded source in only to print it with `-print-unit`, and removes the binary
  again;
- `"platform": "mount"` in `/etc/box/image.json` and in the build manifest: the
  image saying it expects a bundle. `images/publish.sh` copies it into the
  image's index entry, and the CLI adds a `platform` block to a box's task config
  only for such an image;
- `guest/no-secrets.sh` passing before the disk is captured: no secret, no ssh
  host key, no authorized key, no machine-id, no shell history, cloud-init giving
  every box its own;
- 8 GiB of virtual disk by default (`DISK`), which is the floor for a box's
  `disk_gib`: Lima grows a disk and refuses to shrink one.

The bundle is what changes often: `bin/boxd`, `bin/boxctl`, `bin/prime-agent`,
`bin/opencode`, their `tools/<name>/` trees, the operator's `addons/`, and a
`manifest.json` naming the version, the boxd and the tool versions.
`docs/guest-image.md` has the layout and the rules an archive has to follow. Two
things about building it: it needs no VM and no root (the executors are fetched
as their own per-architecture tarballs, so an `arm64` bundle builds on an `amd64`
host), and the same inputs give the same bytes -- `--built-at` or
`SOURCE_DATE_EPOCH` fixes the one clock in it.

An image that carries its own boxd still works: `guest/boxd-install.sh` installs
`/usr/local/bin/boxd` and the unit for that layout
(`boxd -print-unit-installed`), and an image like that has no `platform` field,
so no bundle is mounted over its `/opt/box`.

## Which boxd the image or the bundle carries

One source, recorded in the manifest and in the image at `/etc/box/image.json`
(kind, tag or ref, resolved commit, binary sha256, version):

| setting | source |
| --- | --- |
| `BOXD_RELEASE=vX.Y.Z` | `boxd_linux_<arch>` and `SHA256SUMS` from that release of `BOXD_REPO` (default `shady2k/nomad-driver-lima`); the digest is checked and nothing is built |
| `BOXD_COMMIT=<sha or ref>` | `cmd/boxd` built from exactly that commit, with the version stamping of `deploy/build.sh` |
| neither | `cmd/boxd` from this checkout's HEAD commit; a tree with uncommitted changes under `cmd/` or `internal/` is refused unless `BOXD_ALLOW_DIRTY=1` says otherwise, and the build is then recorded as dirty |

`images/boxd-source.sh <arch> <dir>` does that resolution alone, if you want the
binary without a build. `images/platform.sh --boxd` takes the binary and reads
its version from the `boxd.json` written beside it (or from `--boxd-version`, or
from the binary itself when the build host can run it).

## The executors a bundle carries: `images/pins.sh`

`images/pins.sh` is the one place the executors' versions and digests are
written down: prime-agent's release version and the sha256 of each linux tarball,
opencode's npm version, the per-architecture npm package and its integrity. A
version bump is a new pin -- a download that does not match stops the build. A
bundle built for another set is a bundle built by other means;
`docs/guest-image.md` is what it has to look like.

## Extra files in every box: the bundle's add-ons

`--addons DIR` puts a directory on the build host into the bundle at
`/opt/box/addons`, so `DIR/foo/index.js` becomes `/opt/box/addons/foo/index.js`:

```sh
images/platform.sh --version 1.0.1 --arch amd64 --boxd /tmp/boxd/boxd \
  --addons ~/box-addons --out out
```

It is for code and data that the guest software loads from a local path -- a
program's plugin or extension package, a set of data files -- and it is not for
credentials: a bundle goes to anyone who can pull it, and `guest/no-secrets.sh`
covers `/opt/box` along with everything else before an image is captured.

`images/addons-pack.sh SRC_DIR OUT.tar` does the packing, and `--addons` reuses
it: regular files and directories only (a symlink, socket, fifo or device in the
tree stops it by name, and so does an empty directory), sorted entries, owner 0
and mtime 0, and normalised modes: directories 0755, files 0644, and 0755 for a
file that had any execute bit. One tree gives byte-identical archives, and the
script prints one JSON line:
`{"files": N, "bytes": B, "sha256": "<digest of the sorted file listing>"}`. It
needs GNU tar (`gtar` on macOS, from `gnu-tar`).

The bundle's manifest records that object as
`"addons": {"files": N, "bytes": B, "sha256": "..."}`, `images/guest-checks.sh`
checks in a booted box that nothing below `/opt/box/addons` is group- or
world-writable and that the file count is the one the bundle records, and without
`--addons` the manifest says `"addons": null` and there is no such directory.

Nothing in the recipe, boxd or the driver knows what is inside. A program that
loads plugins from a local path is pointed at `/opt/box/addons/<name>` in its own
settings, which the operator supplies with the box's executor configuration --
prime-agent's `settings.json` takes such an absolute path in its `packages` list,
and every other program has its own way. The bundle only has to put the files
there.

## Deployment settings, not repository settings

Every source has a public upstream default written down in `provision.sh` (the
image) or `pins.sh` and `platform.sh` (the bundle) -- Ubuntu's archive,
nodejs.org, go.dev, PyPI, GitHub, npm, Docker Hub -- and a `BOX_MIRROR_*`
variable moves it for a build: `BOX_MIRROR_APT`, `BOX_MIRROR_APT_SECURITY`,
`BOX_MIRROR_NPM`, `BOX_MIRROR_PYPI`, `BOX_MIRROR_GO`, `BOX_MIRROR_GO_DIST`,
`BOX_MIRROR_GITHUB`, `BOX_MIRROR_NODE`, `BOX_MIRROR_DOCKER` and
`BOX_MIRROR_CLOUDIMAGE`. A mirror serves the same bytes or the build stops: every
version is pinned and every archive is checked against its digest.
`BOX_STORE_URL`, `BOX_STORE_TOKEN`, `BOX_STORE_TOKEN_FILE`,
`BOX_STORE_TOKEN_HEADER` and `BOX_STORE_TOKEN_SCHEME` are the same kind of
setting for the store. This repository names no private host, mirror or token.

`BOX_MIRROR_GO` has one effect inside the box: the public Go proxy serves the
checksum database and a mirror usually does not, so `GOSUMDB=off` exactly when a
Go mirror is configured.

An arm64 image build on an x86_64 host runs under TCG and needs a lima that
carries the aarch64 guest agent (nixpkgs `lima-additional-guestagents`); on an
arm64 host neither is involved. An arm64 *bundle* build needs neither.
