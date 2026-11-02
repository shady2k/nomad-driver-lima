# Box base images

The box guest image: Ubuntu 24.04 (noble) with its package sources pointed at a
store you choose, built from a recipe that is the whole of it and published to
any HTTP PUT store, where the `box` driver finds it as `image { url, sha256 }`
through an image index.

| file | what it is |
| --- | --- |
| `provision.sh` | the whole guest recipe; runs as root inside a Lima instance of the cloud image |
| `box-build.yaml` | the Lima template `build.sh` starts: qemu, no host mounts, the recipe as a system provision |
| `build.sh` | `images/build.sh <arch> <version>` builds `out/box-base-<version>-<arch>.qcow2` and its `.sha256`, manifest and report |
| `publish.sh` | `images/publish.sh <arch> <version>` uploads the image, its `.sha256` and manifest, verifies them, then updates `box-base/index.json` under a lock |
| `publish_test.py` | `python3 images/publish_test.py` runs `publish.sh` against a local fake store (nothing is published) |
| `box-boot.yaml` | the Lima template `verify.sh` boots the published image with: the same shape the driver writes |
| `guest-checks.sh` | the checks a published image must pass, run inside the guest by `verify.sh` |
| `no-secrets.sh` | proves that an image carries no credential; `build.sh` runs it before it captures the disk, `guest-checks.sh` through `verify.sh` |
| `verify.sh` | `images/verify.sh <arch> <version>` downloads the published image, verifies its digest, boots it and runs `guest-checks.sh` |

A full run of the checks the image needs, against a store of your own:

```sh
export BOX_STORE_URL=https://store.example.com/box-images
export BOX_STORE_TOKEN_FILE=/run/secrets/store-token   # or BOX_STORE_TOKEN

images/build.sh amd64 1.0.0
images/publish.sh amd64 1.0.0
images/verify.sh amd64 1.0.0
```

`build.sh` and `publish.sh` take no interactive input: every input is an
argument or an environment variable, so the same scripts run as a Nomad batch
job on a node of the matching architecture.

## Where the packages come from

Every source is the project's own public download host unless a `BOX_MIRROR_*`
variable points it at a mirror (a closed network, a caching proxy). The versions
and their `sha256` pins stay in force either way: a mirror serves the same bytes
or the build stops.

| variable | what it moves | default |
| --- | --- | --- |
| `BOX_MIRROR_APT` | the Ubuntu archive (and the security suite, unless `BOX_MIRROR_APT_SECURITY` is set too) | `http://archive.ubuntu.com/ubuntu`, `http://ports.ubuntu.com/ubuntu-ports` on arm64 |
| `BOX_MIRROR_APT_SECURITY` | the Ubuntu security archive | `http://security.ubuntu.com/ubuntu`, the arm64 ports archive |
| `BOX_MIRROR_NPM` | the npm registry | `https://registry.npmjs.org/` |
| `BOX_MIRROR_PYPI` | the pip and uv index | `https://pypi.org/simple` |
| `BOX_MIRROR_GO` | the Go module proxy | `https://proxy.golang.org` |
| `BOX_MIRROR_GO_DIST` | the Go release tarballs | `https://go.dev/dl` |
| `BOX_MIRROR_GITHUB` | GitHub release assets (prime-agent) | `https://github.com` |
| `BOX_MIRROR_NODE` | the Node.js release files | `https://nodejs.org/dist` |
| `BOX_MIRROR_DOCKER` | a Docker Hub registry mirror in `/etc/docker/daemon.json` | none: the daemon pulls from Docker Hub |
| `BOX_MIRROR_CLOUDIMAGE` | the base cloud image (`build.sh` only) | `https://cloud-images.ubuntu.com/releases/noble/release` |

One difference reaches the box: `GOSUMDB`. The public Go proxy serves the
checksum database, so a box verifies module sums against `sum.golang.org`; a
mirror usually does not serve it, so `GOSUMDB=off` when `BOX_MIRROR_GO` is set.

`images/build.sh` passes the variables it was given to the recipe; the recipe
itself holds the defaults, so a partial build with one mirror set uses the public
upstreams for everything else. The manifest records every source the build used
under `sources`, and `gaps` is for a download that had to come from somewhere
other than a configured source (it is empty when everything came from where it
was configured to come from).

## Publishing

`publish.sh` writes this layout under `BOX_STORE_URL` (any server that accepts
PUT, GET and HEAD):

```
box-base/<version>/<arch>/box-base-<version>-<arch>.qcow2
box-base/<version>/<arch>/box-base-<version>-<arch>.qcow2.sha256
box-base/<version>/<arch>/box-base-<version>-<arch>.json         (build manifest)
box-base/index.json                                              (every version)
box-base/.lock                                                   (one publisher at a time)
```

The write token comes from the environment: `BOX_STORE_TOKEN`, or
`BOX_STORE_TOKEN_FILE` for a mounted secret. `BOX_STORE_TOKEN_HEADER` is the
header the store reads (default `Authorization`) and `BOX_STORE_TOKEN_SCHEME`
goes in front of the token (default `Bearer`; set it empty for a bare key). With
neither variable set the publish is unauthenticated, which is what a store that
needs no credential expects. `verify.sh` reads `BOX_STORE_URL` and the same token
variables, so a store that is private to read works too.

**This project publishes no images of its own.** There is no hosted index: an
index is a file someone uploads to their own store, and `BOX_IMAGE_INDEX` (or
`box --image-index`) points the CLI at it. `.github/workflows/image.yml` builds
and verifies an image on a release and attaches it to the release, and writes the
index of those release assets — that is the one place an index comes from here.

## No secret is ever in an image

An image goes to anyone who pulls it, so nothing a box receives as a credential
may be in it. In a running box the credentials live on the guest's tmpfs
(`/run/box/secrets`, written by the Nomad driver) and boxd keeps everything it
derives from them in `/run/boxd`; both are gone after a reboot. `no-secrets.sh`
fails an image whose executor directories, box version caches or account homes
(`/root/.prime`, `/root/.config/opencode`, `/root/.local/share/opencode`,
`/root/.claude`, the same below `/home/*`, `/etc/box/secrets`,
`/var/lib/boxd/executors`) hold anything, or that has an `auth.json`,
`credentials.json` or `.credentials.json` below an account or the box
directories. `build.sh` runs it in the build VM before the disk is captured and
writes nothing to `out/` if it fails; `guest-checks.sh` runs it in a booted
image, where the tmpfs locations must be tmpfs and empty.

## Publishing is one-way

A published version never changes. `publish.sh` refuses a version whose image,
`.sha256` or manifest already answers HEAD 200, and there is no override: an
upload usually cannot be taken back, and boxes hold the sha256 of what they
pulled, so a version must keep meaning one image. A mistake is fixed by building
and publishing the next version.

The run is ordered so that `index.json` never names an image that did not
arrive: take `box-base/.lock`, refuse a published version, upload the three
files, download them back (sha256 of the image, the two small files byte for
byte), then re-read the index, merge, write it, read it back and check that this
entry and every earlier entry survived. The lock is a store object with owner and
time, refreshed after every upload; a record older than 30 minutes is taken over,
so a crashed run cannot block the store. Two publishers of different versions
therefore serialise instead of overwriting each other's index.

`python3 images/publish_test.py` exercises all of that against a local server
that behaves like a store (PUT/GET/HEAD, 413 above a body limit, 401 without the
token, no DELETE), plus the token file, a custom header name and scheme, and a
store that needs no token. It needs `curl` and `sha256sum`; it prints one line
per check and exits non-zero on a failure.

## What the image contains

- apt (deb822, `/etc/apt/sources.list.d/box-apt.sources`) with the archive and
  the security archive, suites `noble` and `noble-security`, components
  main/restricted/universe/multiverse, and cloud-init told to keep those sources
  (`/etc/cloud/cloud.cfg.d/90-box-apt.cfg`). `noble-updates` and
  `noble-backports` are not configured: a mirror is not guaranteed to serve them
  and one missing suite fails the whole `apt-get update`.
- `docker.io` from Ubuntu (`docker-buildx`, `docker-compose-v2`), with
  `/etc/docker/daemon.json` written only when `BOX_MIRROR_DOCKER` names a
  registry mirror.
- npm (`/etc/npmrc`), pip (`/etc/pip.conf`), uv (`/etc/uv/uv.toml`) and Go
  (`GOPROXY`, `GOSUMDB` in `/etc/environment` and `/etc/profile.d/box-sources.sh`).
  `GOTOOLCHAIN=local` keeps `go` from downloading a toolchain of its own.
- git, curl, jq, ripgrep, build-essential, python3 + uv, Node 24 LTS, Go stable,
  prime-agent 0.10.2 and opencode 2.2.5 in `/usr/local/bin`.
- `/opt/box`, where boxd and boxctl are installed (`boxd-install.sh`).
- no SSH host keys, no authorized keys, no passwords, no shell history, no
  machine-id, no journal: cloud-init re-runs on the first boot of every box.

## The disk

The published image is 8 GiB virtual (`DISK`, default in `build.sh`). Lima grows
a disk but refuses to shrink one, so a box may ask for `disk_gib` 8 or more and
fails at create with `disk shrinking is not supported` below that. Building with
a larger `DISK` raises that floor for every box.

## Building another architecture

`images/build.sh arm64 <version>` on an x86_64 host runs the guest under TCG
(slow: minutes to boot, tens of minutes to provision). It also needs a Lima that
ships the aarch64 guest agent: the plain nix `lima` package only carries x86_64
and limactl fails with `guest agent binary could not be found for
Linux-aarch64`. Add nixpkgs' `lima-additional-guestagents`, copy its
`share/lima/lima-guestagent.Linux-aarch64.gz` into a copy of the lima tree and
pass that copy as `LIMACTL`. On an arm64 host no emulation and no extra agent are
involved.
