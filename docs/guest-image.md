# The guest image

A box boots a qcow2 image. There are two ways to get one:

1. **Build it from the recipe in `images/`.** It is the image this project tests
   against: Ubuntu 24.04 with a toolchain, `boxd` installed and its unit enabled,
   every download pinned, and `images/verify.sh` to prove the result boots and
   passes its checks. `images/README.md` is the one-screen version of this page.
2. **Bring your own image**, provided it meets the contract below. `boxd` and
   `guest/` are what it needs from this repository; how you build it is yours.

**This repository publishes no image.** It keeps the recipe and its build
automation; the binary image is never part of it. Versioned images are published
by the operator, separately, to their own registry or store -- `images/publish.sh`
writes one to any HTTP PUT store, and `BOX_IMAGE_INDEX` (or `--image-index`) is
how a CLI is pointed at an index of them. Site addresses -- mirrors, the store,
and how its token is obtained -- are deployment settings the operator passes in;
none of them is part of this repository or of an image it builds.

## Building from the recipe

```sh
images/build.sh amd64 1.0.0        # out/box-base-1.0.0-amd64.qcow2, .sha256, .json
IMAGE_PATH=out/box-base-1.0.0-amd64.qcow2 OUT=out images/verify.sh amd64 1.0.0
images/publish.sh amd64 1.0.0      # to BOX_STORE_URL, your store
```

`images/build.sh` runs the recipe (`images/provision.sh`) inside a Lima instance
of the Ubuntu cloud image, installs `boxd` through `guest/boxd-install.sh`, runs
`guest/no-secrets.sh` before the disk is captured, and writes the manifest that
`images/verify.sh` and `images/publish.sh` read. `images/verify.sh` boots the
image the way the driver does and runs `images/guest-checks.sh` inside it.

### Which boxd the image carries

`boxd` comes from exactly one recorded source, and the build refuses an ambiguous
one:

| setting | source |
| --- | --- |
| `BOXD_RELEASE=vX.Y.Z` | `boxd_linux_<arch>` and `SHA256SUMS` from `https://github.com/<BOXD_REPO>/releases/download/<tag>/` (repo from `BOXD_REPO`, default `shady2k/nomad-driver-lima`); the sha256 is checked against the release's `SHA256SUMS`, and nothing is built |
| `BOXD_COMMIT=<sha or ref>` | `cmd/boxd` built from exactly that commit (the tree is taken with `git archive`, `CGO_ENABLED=0`, `-trimpath`, and the version stamping of `deploy/build.sh`) |
| neither | `cmd/boxd` built from this checkout's HEAD commit; the build refuses a tree with uncommitted changes under `cmd/` or `internal/` (`BOXD_ALLOW_DIRTY=1` overrides, and the image is recorded as dirty) |

The chosen source is recorded twice: in the build manifest (`"boxd"`: kind, tag or
ref, resolved commit, binary sha256, version, dirty) and in the image itself at
`/etc/box/image.json`, next to the image version, the base image and the tool
versions. A booted box can therefore answer "which boxd is in me" on its own, and
`images/verify.sh` checks the installed binary against that record.

### Extra files the image carries: `IMAGE_ADDONS`

Sometimes the toolchain is not enough: the guest software needs extra files in
every box, such as a program's plugin or extension package. A directory on the
build host installs them at one fixed path:

```sh
IMAGE_ADDONS=~/box-addons images/build.sh amd64 1.0.0
# ~/box-addons/foo/index.js  ->  /opt/box/addons/foo/index.js in the image
```

`images/addons-pack.sh` packs that directory into one deterministic tar -- sorted
entries, owner 0, mtime 0, directories 0755, files 0644, and 0755 for a file that
had any execute bit -- and refuses anything that is not a regular file or a
directory (a symlink, socket, fifo or device), and an empty directory. The same
tree always gives the same archive, and the script prints
`{"files": N, "bytes": B, "sha256": "<digest of the sorted file listing>"}`.

These are code and data, never credentials. The files are in every copy of the
image, and `guest/no-secrets.sh` covers `/opt/box/addons` like the rest of the
image: before the disk is captured, and again in a booted box. A credential
belongs in the box's executor configuration, which reaches the guest after boot
on `/run/box/secrets`.

The build records what it installed, in the manifest and in `/etc/box/image.json`:

```json
"addons": {"path": "/opt/box/addons", "files": 12, "bytes": 90210, "sha256": "3f1a...9c"}
```

Without `IMAGE_ADDONS` both say `"addons": null` and the image has no
`/opt/box/addons`. `images/guest-checks.sh` checks a booted box: everything below
`/opt/box/addons` owned by root, nothing group- or world-writable, and the file
count the image records -- or, when there is no such directory, the line "no
add-ons", which is not a failure.

The recipe, boxd and the driver never look inside these files, and no path in
them is named after a particular program. A program that loads plugins from a
local path is pointed at `/opt/box/addons/<name>` in its own settings, which the
operator supplies with the box's executor configuration; prime-agent's
`settings.json`, for one, takes an absolute path in its `packages` list. Keeping
the program's configuration out of the image is the point: the same image serves
a box with add-ons and one without.

## Bringing your own image

The release carries `boxd_linux_amd64`, `boxd_linux_arm64` and
`guest/boxd-install.sh` (`deploy/build.sh`, and `SHA256SUMS` covers all three).
Both binaries are built with `CGO_ENABLED=0`, so they are static and need no C
library in the image.

```sh
# into an image being built (a chroot or a mounted image, no systemd running)
sudo guest/boxd-install.sh --from dist/1.0.0/boxd_linux_amd64 --no-start
# or into a guest that is already running: installs, enables and starts it
sudo guest/boxd-install.sh --from ./boxd_linux_amd64
```

It installs `/usr/local/bin/boxd`, links `/usr/local/bin/boxctl` to it, writes the
embedded systemd unit to `/etc/systemd/system/boxd.service`, creates
`/var/lib/boxd` and `/run/boxd`, and either starts the service or enables it for
the next boot. `--dry-run` prints what it would do; `DESTDIR` stages a tree.

## What the driver gives the image

The driver writes a Lima 2.x configuration (`minimumLimaVersion: 2.0.0`) with
`vmType: qemu` on Linux and `vz` on macOS, `arch` of the node (`x86_64` or
`aarch64`), the image as a local file it downloaded and verified by sha256, no
host mounts (`mounts: []`) and containerd off. It then reaches the guest only
through `limactl shell` and `limactl copy`.

So the image has to be:

- **bootable by Lima 2.x** for the node's architecture, as qcow2 (or raw), with a
  kernel and initramfs on the disk — no host kernel, no host rootfs.
- **cloud-init enabled, and a first boot for every box.** Lima creates its user
  with cloud-init at boot and gives it passwordless sudo. The image must therefore
  not carry an identity of its own: no machine-id, no ssh host keys, no
  authorized keys, no passwords, no `/var/lib/cloud/instances`. The recipe runs
  `cloud-init clean --logs --machine-id` and empties `/etc/machine-id` before it
  captures the disk.
- **reachable over ssh, with systemd as init, and with `bash` and coreutils**:
  `limactl shell` is an ssh session as Lima's user, that user needs `sudo -n`
  (the driver installs task files as root and reads `journalctl -u boxd`), and
  every guest command is a `bash -euo pipefail -c` script.
- **as large as the boxes will be, at least.** Lima grows a disk and refuses to
  shrink one, so the image's virtual size is the floor for `disk_gib`; a box
  asking for less fails at create. Growing the partition into a bigger disk is the
  image's job (below).

Beyond that the recipe installs `qemu-guest-agent`, `openssh-server`, docker with
buildx and compose, git, curl, jq, ripgrep, build-essential and python3. None of
it is required; a box is more useful with a toolchain, but which one is the
operator's choice.

## What boxd needs in the image

- **`boxd` installed by `guest/boxd-install.sh`**, `boxd.service` **enabled** in
  `multi-user.target`. The unit runs
  `boxd --socket /run/boxd/boxd.sock --state-dir /var/lib/boxd --runtime-dir
  /run/boxd --secrets-dir /run/box/secrets --local-dir /etc/box/local` with
  `StateDirectory=boxd` and `RuntimeDirectory=boxd`, and its stdout (one JSON
  event per line) is the feed the driver streams into the Nomad task log.
  `RuntimeDirectory` puts `/run/boxd` on `/run`, so **`/run` must be a tmpfs** —
  the default on any systemd guest. Install the `boxd` of the same release as the
  driver and the CLI: both reach it through `boxctl`, and one release is one
  contract.
- **`/run/box/secrets`** is written by the driver after boot (root, mode 0700,
  one directory per executor) and never exists in the image. `boxd` reads it and
  derives every executor home and configuration below `/run/boxd`. Neither path
  may be a directory on the disk: a credential there would survive the box.
- **the executors on `PATH`** — `boxd` probes each one and reports what it finds
  in `boxctl info`:

  | executor | binary | version the recipe installs |
  | --- | --- | --- |
  | `prime` | `prime-agent` (`--mode acp`) | 0.10.2 |
  | `opencode` | `opencode` (`acp`) | 2.2.5 |
  | `claude` | `claude-code-acp` (npm `@zed-industries/claude-code-acp`) | not installed |

  A missing binary is not an error: that executor reports `available: false` with
  the sentence that says what to install. A box with no executor still starts.
- **`/etc/opencode/opencode.json` with `{"update": "disable"}`** if opencode is
  installed. `/etc` is opencode's system configuration and outranks the per-user
  file, so a box cannot be talked into updating itself while the coordinator runs
  it. `boxd` puts the box's own `opencode.json` (the executors variable) in the
  executor's home; the image's file is what applies to everyone.
- **no executor home or cache on the disk**: no `/root/.prime`,
  `/root/.config/opencode`, `/root/.local/share/opencode`, `/root/.claude`, no
  `/root/.cache/opencode`. Running `<executor> --version` as root creates some of
  them, which is why `guest/no-secrets.sh` checks an image for them.

## Model catalog

The models a session may name are the ones the provider offers to the box's
key. When a session starts, boxd rewrites the model list in its own copy of
each executor's configuration on the tmpfs (`/run/boxd/executors/prime/models.json`,
opencode's `opencode.json`), from the first of these that answers:

1. **An addon's catalog.** An addon under `/opt/box/addons/<name>/` may declare
   one in its `package.json`:

   ```json
   "box": {"catalog": {"providers": ["myprovider"], "command": ["node", "box-catalog.mjs"]}}
   ```

   boxd runs the command in the addon's directory, with
   `{"provider", "baseUrl", "apiKey", "box"}` as one JSON object on stdin (the
   key is never in argv or the environment; `box` is the guest's hostname), and
   reads `{"models": [...], "headers": {...}}` from stdout. `models` are
   prime's model entries, each with an `id` and what the addon knows of it,
   `contextWindow`, `maxTokens`, `reasoning`, `compat`. That answer is the list,
   sizes included; opencode gets the sizes as `limit.context` and
   `limit.output`. `headers`, optional, go with every request to the provider
   (prime's `headers`, opencode's `options.headers`): what a Pi extension would
   add per request, since prime-agent 0.10.2 runs no extension code. The
   journal names the headers, never their values. The command has 20 seconds.
2. **The provider's own list**, `GET <baseUrl>/models` with the key. An
   OpenAI-compatible provider names the models but not their size, so a model
   keeps the size the staged definition gives it, and one the definition does
   not size is named in the journal: the executor assumes its own default.
3. **The staged list**, unchanged, when neither answers.

The journal says which it was and which models there are. `boxctl session new`
refuses a model outside the list and names the ones there are.

## Disk growth

A box can be resized larger and the driver only stops, edits and starts the VM:
the guest has to grow its own root partition and filesystem on boot. cloud-init's
`growpart`/`resizefs` is not enough on the recipe's image (its EFI and boot
partitions are numbered 14–16, so root is not the highest partition and `growpart`
refuses it), so the recipe installs a `box-grow-rootfs.service` that runs
after the root is mounted read-write: `parted -s -f <disk> resizepart <n> 100%`,
`partx -u <disk>`, `resize2fs <root>`. An image without that keeps its first
size, whatever `disk_gib` asks for.

## No secret is ever in an image

An image goes to anyone who can pull it. Credentials reach a box only at run time,
on `/run/box/secrets`, and `boxd` keeps what it derives in `/run/boxd`. Before you
capture an image, and again inside a booted box, run the check that proves it:

```sh
sudo guest/no-secrets.sh 1.0.0      # one line per location, non-zero on a hit
```

It fails on a non-empty executor directory, on an `auth.json`, `credentials.json`
or `.credentials.json` below an account or the box directories, and on a
`/run/box/secrets` or `/run/boxd` that is not a tmpfs. `images/build.sh` runs it
in the build VM before the disk is captured, and `images/verify.sh` runs it in a
booted image.

## What `images/verify.sh` checks

`images/verify.sh <arch> <version>` takes a local build (`IMAGE_PATH`) or a
published one (`BOX_STORE_URL`, with `BOX_STORE_TOKEN` or
`BOX_STORE_TOKEN_FILE` when the store needs a token), verifies the image's
sha256, boots it with `images/box-boot.yaml`, and prints one line per check: PASS
or FAIL, expected against actual, and a non-zero exit when anything failed.

| check | expected |
| --- | --- |
| the image's sha256 | the digest in its `.sha256` file, published or local |
| the image boots under Lima, and the instance is running | a running instance |
| cloud-init finished on this boot | `status: done` |
| `boxd.service` is enabled and active, `boxctl info` answers | enabled, active, a JSON document |
| the executors the image installs are available | `prime` and `opencode` |
| the boxd binary is the one the image records | the manifest's `boxd.sha256`, and its `boxd.version` from `boxd --version` |
| the tool versions are the ones the image records | every entry of the manifest's `tools`, each checked with the command that recorded it |
| the add-ons at `/opt/box/addons` (when the image was built with `IMAGE_ADDONS`) | owned by root, nothing group- or world-writable, and the file count the manifest records; without the directory, "no add-ons" |
| apt talks only to the sources the image configures | no fetch from any other host |
| `/run/box/secrets` and `/run/boxd` are tmpfs | `tmpfs` for both |
| a credential written into `/run/box/secrets` the way the driver delivers it | readable by `boxd`, root and 0600, nowhere on the disk (`find / -xdev`) |
| the same credential, as the box's default user | permission denied |
| a rotated credential, then `boxctl reload` | the executor listed as configured and the new value in `/run/boxd/executors/<executor>/` |
| `guest/no-secrets.sh` in the booted image | no credential in the image |
| the root filesystem after boot | it fills its partition (the image grows it) |
| the box's identity | ssh host keys and the machine-id of this boot, not of the image |

## Deployment settings

Every source has a public upstream default, and a `BOX_MIRROR_*` variable moves
it for one build (a closed network, a caching proxy). The versions and their
sha256 pins stay in force either way: a mirror serves the same bytes or the build
stops. `images/README.md` lists the variables; `provision.sh` is where the
defaults are written down.

| variable | what it moves |
| --- | --- |
| `BOX_MIRROR_APT`, `BOX_MIRROR_APT_SECURITY` | the Ubuntu archive and its security suite |
| `BOX_MIRROR_NPM`, `BOX_MIRROR_PYPI` | the npm registry, the pip and uv index |
| `BOX_MIRROR_GO`, `BOX_MIRROR_GO_DIST` | the Go module proxy, the Go release tarballs |
| `BOX_MIRROR_GITHUB`, `BOX_MIRROR_NODE` | GitHub release assets, the Node.js release files |
| `BOX_MIRROR_DOCKER` | a Docker Hub registry mirror in `/etc/docker/daemon.json` |
| `BOX_MIRROR_CLOUDIMAGE` | the base cloud image (`images/build.sh` only) |
| `IMAGE_ADDONS` | extra files installed at `/opt/box/addons` in the image (`images/build.sh` only; see above) |
| `BOXD_REPO`, `BOXD_RELEASE_BASE_URL`, `BOXD_ALLOW_DIRTY` | which repository a `BOXD_RELEASE` asset comes from (`shady2k/nomad-driver-lima` by default), where its release assets are served (`https://github.com`), and whether a dirty tree may be built into an image (no, unless it is set to `1` and then recorded) |
| `BOX_STORE_URL`, `BOX_STORE_TOKEN`, `BOX_STORE_TOKEN_FILE`, `BOX_STORE_TOKEN_HEADER`, `BOX_STORE_TOKEN_SCHEME` | the operator's store, and the token `images/publish.sh` and `images/verify.sh` send it |

One difference reaches the box: `GOSUMDB`. The public Go proxy serves the
checksum database, so a box verifies module sums against `sum.golang.org`; a
mirror usually does not serve it, so `GOSUMDB=off` exactly when `BOX_MIRROR_GO`
is set.

A token is never an argument and never on stdout: it goes into a 0600 header file
that `curl` reads. Whether it comes from a vault, a secret manager or a mounted
file is the operator's business.

## Publishing: the image index

`box images` and `box create --image` read one JSON document over HTTP(S), named
by `BOX_IMAGE_INDEX` or `--image-index`. There is no built-in default. The CLI
fetches it with a 30 second timeout and reads at most 8 MiB; a 404 means "no
images are published yet" and is reported as such.

`images/publish.sh` writes that document for its store, and it is the only thing
here that writes one: it refuses a version that already exists (a published
version never changes), uploads the image, its `.sha256` and its manifest, reads
them back, and only then merges the version into `index.json` under a lock.
`images/publish_test.py` exercises all of that against a local fake store.

The canonical shape is `schema`, `updated_at` and `images[<version>][<arch>]`,
each artifact holding `url`, `sha256` (`size` and `built_at` are shown by
`box images`); this is the smallest useful index:

```json
{
  "schema": 1,
  "updated_at": "2026-01-01T00:00:00Z",
  "images": {
    "1.0.0": {
      "amd64": {
        "url": "https://images.example.com/box/1.0.0/box-1.0.0-amd64.qcow2",
        "sha256": "3f1a...64 hex characters...9c",
        "size": 943718400,
        "built_at": "2026-01-01T00:00:00Z"
      },
      "arm64": {
        "url": "https://images.example.com/box/1.0.0/box-1.0.0-arm64.qcow2",
        "sha256": "aa01...64 hex characters...7d",
        "size": 901775360
      }
    }
  }
}
```

What the reader accepts, besides that:

- `"images"` may also be a list of objects each with a `version` key, a version
  may nest its artifacts under `"arches"`, and `"versions"` works in place of
  `"images"`. Architecture keys are normalised: `x86_64`, `x64` and `amd64` are
  one architecture, and so are `aarch64` and `arm64`.
- `--image latest` (and an empty version) means the newest version that has the
  architecture; versions compare numerically, so `1.10.0` is newer than `1.9.0`.
- `sha256` must be 64 hex characters and may carry a `sha256:` prefix. The
  driver downloads the `url`, verifies the digest and caches it by digest; a URL
  that does not answer the pinned bytes is refused.

Publish a new version rather than changing a published one: a box is created from
a digest, so a version that means two images is a bug waiting to happen.

`docs/cli.md` has the commands (`box images`, `box create --image`), and
`deploy/README.md` shows where `BOX_IMAGE_INDEX` is set for a coordinator.
