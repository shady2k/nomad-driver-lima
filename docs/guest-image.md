# The guest image and its platform bundle

A box is two artifacts: a **base image** (a qcow2: the OS and a toolchain) and a
**platform bundle** (a small tree the node mounts read-only at `/opt/box`: `boxd`,
the executors and the operator's add-ons). The image is built rarely and changes
slowly; the bundle is versioned on its own and can be newer than the image that
runs it. There are two ways to get an image:

1. **Build it from the recipe in `images/`.** It is the image this project tests
   against: Ubuntu 24.04 with a toolchain, a stub `boxd.service` that runs the
   bundle's daemon, every download pinned, and `images/verify.sh` to prove the
   result boots with a bundle and passes its checks. `images/README.md` is the
   one-screen version of this page.
2. **Bring your own image**, provided it meets the contract below. `boxd` and
   `guest/` are what it needs from this repository; how you build it is yours.

**This repository publishes no image and no bundle.** It keeps the recipes and
their build automation; the artifacts are never part of it. Versioned images and
bundles are published by the operator, separately, to their own registry or store
-- `images/publish.sh` writes both into any HTTP PUT store, and `BOX_IMAGE_INDEX`
(or `--image-index`) is how a CLI is pointed at an index of them. Site addresses
-- mirrors, the store, and how its token is obtained -- are deployment settings
the operator passes in; none of them is part of this repository or of an image it
builds.

## Building from the recipe

```sh
images/boxd-source.sh amd64 /tmp/boxd                 # which boxd, recorded
images/platform.sh --version 1.0.0 --arch amd64 \
  --boxd /tmp/boxd/boxd --out out                     # out/box-platform-1.0.0-linux-amd64.tar.gz
images/build.sh amd64 1.0.0                           # out/box-base-1.0.0-amd64.qcow2, .sha256, .json

PLATFORM_ARCHIVE=out/box-platform-1.0.0-linux-amd64.tar.gz \
IMAGE_PATH=out/box-base-1.0.0-amd64.qcow2 OUT=out \
  images/verify.sh amd64 1.0.0                        # boots the image with the bundle
images/publish.sh --platform out/box-platform-1.0.0-linux-amd64.tar.gz amd64 1.0.0
images/publish.sh amd64 1.0.0                         # both into BOX_STORE_URL
```

`images/platform.sh` builds the bundle on a Linux build host with no VM: it
downloads the executors as their own tarballs (a release asset and an npm
package) at the versions `images/pins.sh` pins, checks every digest, and writes a
deterministic archive. `images/build.sh` runs the recipe (`images/provision.sh`)
inside a Lima instance of the Ubuntu cloud image, writes the stub `boxd.service`
from the boxd of the same source, runs `guest/no-secrets.sh` before the disk is
captured, and writes the manifest that `images/verify.sh` and `images/publish.sh`
read. `images/verify.sh` boots the image the way the driver does -- with the
bundle mounted -- and runs `images/guest-checks.sh` inside it.

## The platform bundle

The image carries the OS and the toolchain. Everything that changes often enough
to be its own release -- `boxd`, the executors, the operator's add-ons -- is in a
versioned bundle that the node downloads and mounts into the guest read-only at
`/opt/box` before the boot, the way Lima delivers its own guest agent through
cidata instead of baking it into an image. A new `boxd` or a new executor is then
a new bundle version, not a new image: no image rebuild, no republished qcow2, and
images derived from a base image never have to be rebuilt for it.

### The archive

`box-platform-<version>-linux-<arch>.tar.gz` (`arch`: `amd64` | `arm64`), a
gzip'd tar that is deterministic -- entries sorted, owner and group 0, mtime 0,
gzip without a name or a timestamp -- with `built_at` as its only clock. Its root
is what the guest sees at `/opt/box`:

```
manifest.json        {"schema": 1, "version": "<version>", "arch": "<arch>",
                      "boxd": "<boxd version>",
                      "tools": {"<name>": "<version string>", ...},
                      "addons": {"files": N, "bytes": B, "sha256": "..."} | null,
                      "built_at": "<RFC3339 UTC>"}
bin/boxd             the boxd binary
bin/boxctl           symlink -> boxd
bin/<tool>           symlink into the tool's own directory
lib/no-secrets.sh    guest/no-secrets.sh, the check a seal runs in the guest
tools/<name>/...     each executor's full install directory (prime-agent needs
                     its package.json beside the binary; opencode is an npm tree)
addons/...           the operator's add-ons, absent when there are none
```

`lib/no-secrets.sh` is `guest/no-secrets.sh` (`images/platform.sh` copies it in).
The check belongs to the `boxd` that runs it: a seal runs the copy of the bundle
its box was started from, and refuses to finish when the bundle has none, so the
daemon and its proof travel in one archive. A build host without
`guest/no-secrets.sh` fails the bundle's build.

Every symlink in the archive is relative and stays inside it: the guest mounts
the tree read-only, and a link that escaped would point at something the operator
never chose. `images/platform.sh` refuses a bundle that breaks either rule.

The same inputs give the same bytes, `built_at` included: it comes from
`--built-at` or `SOURCE_DATE_EPOCH` when they are set and is the current time
otherwise. An `arm64` bundle builds on an `amd64` host -- nothing in the archive
is executed while it is built -- because the executors are fetched as their own
per-architecture tarballs instead of being installed.

### Which boxd a bundle carries

`images/platform.sh --boxd PATH` takes the binary; its architecture is checked
against `--arch` (a bundle of the wrong boxd is a guest that cannot start its
daemon), and its version comes from `--boxd-version`, from the `boxd.json`
`images/boxd-source.sh` writes beside it, or from the binary itself when it can
run on the build host. `images/boxd-source.sh` resolves a boxd from exactly one
recorded source:

| setting | source |
| --- | --- |
| `BOXD_RELEASE=vX.Y.Z` | `boxd_linux_<arch>` and `SHA256SUMS` from `https://github.com/<BOXD_REPO>/releases/download/<tag>/` (repo from `BOXD_REPO`, default `shady2k/nomad-driver-lima`); the sha256 is checked against the release's `SHA256SUMS`, and nothing is built |
| `BOXD_COMMIT=<sha or ref>` | `cmd/boxd` built from exactly that commit (the tree is taken with `git archive`, `CGO_ENABLED=0`, `-trimpath`, and the version stamping of `deploy/build.sh`) |
| neither | `cmd/boxd` built from this checkout's HEAD commit; the build refuses a tree with uncommitted changes under `cmd/` or `internal/` (`BOXD_ALLOW_DIRTY=1` overrides, and the record says it was dirty) |

### Which images take a bundle

An image built for bundles says so twice: `"platform": "mount"` in its
`/etc/box/image.json`, and the same field on its entry in `index.json` `"images"`
(`images/publish.sh` copies it from the build manifest). The CLI adds the
`platform` block to a box's task config only for such an image; for any other
image it adds none, so an older image keeps running its own `boxd` (and its own
`/opt/box` is not hidden by a mount). `box create` and `box upgrade` refuse
`--platform` for an image without the field, and say why.

### Extra files in every box: the bundle's add-ons

Sometimes the toolchain is not enough: the guest software needs extra files in
every box, such as a program's plugin or extension package. They go into the
bundle, at `/opt/box/addons`:

```sh
images/platform.sh --version 1.0.1 --arch amd64 --boxd /tmp/boxd/boxd \
  --addons ~/box-addons --out out
# ~/box-addons/foo/index.js  ->  /opt/box/addons/foo/index.js in the guest
```

`images/addons-pack.sh` packs that directory into one deterministic tar -- sorted
entries, owner 0, mtime 0, directories 0755, files 0644, and 0755 for a file that
had any execute bit -- and refuses anything that is not a regular file or a
directory (a symlink, socket, fifo or device), and an empty directory. The same
tree always gives the same archive, and the script prints
`{"files": N, "bytes": B, "sha256": "<digest of the sorted file listing>"}`.
`images/platform.sh` records that object in the bundle's manifest:

```json
"addons": {"files": 12, "bytes": 90210, "sha256": "3f1a...9c"}
```

and without `--addons` it says `"addons": null` and the bundle has no `/opt/box/addons`.

These are code and data, never credentials. The files are in the bundle, and
`guest/no-secrets.sh` covers `/opt/box` like the rest of the guest: before an
image's disk is captured, and again in a booted box. A credential belongs in the
box's executor configuration, which reaches the guest after boot on
`/run/box/secrets`.

The recipe, `boxd` and the driver never look inside these files, and no path in
them is named after a particular program. A program that loads plugins from a
local path is pointed at `/opt/box/addons/<name>` in its own settings, which the
operator supplies with the box's executor configuration; prime-agent's
`settings.json`, for one, takes an absolute path in its `packages` list. Keeping
the program's configuration out of the bundle is the point: the same bundle
serves a box with add-ons and one without.

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

It installs `/usr/local/bin/boxd` and `/usr/local/bin/boxctl`, writes the
embedded unit for that layout (`boxd -print-unit-installed`) to
`/etc/systemd/system/boxd.service`, creates `/var/lib/boxd` and `/run/boxd`, and
either starts the service or enables it for the next boot. `--dry-run` prints
what it would do; `DESTDIR` stages a tree. That is the layout for an image that
carries `boxd` itself and runs it with no bundle.

An image built for a platform bundle goes the other way: it carries no boxd, and
its `boxd.service` is `boxd -print-unit` -- the bundle layout, which runs
`/opt/box/bin/boxd` and requires `/opt/box` as a mount.

## What the driver gives the image

The driver writes a Lima 2.x configuration (`minimumLimaVersion: 2.0.0`) with
`vmType: qemu` on Linux and `vz` on macOS, `arch` of the node (`x86_64` or
`aarch64`), the image as a local file it downloaded and verified by sha256,
`mounts: []` and containerd off. It then reaches the guest only through
`limactl shell` and `limactl copy`.

For a box whose task config carried a `platform` block, the driver also downloads
the bundle by sha256 into its cache and adds one read-only mount: the bundle's
directory at `/opt/box`, with 9p and `cache: fscache` under qemu and virtiofs
under vz. The directory never changes under the guest -- a new bundle is a new
directory, named by its digest -- so the cache never goes stale. The mount exists
before the guest boots, like the task files mount.

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

Beyond that the recipe installs `qemu-guest-agent`, `openssh-server`, `earlyoom`,
docker with buildx and compose, git, curl, jq, ripgrep, build-essential and
python3. None of it is required; a box is more useful with a toolchain, but which
one is the operator's choice.

## What a bundle image must provide

- **an empty `/opt/box`**: the mount point. The bundle is mounted over it, so the
  image leaves it empty and root-owned.
- **`/etc/profile.d/box-platform.sh` putting `/opt/box/bin` on the `PATH`** for
  interactive shells, and `/etc/environment` doing the same for the sessions sshd
  starts (`limactl shell` is one of those, and it reads `/etc/environment`
  through PAM, not `/etc/profile.d`). The driver reaches the box's daemon as
  `/opt/box/bin/boxctl`.
- **`boxd.service` enabled, running `/opt/box/bin/boxd`** with the arguments in
  the release's unit (`--socket /run/boxd/boxd.sock --state-dir /var/lib/boxd
  --runtime-dir /run/boxd --secrets-dir /run/box/secrets --local-dir
  /etc/box/local`), `StateDirectory=boxd`, `RuntimeDirectory=boxd`, and
  `RequiresMountsFor=/opt/box /run/box/secrets /etc/box/local`. `boxd -print-unit`
  prints it; the service user must be root, and `/run` must be a tmpfs (the
  default on any systemd guest).
- **`"platform": "mount"` in `/etc/box/image.json`**, so a booted box and the
  publish step both know it wants a bundle.
- **`/etc/opencode/opencode.json` with `{"update": "disable"}`**: `/etc` is
  opencode's system configuration and outranks the per-user file, so a box cannot
  be talked into updating itself while the coordinator runs it. It stays in the
  image so the setting applies to whatever opencode the bundle carries.
- **`earlyoom` enabled, killing at low memory and never the processes that keep
  the box reachable**: a guest that runs out of memory stops answering `boxctl`
  (that is a box that looks hung), so the image kills the process that takes the
  memory while the guest can still run: `-m 10,5 -r 0 --avoid
  '(^|/)(boxd|sshd|systemd|lima-guestagent|qemu-ga)$'`. boxd follows the unit's
  journal and writes what earlyoom killed into the task log, so a guest that was
  short of memory says which process it lost.
- **no executor home or cache on the disk**: no `/root/.prime`,
  `/root/.config/opencode`, `/root/.local/share/opencode`, `/root/.claude`, no
  `/root/.cache/opencode`. An executor creates some of them the first time it is
  asked its version, which is why `guest/no-secrets.sh` checks an image for them
  and why `boxd` gives every executor its own `HOME` under `/var/lib/boxd`.

An image that carries its own `boxd` instead (no bundle) provides
`/usr/local/bin/boxd` and `/usr/local/bin/boxctl` through
`guest/boxd-install.sh`, and no `platform` field; `boxd` then finds its executors
on the image's `PATH`.

## Project images

A base image is the machine a project starts from. A **project image** is that
machine's environment, kept: an agent takes a box from a base image or from
another project image, installs what its project needs -- a toolchain, a package
set, the caches a build fills -- and the box is baked. A copy of its disk becomes
an immutable image with the dependencies and the caches and **without** the code,
the task data or a credential, and the box keeps the disk it was working on. A new
box is created from it and only brings its code in, so every box of that project
starts minutes ahead instead of installing the same things again.

An image starts as a **draft**. The bake writes it into the image cache of the
node that holds the box's disk, and no other node has it; the driver publishes
the draft's record as node attributes (`driver.box.baked.<version>.<field>`), so
`box images` lists the drafts of every node. A draft is created from on its own
node. **Publishing** it puts the file into the operator's store and names it in
that store's `index.json`, and from then on every node creates a box from it. A
version is immutable once published, as every image version is.

**Only an image with `"platform": "mount"` can be baked.** `boxd` and the
executors are not in an image: they come from the node's platform bundle at
`/opt/box`. So a bake of a box whose image carries its own `boxd` is refused, and
says so: that image is the older shape, and the image a bake makes is made for a
bundle.

### What a bake is

A bake works on a clone of the box, never on the box itself. The box is stopped
while it runs, and it starts again afterwards.

1. `boxctl info` in the box, before anything is stopped, says whether the seal
   would run: a turn running or a preparation in progress is refused there, with
   nothing stopped. That question cannot be asked of the clone, whose `boxd` is a
   fresh daemon that knows nothing of the box's turn.
2. The node must have room for the copy. A clone is a full copy of the disk file,
   written on the same Lima home, so less free space there than the box's disk
   occupies refuses the bake with both numbers.
3. The box is stopped, and a batch job `box-bake-<box id>` of the driver mounts the
   box's volume -- which also places it on the node that holds the disk. The mount
   is read-write, although the task only reads: Nomad places a read-only request of
   a dynamic host volume only when the volume offers a read-only access mode, and a
   box's volume is `single-node-writer`, so a read-only bake job is never placed at
   all. One driver task does the rest: `limactl clone <id> <id>-bake-<n>` copies
   the stopped box -- a full copy of the disk file, a new hostname and no snapshot
   (measured with Lima 2.2.0) -- the clone is started, the seal runs in it through
   `limactl shell`, the clone is stopped, and `qemu-img convert -c -O qcow2` writes
   its disk into a temporary file in the node's image cache as the runtime user,
   hashes the result and renames it to `<image_cache_dir>/<sha256>.img`. That is the
   name and place `box.EnsureImage` looks in, so a box on that node creates from the
   image without a download.
4. Beside it, `<image_cache_dir>/baked/<version>.<arch>.json` records `version`,
   `arch`, `sha256`, `size`, `virtual_bytes`, `parent`, `name`, `built_at`,
   `recipe` and `platform`. That record is what `box create --image <version>` on
   that node reads, and what a publish uploads.

The clone is the machine the seal empties, and it sees no credential: it mounts the
platform bundle at `/opt/box` and two empty directories at `/run/box/secrets` and
`/etc/box/local`, which is where `boxd.service` waits for a box's credentials and
task files. `limactl clone --mount-none` removes the source's mounts, and
`--mount`/`--mount-only` cannot name a guest path, so the driver writes those three
into the clone's `lima.yaml` itself and reads the configuration back: a mount that
is not one of them is refused, and so is a clone whose disk turns out to be a
symlink to the box's disk, which is copied for real before anything starts.

The clone is disposable: it is deleted in every case, also when a step fails or the
task is stopped, and a clone a killed driver left behind is removed by the next bake
of the same box -- until then the driver reports it as an orphan, because its disk
is not on a volume.

A stopped bake removes its temporary file, so a half image never stays in the
cache. A node without `qemu-img` fails the task with a sentence: nothing is
compressed without it. The record carries the image's virtual size, which is the
smallest disk a box made from it may have.

### What a seal removes

A seal refuses a guest that has the box's credentials mounted. `/run/box/secrets`
is the driver's read-only mount of the host's tmpfs, and when it holds any entry the
seal stops before it deletes anything: an image must never be made of a machine that
holds the credentials of the box it came from, and a guest that has them is a box
somebody works in. Its answer ends "a seal is only for a guest that is about to
become an image. Bake the box instead, which seals a clone with empty stand-ins for
the credentials" (`docs/boxctl-contract.md` has the whole message). `box bake` is
the way to an image, and its clone mounts an empty stand-in there (above), so a bake
never meets this refusal.

The seal closes every session and stops the executors, and the daemon forgets
both: a sealed `boxd` starts no session, and the next write of its state
directory cannot put back what the seal deleted. Then it removes, on the guest
disk:

- `/work`, and the sources and the state of the last preparation
  (`<state dir>/prepare.json` records the destination, and a `.partial` left
  beside it goes too);
- the session store, every session's directory, the transcripts and
  `<state dir>/state.json`;
- from the executor homes (`<state dir>/homes/<executor>`) and from every user
  home: every file with the same base name as a file in the box's secrets
  directories (`/run/box/secrets`, and boxd's copies under
  `/run/boxd/executors`); the directories an executor keeps its own sessions and
  transcripts in (`.prime`, `.claude`, `.config/opencode`, `.config/prime`,
  `.local/share/opencode`, `.local/share/prime`, `.cache/opencode`); the shell
  histories; `~/.ssh` keys and `authorized_keys`; `.git-credentials`; `.netrc`;
  the token lines of `.npmrc`; and the `credential.helper` settings of a git
  configuration -- a helper is a way to find the credential the seal just deleted;
- the contents of `/tmp` and `/var/tmp`, keeping the directories;
- the journal (`journalctl --rotate`, then `--vacuum-time=1s`) and the contents of
  `/var/log`;
- the SSH host keys, `/etc/ssh/ssh_host_*`: an image that carried them would let
  anyone impersonate every box made from it;
- cloud-init's state and the machine-id, exactly as the end of
  `images/provision.sh` does it, so the next boot is the first boot of a new
  machine;
- what ties the node's mounts to the instance the box was built in: the lines of
  `/etc/fstab` that mount them, and a systemd mount unit that names one of them.
  The node writes the mounts of the new instance on the next boot, with its own
  9p tag; a baked image that carried the old lines would come up with mounts that
  cannot work, and `boxd` -- whose unit requires those mounts -- would be refused
  before it ever ran.

`/run` and the mounts are not on the disk, so they are left where they are.

That deletion by file name is narrow in a bake. The clone mounts an empty stand-in
at `/run/box/secrets`, so it has no credential file whose base name could be matched
against a file in a home, and a copy of a credential that sits there is not deleted
by name. The proof is what catches it: `guest/no-secrets.sh` fails on an
`auth.json`, `credentials.json` or `.credentials.json` below `/root`, `/home`,
`/etc/box`, `/var/lib/boxd` or `/opt/box`, and the seal refuses the image. Measured:
a planted `/home/user/auth.json` failed a bake with
`FAIL credential files are in the image: /home/user/auth.json`.

### What a seal keeps

The homes stay, and so does everything installed in them: toolchains, package
caches, language environments. That is most of what an image is for, and it is
what makes a box from a project image fast. The installed packages, the image's
own configuration and its `boxd.service` stay as they are, because the image still
has to boot.

`/work` does not stay: it is where the code was, and code belongs to a box, never
to an image.

### What a seal proves, and what it writes

One proof runs after the deletions, and it can stop the seal: **the bundle's
`lib/no-secrets.sh`** -- the same `guest/no-secrets.sh` the image build runs, and
the one an operator runs in a box. A bundle that carries no copy is a seal that
does not finish, and the error names the file it looked for.

The seal used to search the guest disk for the credential values themselves. That
search is gone on purpose: it read every file of a disk that can be tens of
gigabytes, and what it proved was narrow -- the values of this box's secrets
directory, as they were at that moment. `guest/no-secrets.sh` is the proof the seal
keeps, and it is the same check an operator runs on an image before publishing it.

The consequence, said plainly: a file that merely mentions a credential does not
stop a seal. A log line, a shell history, a build artifact or a database row can
carry a value into an image, and the file names and directories the script looks
for are what this proof covers. A credential that does end up in an image is
rotated and a new version is published, because a published version never changes.

Then the seal releases the free blocks: `fstrim -a`, or a file of zeros written
until the filesystem is full where `fstrim` is not available, and `sync`. A qcow2
built from a disk whose freed blocks were never released carries the data the seal
deleted, in clusters the image still holds.

The seal writes two files, and reads the first back before it says it is done:

- **`/etc/box/image.json`**: `schema`, `version`, `name`, `arch`, `parent`,
  `platform` (`"mount"`), `recipe` (base64) and `sealed_at`. `images/build.sh`
  writes the same file for a built image, so a booted box answers "which image am
  I" without the manifest at hand.
- **`/etc/box/recipe.sh`**: the recipe itself, mode 0755.

A failed seal stops at the step that failed, and its message says what that means:
"What it deleted is gone: a seal empties the machine it runs in, which for a bake is
a clone of the box and never the box itself. Fix what is left and bake again". A
failed proof says the same in its own words: "A seal empties the machine it runs in,
which for a bake is a clone of the box and never the box itself: remove what it
names in the box and bake again". Either way the box the owner works in was never
sealed, it starts again with its work, and nothing the seal deleted returns.

### The recipe

The recipe is the script that makes this environment from the parent image: for
the machine, what `.box/prepare.sh` is for the sources. It comes from `box bake
--recipe FILE`, and a box created from the image carries it with the record.

It travels in base64 (at most 64 KiB) in the image's record, in the task config of
a box created from the image, in the baked record and in the index entry, and as a
script at `/etc/box/recipe.sh` in the guest. It is what `box bake --rebuild NAME`
replays: a temporary box from a newer base image, the recipe run in it, and the
result baked as the next version of the same name. So a recipe has to start from
the parent image and from nothing else.

An image baked with `--no-recipe` carries none. It can be created from, and it is
never rebuilt: a rebuild of an image without a recipe is refused.

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

## No secret is ever in an image or a bundle

An image goes to anyone who can pull it, and so does a bundle. Credentials reach
a box only at run time, on `/run/box/secrets`, and `boxd` keeps what it derives in
`/run/boxd`. Before you capture an image, and again inside a booted box, run the
check that proves it:

```sh
sudo guest/no-secrets.sh 1.0.0      # one line per location, non-zero on a hit
```

It fails on a non-empty executor directory, on an `auth.json`, `credentials.json`
or `.credentials.json` below an account or the box directories (the mounted
`/opt/box` included), and on a `/run/box/secrets` or `/run/boxd` that is not a
tmpfs. `images/build.sh` runs it in the build VM before the disk is captured, and
`images/verify.sh` runs it in a booted image.

## What `images/verify.sh` checks

`images/verify.sh <arch> <version>` takes a local build (`IMAGE_PATH`) or a
published one (`BOX_STORE_URL`, with `BOX_STORE_TOKEN` or
`BOX_STORE_TOKEN_FILE` when the store needs a token), verifies the image's
sha256, and boots it with a platform bundle -- the archive in `PLATFORM_ARCHIVE`,
or one built here with `images/platform.sh` -- mounted read-only at `/opt/box`
with 9p and `cache: fscache`, the way the driver mounts one. It prints one line
per check: PASS or FAIL, expected against actual, and a non-zero exit when
anything failed.

| check | expected |
| --- | --- |
| the image's sha256 | the digest in its `.sha256` file, published or local |
| the bundle's sha256 | the digest in its `.sha256` file, when it came with one |
| the image boots under Lima, and the instance is running | a running instance |
| cloud-init finished on this boot | `status: done` |
| `boxd.service` is enabled and active, the unit runs `/opt/box/bin/boxd` | enabled, active, `ExecStart=/opt/box/bin/boxd` |
| the image carries no boxd of its own | `/usr/local/bin/boxd` absent |
| `/opt/box` is a mount, not a directory on the box disk, and read-only | `/opt/box`, `ro` in its mount options |
| the bundle's manifest | readable, `schema: 1`, and the boxd its unit runs |
| the bundle's boxd | `boxd --version` the version its manifest names |
| every tool the bundle names is in its `bin` | an executable for each |
| `boxctl info` reports the bundle | the mounted version and boxd |
| the box can run an executor out of the bundle | at least one available |
| the image's tool versions | every entry of the manifest's `tools`, each checked with the command that recorded it |
| the add-ons at `/opt/box/addons` (when the bundle was built with them) | nothing group- or world-writable, and the file count the manifest records; without the directory, "no add-ons" |
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
stops. `images/README.md` lists the variables; `provision.sh` and `images/pins.sh`
are where the defaults are written down.

| variable | what it moves |
| --- | --- |
| `BOX_MIRROR_APT`, `BOX_MIRROR_APT_SECURITY` | the Ubuntu archive and its security suite |
| `BOX_MIRROR_NPM`, `BOX_MIRROR_PYPI` | the npm registry (the bundle's opencode comes from it too), the pip and uv index |
| `BOX_MIRROR_GO`, `BOX_MIRROR_GO_DIST` | the Go module proxy, the Go release tarballs |
| `BOX_MIRROR_GITHUB`, `BOX_MIRROR_NODE` | GitHub release assets (the bundle's prime-agent comes from there too), the Node.js release files |
| `BOX_MIRROR_DOCKER` | a Docker Hub registry mirror in `/etc/docker/daemon.json` |
| `BOX_MIRROR_CLOUDIMAGE` | the base cloud image (`images/build.sh` only) |
| `BOXD_REPO`, `BOXD_RELEASE_BASE_URL`, `BOXD_ALLOW_DIRTY` | which repository a `BOXD_RELEASE` asset comes from (`shady2k/nomad-driver-lima` by default), where its release assets are served (`https://github.com`), and whether a dirty tree may be built (no, unless it is set to `1` and then recorded) |
| `BOX_STORE_URL`, `BOX_STORE_TOKEN`, `BOX_STORE_TOKEN_FILE`, `BOX_STORE_TOKEN_HEADER`, `BOX_STORE_TOKEN_SCHEME` | the operator's store, and the token `images/publish.sh` and `images/verify.sh` send it |

One difference reaches the box: `GOSUMDB`. The public Go proxy serves the
checksum database, so a box verifies module sums against `sum.golang.org`; a
mirror usually does not serve it, so `GOSUMDB=off` exactly when `BOX_MIRROR_GO`
is set.

A token is never an argument and never on stdout: it goes into a 0600 header file
that `curl` reads. Whether it comes from a vault, a secret manager or a mounted
file is the operator's business. `BOX_STORE_TOKEN_HEADER` and
`BOX_STORE_TOKEN_SCHEME` say where a store reads the token and what comes before
it, and the publish task of the driver has the same two settings in its
`nomad/jobs/box-store` variable (`box store set --token-header/--token-scheme`).

## Publishing: the index

`box images` and `box create --image` read one JSON document over HTTP(S), named
by `BOX_IMAGE_INDEX` or `--image-index`. There is no built-in default. The CLI
fetches it with a 30 second timeout and reads at most 8 MiB; a 404 means "no
images are published yet" and is reported as such.

`images/publish.sh` writes that document for its store, and the `box images
publish` task of the driver writes it too, in the same order and under the same
lock (`box-base/.lock`, a record of who holds the store and when, taken over when
it is 30 minutes old): a version that already exists is refused (a published
version never changes), the artifact, its `.sha256` and its manifest are
uploaded, all three are read back, and only then is the version merged into
`index.json`. A store or a proxy that answers 413 to a body of a declared size
gets the same bytes in chunked form. Images go under `"images"`, platform
bundles under `"platforms"`, and publishing one leaves the other alone.
`images/publish_test.py` exercises all of that against a local fake store, and
the driver's side against a fake store of its own.

The canonical shape is `schema`, `updated_at`, `images[<version>][<arch>]` and
`platforms[<version>][<arch>]`. Each image entry holds `url`, `sha256` (`size`
and `built_at` are shown by `box images`), and `"platform": "mount"` when the
image expects a bundle. Each platform entry holds `url`, `sha256`, `size`,
`built_at`, `boxd` and `tools`:

```json
{
  "schema": 1,
  "updated_at": "2026-01-01T00:00:00Z",
  "images": {
    "1.0.0": {
      "amd64": {
        "url": "https://images.example.com/box-base/1.0.0/amd64/box-base-1.0.0-amd64.qcow2",
        "sha256": "3f1a...64 hex characters...9c",
        "size": 943718400,
        "built_at": "2026-01-01T00:00:00Z",
        "platform": "mount"
      }
    }
  },
  "platforms": {
    "1.0.0": {
      "amd64": {
        "url": "https://images.example.com/box-platform/1.0.0/amd64/box-platform-1.0.0-linux-amd64.tar.gz",
        "sha256": "aa01...64 hex characters...7d",
        "size": 150000000,
        "built_at": "2026-01-01T00:00:00Z",
        "boxd": "1.0.0",
        "tools": {"prime-agent": "0.10.2", "opencode": "2.2.5"}
      }
    }
  }
}
```

An index without `"platforms"` is valid: no bundle is published yet, and every
image runs its own boxd. What the reader accepts, besides that:

- `"images"` may also be a list of objects each with a `version` key, a version
  may nest its artifacts under `"arches"`, and `"versions"` works in place of
  `"images"`. Architecture keys are normalised: `x86_64`, `x64` and `amd64` are
  one architecture, and so are `aarch64` and `arm64`.
- `--image latest` (and an empty version) means the newest version that has the
  architecture; versions compare numerically, so `1.10.0` is newer than `1.9.0`.
- `sha256` must be 64 hex characters and may carry a `sha256:` prefix. The
  driver downloads the `url`, verifies the digest and caches it by digest; a URL
  that does not answer the pinned bytes is refused, and so is a bundle URL that
  does not answer its digest.

Publish a new version rather than changing a published one: a box is created from
a digest, so a version that means two images (or two bundles) is a bug waiting to
happen.

`docs/cli.md` has the commands (`box images`, `box create --image`,
`--platform`), and `deploy/README.md` shows where `BOX_IMAGE_INDEX` is set for a
coordinator.
