# Architecture

`box` runs one Linux VM per Nomad task. The VM is called a **box**. Its disk is
a Nomad dynamic host volume, so the scheduler places the allocation on the node
that holds the disk, and a replacement allocation adopts the same VM instead of
creating a new one.

```
            coordinator host                          Nomad client node
  +-------------------------------+        +------------------------------------+
  | box (CLI)                     |        | nomad agent (root)                 |
  |   jobs, volumes, ACL, capacity|  API   |   driver plugin  box               |
  |   executors variable          |------->|   host volume plugin  box-volume   |
  +-------------------------------+        |                                    |
            |                              |   limactl hostagent (runtime user) |
            | nomad alloc exec             |     +-- qemu / vz  -> guest        |
            v                              |   box-<id>/  (real dir)            |
  +-------------------------------+        |     disk -> volume/box-<id>/disk   |
  | guest: boxd + boxctl          |<-------+                                    |
  |   executor processes (ACP)    |        +------------------------------------+
  +-------------------------------+
```

## The parts

| part | what it is |
| --- | --- |
| `cmd/box-driver` (`internal/driver`) | the Nomad task driver named `box`: it turns one task into one Lima VM |
| `cmd/box-volume` (`internal/volume`) | the Nomad dynamic host volume plugin `box-volume`: it creates and deletes a box's disk directory, and deletes the VM that owns it |
| `cmd/box` (`internal/cli`) | the CLI: create, start, stop, rm, resize, upgrade, run, ssh, cp, logs, status, capacity, sessions, executors, images, platform bundles |
| `cmd/boxd` (`internal/boxd`) | `boxd` and `boxctl` in one binary: the guest daemon that runs agent sessions, and the client the CLI execs |
| `internal/box` | one Lima instance: the disk on a volume, the image cache, resize, the cgroup memory cap |
| `internal/limactl` | a thin wrapper over `limactl` (list, create, start, stop, edit, shell, copy) |
| `images/` | the guest image recipe and its automation: `provision.sh`, `build.sh`, `boxd-source.sh`, `verify.sh`, `guest-checks.sh`, `publish.sh`. The repository keeps the recipe, never an image binary |
| `guest/` | the operator's tools for their own image: `boxd-install.sh`, `no-secrets.sh` |
| `deploy/` | the Ansible kit for a node and a coordinator, the ACL policies, the acceptance script |
| `claude-plugin/skills/box` | the `box` skill an agent reads, packaged as a Claude Code plugin |

## A box in Nomad

A box is two Nomad objects with the same name:

- a **job** `box-<id>`: one group `box`, one task `box`, driver `box`, with the
  box id, the image (url and sha256), cpus, memory, disk size and the log
  command in the task config. The group mounts the volume `disk` and sets
  `disconnect { replace = false, reconcile = "keep_original" }` so a client
  restart never replaces the allocation.
- a **dynamic host volume** `box-<id>`: type `host`, plugin `box-volume`,
  capabilities `single-node-writer`/`file-system`. It carries no parameters at
  all — the node decides where the disk lives (see below).

The scheduler's host volume check pins the allocation to the node that holds the
volume. Moving a box means moving its volume; a box never migrates by itself.

## The disk, and why only the disk is on the volume

Lima requires `LIMA_HOME/<name>` to be a real directory: `limactl create`
refuses a path that already exists, even as a symlink, and `limactl list` skips
any entry that is not a directory, so a symlinked instance directory makes the
instance invisible to every later `limactl` call. The `disk` file inside it is
an ordinary file, and symlinking that one file into the volume works completely.

So a box is:

```
<lima_home>/<box-id>/            a real directory, about 21 MB (sockets, ssh config, cidata)
<lima_home>/<box-id>/disk        a symlink to
<volume host path>/box-<id>.qcow2    the qcow2 the VM boots from
```

The instance directory is a few megabytes of control files; the disk, which is
gigabytes and holds the work, is the part that must be on a volume and must
survive. That is exactly what the volume plugin manages.

## Lifecycle

The driver's three lifecycle calls are one idea: a box is identified by its id,
and the VM is adopted, never duplicated.

- **StartTask** is create-or-adopt. A running instance is attached to; a stopped
  one is resized offline and started; a broken one (Lima says `Broken` because
  the hostagent was lost while qemu kept running) is force-stopped and started
  again; a missing instance is recreated around an existing disk.
- **RecoverTask** re-attaches only when the VM and its hostagent are alive.
  Nomad then keeps the same allocation and the same VM. When it fails, Nomad
  destroys the task and starts it again *in the same allocation*, which is
  exactly the adopt path. A VM crash is therefore the normal path, not an error
  the operator has to repair.
- **DestroyTask** stops the VM and never deletes the instance or the disk. The
  box keeps its files until the volume is deleted (`box rm`), which is where the
  volume plugin stops the VM, deletes the instance and removes the directory.

**Resize is offline.** `limactl edit` refuses a running instance, so a resize is
stop, edit, start. The guest grows its partition and root filesystem by itself
on the next boot, and the driver reports what the guest says (`df`), not what
the yaml says. A disk can grow and never shrink.

**A new platform bundle is a stop and a start too.** The bundle of `boxd` and the
executors is a read-only mount of the instance's `lima.yaml` at `/opt/box`, one
cache directory per sha256, so a task that names another one -- what `box
upgrade` re-registers -- makes the driver stop the box, write the mount and
start it again. The disk is untouched and every bundle stays in the cache.
`docs/design-decisions.md` has why the platform is a mount and not part of the
image.

## Processes and the memory cap

On Linux a box is a detached `limactl hostagent` plus a `qemu-system-x86_64`
process. The VM outlives whatever started it, which is what makes adoption
possible. On macOS with `vz`, the VM lives inside the hostagent process itself.

The Nomad client runs as root; Lima refuses to run as root. So every process the
driver starts runs as the node's unprivileged **runtime user**, with that user's
uid, groups (including `kvm`) and an explicit environment. Nothing of the root
client's environment reaches a VM.

On Linux the VM runs in a transient systemd scope with `memory.max` = guest
memory + overhead and `memory.swap.max=0`. Both matter: with swap allowed the
limit is soft and the guest quietly overflows, and a limit that is too low kills
qemu mid-flight and loses unflushed guest writes. The disk survives either way.

## The guest

The base image boots into a systemd unit `boxd.service`. boxd owns the agent
sessions inside the box:

- one executor process per session, over the Agent Client Protocol (`prime`,
  `opencode`, `claude`), each with its own home, model and transcript;
- a session store on the box disk (`/var/lib/boxd`) so a restarted box reopens
  its transcripts instead of losing them;
- an event feed: every event goes to the journal (which the driver streams into
  the Nomad task log) and to `sessions/<id>/events.jsonl`.

The credentials an executor needs arrive as the task's `secrets/` directory and
land on a tmpfs, `/run/box/secrets`, never on the box disk. One task template
writes each file: a plain item of the `nomad/jobs/box-executors` variable is read
out of it, and a `.tmpl` item is template text that reads the key from Vault with
the box's own workload identity, so the key is in no variable and no job spec.
An image therefore carries no credential, and a box that gets none still starts:
it reports each executor as not configured instead of failing.

`docs/boxctl-contract.md` is the interface between the CLI and boxd: commands,
JSON objects, error codes and the guest layout.

## Host to guest

The CLI never uses the guest's network. Every call is
`nomad alloc exec -task box <alloc> /usr/local/bin/boxctl ...`, so a box needs no
ssh key, no published port and no address. `box cp` streams a tar over the same
channel; `box ssh` is an interactive shell over it.

## Images

A box boots an image the operator built, and -- for an image that says so -- a
platform bundle the node mounts into it. There are two ways to an image:
`images/` is the recipe and the build automation -- `images/build.sh` builds,
`images/platform.sh` builds the bundle, `images/verify.sh` boots and checks the
two together, `images/publish.sh` uploads both to the operator's store -- and any
qcow2 that boots under Lima, runs `boxd` and meets the guest contract (cloud-init
on the first boot, passwordless `sudo` for Lima's user, an empty `/opt/box`, a
`boxd.service` that runs `/opt/box/bin/boxd`, `/run` a tmpfs) works just as well.
The release carries `boxd` for both guest architectures and `boxd-install.sh`,
which puts it into an image of the operator's own that carries no bundle.
`docs/guest-image.md` is the contract; nothing here ships or publishes an image
or a bundle.

Why the split: `boxd` and the executors change far more often than an OS image
should, and an image derived from another one is nobody's to rebuild. The bundle
is a versioned archive the node downloads and mounts read-only at `/opt/box`
before the boot, the way Lima delivers its own guest agent; the image keeps the
kernel, the toolchain and a stub unit. A bundle is a new version of an executor
or of `boxd` with no image build at all.

An image is identified by a URL and a sha256 through an index the CLI reads
(`BOX_IMAGE_INDEX`), so a published version never changes: a new build is a new
version. The driver downloads it, verifies the digest and caches it by digest.

## The deploy kit

`deploy/` installs one release directory per version and points a symlink at the
live one, so a rollback is a symlink flip:

```
/opt/box/releases/<version>/plugins/box                    the driver
/opt/box/releases/<version>/host_volume_plugins/box-volume  the volume plugin
/opt/box/releases/<version>/bin/box                        the CLI (coordinator)
/opt/box/current -> /opt/box/releases/<version>
```

Two roles per node: `box_node` owns the release, the binaries, the volume
plugin's node settings file and the runtime user; `nomad_client` owns all of
`/etc/nomad.d`, the secrets, the service and the one restart, and renders the
driver's plugin block from `box_node`'s variables. `deploy/README.md` is the
whole of it.

## What shaped this design

Every constraint above was measured before it was coded: Lima's instance
directory and resize rules, the two-process model and SIGKILL recovery, the
memory overhead, Nomad's dynamic host volume plugin API and the workload
identity's read-only Variables access. `docs/design-decisions.md` records the
decisions and what was rejected; `docs/known-limits.md` records what is still
open.
