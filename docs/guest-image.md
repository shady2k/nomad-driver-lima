# The guest image

A box boots a qcow2 image that **you** build and publish. This repository builds
no image and publishes none: it ships `boxd` (the guest daemon) and `boxctl` (the
same binary), the installer that puts them into an image, and this page, which is
the contract between them. Any image that boots under Lima, runs `boxd` and meets
the requirements below works; how it was built is yours to choose.

The release carries `boxd_linux_amd64`, `boxd_linux_arm64` and `boxd-install.sh`
(`deploy/build.sh`, `SHA256SUMS` covers all three). Both binaries are built with
`CGO_ENABLED=0`, so they are static and need no C library in the image.
`guest/boxd-install.sh` is the tool that puts boxd into an image of your own:

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
  authorized keys, no passwords, no `/var/lib/cloud/instances`. `cloud-init clean
  --logs --machine-id` plus `truncate -s 0 /etc/machine-id` is what the reference
  recipe did before capture.
- **reachable over ssh, with systemd as init, and with `bash` and coreutils**:
  `limactl shell` is an ssh session as Lima's user, that user needs `sudo -n`
  (the driver installs task files as root and reads `journalctl -u boxd`), and
  every guest command is a `bash -euo pipefail -c` script.
- **as large as the boxes will be, at least.** Lima grows a disk and refuses to
  shrink one, so the image's virtual size is the floor for `disk_gib`; a box
  asking for less fails at create. Growing the partition into a bigger disk is the
  image's job (below).

Beyond that the reference recipe installed `qemu-guest-agent`, `openssh-server`,
docker with buildx and compose, git, curl, jq, ripgrep, build-essential and
python3. None of it is required; a box is more useful with a toolchain, but which
one is the operator's choice.

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

  | executor | binary | version the reference image was tested with |
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

## Disk growth

A box can be resized larger and the driver only stops, edits and starts the VM:
the guest has to grow its own root partition and filesystem on boot. cloud-init's
`growpart`/`resizefs` is not enough on the reference image (its EFI and boot
partitions are numbered 14–16, so root is not the highest partition and `growpart`
refuses it), so the reference image carried a `box-grow-rootfs.service` that runs
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
`/run/box/secrets` or `/run/boxd` that is not a tmpfs.

## Publishing: the image index

`box images` and `box create --image` read one JSON document over HTTP(S), named
by `BOX_IMAGE_INDEX` or `--image-index`. There is no built-in default. The CLI
fetches it with a 30 second timeout and reads at most 8 MiB; a 404 means "no
images are published yet" and is reported as such.

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
