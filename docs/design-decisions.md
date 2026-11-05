# Design decisions

Why the box platform looks like this, and what was rejected on the way. Each
decision was measured before it was coded; the numbers below are from the host
and versions named in "What was measured".

## A box is an instance directory plus a disk on a volume

- **The box id names a Lima instance.** `LIMA_HOME/<box-id>/` is a real
  directory and its `disk` file is a symlink into the box's Nomad host volume.
  Lima has no flag to put an instance anywhere else, so the id is the only handle
  the driver, the volume plugin and the operator share.
- **Only the disk is symlinked, never the instance directory.** A symlinked
  `LIMA_HOME/<name>` still starts and stops, but `limactl list` reports "No
  instance found", because Lima skips non-directory entries. A real instance
  directory of about 21 MB next to a symlinked multi-GB disk keeps discovery
  working. Rejected: moving the whole directory onto the volume. A per-box
  `LIMA_HOME` on the volume is consistent with Lima's discovery rule but was not
  measured.
- **The volume pins the box to one node.** Nomad's host volume filter excludes
  nodes without a compatible volume ("missing compatible host volumes"), so the
  driver never has to move a VM. Volume placement (`node_id`, `node_pool`,
  `constraint`) is the operator's lever, and `sticky = true` keeps a replacement
  allocation on the same disk.
- **The disk only grows.** Lima refuses to shrink a disk, so the image's virtual
  size is the floor for `disk_gib`: an image built with a 20 GiB virtual disk
  makes an 8 GiB box impossible, which is why an image should be built as small
  as the boxes will be.

## The driver's lifecycle

- **StartTask is create-or-adopt, never create twice.** `limactl create` refuses
  an existing instance ("instance already exists"), so the driver creates only
  when the instance is absent and otherwise adopts it with `limactl start`.
  Adoption is cheap because the VM outlives the process that started it: Lima
  reparents the hostagent to PID 1.
- **RecoverTask must error, never return a fake handle.** On a client restart
  Nomad calls `RecoverTask`; on its error it calls `DestroyTask`, emits "Failed
  Restoring Task" and later calls `StartTask` **in the same allocation**. A VM
  crash therefore looks like a failed restore plus a fresh start, which is exactly
  where adopt-by-box-id belongs.
- **DestroyTask stops the VM and keeps the disk.** Deleting a box is the volume
  plugin's job (`Delete`), which stops the VM, deletes the instance and removes
  the volume directory. The two halves never delete the same thing.
- **Lima "Broken" is not always a crash.** Lima marks an instance broken when its
  hostagent API misses a three second deadline, which a busy host can cause.
  Liveness comes from the hostagent process and Lima's pid files, so a transient
  "Broken" does not cost a VM.
- **A dead hostagent is recovered in place.** On "qemu driver is running but host
  agent is not" the driver keeps serving over plain ssh from
  `<instance>/ssh.config` while qemu and the guest are alive, or regains control
  with `limactl stop -f` plus `limactl start`. `stop -f` hard-kills the guest; the
  disk survives either way.
- **One box can be shared by several allocations.** After a node-down round trip
  Nomad restores every allocation of the job, so `StopTask` and `DestroyTask`
  leave the VM alone while another allocation still names the box. Otherwise a
  replacement would start against an empty disk.
- **The plugin must not outlive its Nomad client.** Nomad 2.0 does not reattach a
  restarted client to a running external plugin ("reattachment process not
  found"), so a watchdog stops the plugin when the client goes away. Without it
  the restarted client keeps a dead handle and the restore fails with "plugin is
  shut down". This is a workaround and should go when Nomad fixes reattachment.
- **The driver declares mount support and honours `readonly` itself.** Nomad
  refuses the mount unless the driver advertises `MountConfigSupport`. The volume
  arrives as `TaskConfig.mounts[].host_path`, and nothing is bind-mounted for the
  driver, so it must enforce `readonly` itself.
- **A resize is offline.** `limactl edit` refuses a running instance, so a resize
  is stop, edit, start. The guest grows cpus, memory, its partition and its root
  filesystem by itself at the next boot. Sizes are read from the guest (`df`),
  never from `lima.yaml`: right after create, a 4 GiB disk still showed 3 GiB of
  partition. The cost is real downtime.

## A guest that stops answering

Measured on a real node: a box of 2 vCPU and 2 GiB ran a large cgo build, the
guest stopped answering (no reply to `boxctl info`, `box top` or `box session
ls` - all three hung), and qemu spent about five host cores for twenty minutes
reading the host page cache in 12 KiB pieces. The box disk is a qcow2 of zlib
clusters with the image's cluster size, so qemu was inflating compressed clusters
for a guest that re-read its program files, and the guest being out of RAM is the
likely reason (not proven: no memory figure was kept). Three decisions follow
from what the box looked like from outside.

- **A box disk is built from an uncompressed copy of the image.** The published
  image stays compressed (it is a download, and `images/build.sh` converts with
  `-c`); the driver converts it once per image with `qemu-img convert -O qcow2`
  (no `-c`) into its cache, next to the verified download, as a temporary file
  renamed into place, owned by the runtime user, and reuses it for every box made
  from that image. The copy stays thin: no preallocation, so the clusters the
  image never wrote stay unallocated. A node without `qemu-img` falls back to the
  compressed image and says so in a task event, because a box that starts from a
  compressed disk is better than a box that does not start.
- **A guest that does not answer says so in twenty seconds.** `box status`, `box
  top` and `box session ls` bound their guest call (`guestCallTimeout`, 20 s) and
  report "the guest does not answer (no reply in 20s): ..." -
  `box status` after everything Nomad knows, `box top` and `box session ls` as a
  failure with the timeout exit code that names `box logs` and `box status`. A
  command that hung was the worst of the three: it showed nothing at all, not even
  that the guest was the problem. `box wait`, `box send`, `box run` and `box exec`
  are deliberately not bounded: they run work, and their caller decides how long.
- **The image kills the process that takes the memory.** A guest out of RAM
  becomes unusable before the kernel's own OOM killer acts, so
  `images/provision.sh` installs `earlyoom` and runs it with `-m 10,5 -r 0
  --avoid '(^|/)(boxd|sshd|systemd|lima-guestagent|qemu-ga)$'`: a process dies
  while the guest can still run, the services that keep the box reachable are
  never chosen, and the choice reaches the journal. `boxd` follows the `earlyoom`
  unit (`watchEarlyOOM`, filter `earlyoomTrouble`) and writes "guest earlyoom:
  ..." into its own log, which the task log already carries, so a box that was
  short of memory says which process it lost. earlyoom takes no configuration of
  ours and no secret, and knows nothing about the executors.
- **A graceful stop is allowed 90 seconds, and the client must allow it too.**
  `limactl stop` was killed at 30 s, which powers the guest off hard. Two log
  lines say it: Nomad's own task event "Sent interrupt. Waiting 30s before force
  killing" and the driver's "Graceful stop of box ... failed (signal: killed):
  forcing the VM to stop". The chain is: the job's `kill_timeout`
  (`boxStopTimeout`, 90 s) -> the client caps it with
  `min(kill_timeout, client.max_kill_timeout)`, and Nomad's default for that is
  30 s -> `StopTask` is called with 30 s -> the driver's context, and with it
  `limactl stop`, ends at 30 s. The job already asked for 90 s; `deploy/ansible`
  now writes `max_kill_timeout = 2m` on the client, which is what lets the ask
  arrive.

## Root client, unprivileged VMs

- **The Nomad client runs as root and every VM process does not.** Lima refuses
  to run as root. The driver starts `limactl`, `systemd-run --user` and
  `systemctl --user` as the node's runtime user, with that user's uid, gid,
  supplementary groups (including `kvm`) and an explicit environment (`PATH`,
  `HOME`, `LIMA_HOME`, `XDG_RUNTIME_DIR`, `DBUS_SESSION_BUS_ADDRESS`). Nothing of
  the root client's environment reaches a VM.
- **There is no default runtime user.** The driver and the volume plugin refuse
  to run as root without one and refuse `root` as the value. A wrong guess would
  make every box fail, so a missing value leaves the plugin unhealthy with the
  reason in plain words instead.
- **Everything Lima touches belongs to that user**: the Lima home, the image
  cache, the instance directory, the disk in the volume and its symlink, the
  volume directory, and the staging directory of task files. Task files are
  staged because Nomad renders `local/` and `secrets/` as root while `limactl
  copy` is an rsync that runs as the runtime user. The staging directory is
  `<lima_home>/_staging`: Lima parsed a directory named `staging` as a broken
  instance and the whole list parse failed.
- **The volume plugin takes its settings from the node, not from the request.** A
  volume request may carry no parameters at all; `limactl`, the Lima home and the
  runtime user come from `/etc/box/volume.json`, written by the node role. A
  request with any parameter is refused with a plain error, so a coordinator
  cannot choose which binary runs on a node.
- **The CPU cap is `cpu.max` in the same scope: the box's vCPUs plus one core.**
  A Nomad CPU reservation is only a weight, so without a cap one box took about
  five cores of an eight-core node with 2 vCPUs: the vCPUs are limited by their
  count, but qemu's main loop and its disk threads are not. The extra core keeps
  those threads from taking time from the guest. The driver reads `cpu.max` back
  and says so in a task event when it is missing, because systemd accepts
  `CPUQuota` on a user manager without the cpu controller and enforces nothing.
- **A vCPU reserves one core of its node, less its part of the client's own
  reservation** (`(total - reserved) / cores`). A fixed 1000 MHz meant half a core
  on a 2 GHz node, so Nomad placed about twice the boxes the node can run. With
  this figure a node offers as many vCPUs as it has cores: 8 cores take four boxes
  of 2 vCPU, and the fifth is refused. The figure comes from the box's node, so it
  is set at `box create` and again by `box resize` and `box upgrade`.
- **The memory cap is a systemd user scope with `memory.max` and
  `memory.swap.max=0`.** The budget is guest RAM plus about 150 MB (qemu about
  guest RAM plus 60 MB, the hostagent about 60 MB). `memory.max` alone is soft:
  with swap available a 300 MiB allocation inside a 100 MiB limit succeeded. A
  limit that is too low kills qemu mid-flight and loses unflushed guest writes.
  If the scope cannot be created the task fails with the reason — there is no
  silent fallback to an unlimited VM (an operator can opt out explicitly).

## The guest

- **`boxd` and `boxctl` are one binary, and the CLI never uses the guest
  network.** `boxd` serves a small JSON API on `/run/boxd/boxd.sock`; `boxctl` is
  the same binary by `argv[0]`. Every guest call is
  `nomad alloc exec -task box <alloc> /usr/local/bin/boxctl ...`, carried by the
  driver through `limactl shell`. Prompts and files travel on stdin, directories
  as a tar stream, so no guest port is exposed. `boxctl` prints one JSON document
  and exits 0, or prints `{"error":{"code","message"}}` and exits 1; diagnostics
  go to stderr.
- **Every event is one JSON line on stdout and in `events.jsonl`.** The driver
  streams `journalctl -u boxd` into the Nomad task log, so `nomad alloc logs` is
  the activity feed, and `boxctl events --since` is a resumable cursor over the
  same events.
- **One executor process per session, started lazily.** ACP over stdio, one
  process per session, spawned on the first `send`, so an idle session costs no
  memory. One session costs about 180 MiB RSS / 81 MiB Pss, which puts 4 to 5
  sessions in a 1 GiB box. The 6.2 GiB virtual size is harmless but fatal under
  `RLIMIT_AS`, which is why the limit is a cgroup.
- **Continuity is the transcript plus the stored root session id.** ACP has no
  reattach (`session/load` answers "method not found") and the ACP session id is
  not the transcript file name, so boxd stores the executor's root session id as
  `executor_session` and resumes with `-r <root id>` on the same session
  directory. Resume survived a SIGKILL of the executor and of boxd; only the
  in-flight turn is lost.
- **Rejected: prime-agent daemon mode per box.** It doubles memory (supervisor
  plus worker, about 274 MiB RSS), the CLI cannot address a custom socket, and a
  hand-started daemon never finishes the handshake. Reattaching to a live ACP
  session is impossible either way.
- **Cancel is a kill, and permission requests are auto-answered.** prime-agent
  0.9.8 does not implement `session/cancel`, so boxd sends the notification,
  waits a five second grace, kills the process group, marks the turn cancelled and
  resumes from the transcript on the next send. An agent's
  `session/request_permission` picks the first allow option, because a box has no
  human to ask.
- **Credentials live on a tmpfs, never on the disk.** The task's `secrets/`
  directory reaches the guest as `/run/box/secrets`, a read-only mount of the
  box's files on the host's tmpfs, and boxd derives everything it needs below
  `/run/boxd`. Sessions and transcripts stay on the disk; no secret does. The
  image build and the guest checks fail if any executor credential location is
  non-empty.
- **An executor's HOME is on the disk, its credentials and configuration are
  not.** boxd gives each executor `HOME=/var/lib/boxd/homes/<executor>` and
  `XDG_CONFIG_HOME=/run/boxd/executors/<executor>-config`; the credentials stay in
  the runtime copy of the secrets and in the environment. An executor downloads a
  Python, builds a virtual environment and runs it out of its HOME (prime's
  Python tool does all three); the guest's `/run` is a tmpfs of a tenth of the RAM
  mounted `noexec`, so with HOME there that tool never started. Rejected: a
  larger tmpfs without `noexec`, which costs hundreds of MiB of a box's RAM and a
  new download on every boot. The cost: what a harness writes into HOME by itself
  reaches the disk, so a harness that stores a credential in HOME (a login
  command, not the box's way of configuring it) would leave it there.
- **Task files are a read-only mount, put in place before the VM boots.** The
  driver puts the task's `local/` and `secrets/` files into
  `/run/box-files/<box>` on the host's tmpfs (`files_root`), owned by the runtime
  user, before it starts the VM; Lima mounts the two directories read-only at
  `/etc/box/local` and `/run/box/secrets` (9p without a cache on qemu, virtiofs
  on vz). The files are there from the guest's first second, after a reboot of
  its own, and a re-render is visible at once (then `boxctl reload`). A stopped
  box keeps none of them on the host, and the driver's start removes those of
  every box whose VM does not run, which is what a crash of the driver leaves.
  macOS has no tmpfs: there the files go into the task's private directory,
  where Nomad keeps its own copy of the secrets on that disk and which it
  removes with the allocation, and `box-files/<box>` is a link to it, so a crash
  leaves no copy anywhere Nomad does not clean up. Rejected: copying them into the guest
  after it booted, as a tar stream over `limactl shell` (up to v0.1.13): boxd
  started before they arrived, a guest that rebooted by itself lost them until
  the task restarted, and every fix of that was another mechanism. Also
  rejected: the copy on the host disk the driver made up to v0.1.3. The cost:
  9p shows the files as the runtime user's, whose uid is the guest's default
  user's, so that user reads them directly; it has passwordless sudo anyway, so
  root-only files in the guest were never a boundary from it.
- **The key is read from Vault, never copied from a host.** An item named
  `prime/auth.json.tmpl` is Nomad template text holding a Vault path, not a key:
  `box create` embeds the text in the job and Nomad renders the file on the node
  with the box's own workload identity, so the variable and the job spec hold the
  path. Rejected: staging the credential out of a host's `auth.json`, which is
  what the staging script used to do -- it puts the key through a shell, a
  staging directory and a Nomad variable, and it makes every host with a login a
  source of the cluster's credentials. The block is `change_mode = "noop"` (a
  renewed or revoked token must not restart a VM), `disable_file` (Nomad's token
  files would otherwise be copied into the guest, because the driver copies the
  whole `secrets/` directory) and no `env` (the task process is the VM, not an
  executor). A job with no template item has no Vault block at all, so a cluster
  without Vault keeps working exactly as before.
- **One provider per box.** `box executors set --provider` keeps one provider's
  credential and definition out of `auth.json`, `models.json` and
  `opencode.json`, because a box runs one provider and the whole set does not fit
  a Nomad variable (64 KiB).
- **Rotation does not recreate a box.** The template `change_mode` stays `noop`.
  The driver fingerprints the task's `local/` and `secrets/` directories, and
  when Nomad re-rendered a template it copies the files into the guest again and
  runs `boxctl reload`: idle sessions restart their executor now, a session in a
  turn restarts it when the turn ends. The VM, the allocation and the transcripts
  are untouched.
- **The image grows the root filesystem itself.** cloud-init's growpart refuses
  a root partition that is not the highest one (the recipe's image numbers its
  EFI and boot partitions 14 to 16), so the image is expected to carry a small
  service that grows the partition and the filesystem after the root is mounted
  read-write. A 10 GiB box then has 8.7 GiB usable instead of 6.
  `docs/guest-image.md` has the section that does it.

## The CLI

- **The CLI owns the job and volume conventions, and Nomad is the only state.**
  A box is the job `box-<id>` (meta `box`, `box_id`, `image`, `arch`; group and
  task `box`) plus the volume `box-<id>` mounted as `disk`. `box start`
  re-renders the job from its own spec, so a hand-written box job is normalised on
  start. That is deliberate: one definition, in the CLI.
- **A box is its volume; Nomad's job is not a durable record of a stopped box.**
  `box stop` leaves the job dead, and Nomad's job GC deletes a dead job with its
  history after `job_gc_threshold` (4 hours by default). On a real cluster a box
  stopped at 02:16 answered "no such box" at 07:13, with its disk intact, while
  `box stop` had promised that `box start` brings it back. The rendered job is
  therefore kept where GC does not reach: the Nomad variable
  `nomad/jobs/box-<id>/spec`, written by `create`, `start`, `resize` and `upgrade`
  and deleted by `rm`. It is the job as JSON and nothing else, so it holds no
  secret: a credential reaches a box through a template that names a Vault path or
  reads the executors variable at render time, never as a value in the job.
  Rejected: keeping the description in a file on the coordinator (a second state,
  which the coordinator's loss would take with it) and in the volume directory on
  the node (a node-side API the CLI does not have).
- **A box with a description, a volume and no job is `stopped`; a volume with
  neither is an orphan.** A box made before descriptions existed cannot be started
  once its job is collected: there is no way back, so `box start` refuses it and
  says that `box rm <id> --yes` deletes the disk. Inventing a job for such a disk
  would run a VM nobody can account for.
- **`box create` chooses the node itself and cleans up after a failure.** It needs
  a node with the driver, the volume plugin, the right architecture and enough
  free cpu, memory and disk, computed like the scheduler. It creates the volume on
  that node and registers the job; a placement failure is explained from the
  evaluation and the half-made box is removed, so a failed create leaves no disk.
- **A lost answer deletes nothing.** `box create` re-reads the job before any
  cleanup: job exists, keep the box; job definitively 404, delete the disk; Nomad
  unreachable, keep both and print the two commands that look and clean up.
  `box rm` polls (bounded by `--timeout`) until the volume registration is gone,
  because `box create` refuses a name Nomad still lists.
- **Waiting is a wait, not a sleep.** `box create`, `start` and `resize` report a
  box as running only when it stays running across a poll and no allocation of the
  job is pending. Guest commands wait up to two minutes for a rising box and fail
  at once for a stopped or failed one. `box wait` and `box events --since` are the
  same idea for sessions.
- **Exit codes and streams are fixed.** 0 ok, 1 error, 2 usage, 3 not found, 4
  timeout still running; `box run` and `box ssh` return the guest's own exit code;
  `box cp` streams a tar over `alloc exec`, refuses a `..` entry and never writes
  outside the destination.
- **Capacity counts the VMs Nomad does not.** The driver fingerprints every Lima
  instance (state, cpus, memory, disk, whether a task owns it) and `box capacity`
  subtracts the running instances without a running task from the node's free cpu
  and memory, reporting them as uncounted.

## ACLs and credentials

- **The CLI token is scoped to what the CLI does.** `box-operator` grants jobs,
  host volumes, `alloc exec`, log reads, node read, read/write of the one
  executors variable and of the boxes' descriptions (`nomad/jobs/box-*/spec`) —
  and nothing else. A test parses the policy with Nomad's own
  ACL engine and fails if the policy and the command set drift apart.
- **A box's workload identity cannot read the shared variable.** Nomad scopes a
  workload identity to `nomad/jobs/<job>[/<group>[/<task>]]`, and
  `nomad/jobs/box-executors` is a sibling of `nomad/jobs/box-<id>`, so the
  templates would render empty files and every executor would report "not
  configured". The fix is one job-attached policy per box
  (`nomad acl policy apply -job`), which `acl.yml` and `deploy/acl/apply.sh`
  apply.
- **`box create` does not touch ACLs.** The CLI token has no ACL write on purpose,
  so attaching the workload policy to a new box is one operator command.
- **A box can read the executors variable and cannot write it**, so one
  compromised box cannot hand the credentials to another.

## Images

- **Base images are pinned by sha256 through an index, not by a tag.** The index
  lists versions and architectures with `url`, `sha256` and `size`. The driver
  downloads, verifies and caches the image and refuses a corrupt cache entry.
- **A published version never changes.** An index maps a version to one url and
  one digest per architecture, and a box boots the digest it was created from.
  Whoever publishes an index is responsible for a version continuing to mean one
  image; a mistake is fixed by publishing the next version.
- **The repository keeps the recipe, never the image.** `images/` builds an image
  (`provision.sh` is the recipe, `build.sh` the automation), `images/verify.sh`
  proves the result boots and passes its checks, and `images/publish.sh` uploads
  it to the operator's own store. Keeping the binary out of the repository and out
  of CI is the point: an image goes to whoever the operator serves, versions are
  theirs to publish, and the maintainer hosts nothing. What an image contains is
  still the operator's decision -- `docs/guest-image.md` is the contract, and an
  image of your own that meets it needs nothing from the recipe.
- **An image names the boxd it carries.** `BOXD_RELEASE` takes a release asset
  (digest checked against the release's `SHA256SUMS`), `BOXD_COMMIT` builds one
  commit, and with neither the checkout's HEAD is the source and a dirty tree
  under `cmd/` or `internal/` is refused. The choice is recorded in the build
  manifest and in the image at `/etc/box/image.json`, so "which boxd is in this
  box" has one answer, from the image itself.
- **Site addresses are deployment settings.** Mirrors, the store and its token are
  `BOX_MIRROR_*` and `BOX_STORE_*` in the operator's environment; the recipe's
  public upstream defaults are written down once, in `provision.sh`, and no
  private host, mirror or secret manager is named anywhere. The versions and
  their sha256 pins stay in force through any mirror.
- **The image carries no secret and no key.** No ssh host keys, no authorized
  keys, no passwords, no machine-id, no shell history, no journal: cloud-init
  generates them on the first boot of every box. `guest/no-secrets.sh` is the
  check an operator runs on an image before capturing it and inside a booted box.
- **Baking a box into an image is a task of the driver, not a command of the
  CLI.** An image is made from a disk, the disk is a qcow2 on one node's volume,
  and the driver is the host-side code that owns that disk: it knows the volume
  path, it runs as the runtime user, and it already writes the node's image cache.
  The CLI on a coordinator can reach neither the disk nor the cache; all it can do
  is register work and read what came of it. So the bake is a batch job of the box
  driver that runs no VM -- a task, which is what Nomad schedules, reports and
  stops, with a log when it fails -- and the CLI's part is the seal inside the
  guest, the stop, the registration and the wait. A bake that is stopped removes
  its temporary file before the task ends, so no half image stays in a cache; a
  bake that ran leaves a record and a file, not an allocation to keep.
- **A baked image is a draft first, and published second.** The two steps are the
  two things an image's life needs: it is made on one node, and it is distributed
  from one store. A draft is the file where the disk was, plus a record beside it,
  and the driver publishes that record as node attributes so every coordinator can
  see it without reading a node's disk: `box images` lists the drafts of the
  whole cluster, and `box create` from one pins the box to the node that holds it.
  Publishing is a second, deliberate act, because it is not the same kind of
  change: it writes the operator's store and every node can create from it from
  then on. So a draft costs no decision, and a publish costs the owner's -- the
  command refuses without `--yes`, the store's url and token are the operator's
  variable, and the publish job reads that variable with a policy of its own
  rather than through a box's. A version is immutable in both steps: a published
  version never changes, and a bake never reuses a number, because the number it
  takes is one above every published version and every draft of that name.
- **The seal searches the disk for the credential values themselves.** Deleting
  the files a credential is written into is not the same as deleting the
  credential, because a file named like a credential is not the only way a
  credential reaches a disk: a tool writes its key into a log, a shell history
  keeps the command that held it, a build artifact carries it, a database row has
  it in a column nobody named. So after the deletions the seal reads the values
  out of the secrets directory -- every JSON string and every whole line long
  enough to be a value -- and looks for them on the guest's own filesystem,
  skipping the tmpfs the credentials live on and the bundle. A hit names the path
  and never the value: the message is what a person acts on, and repeating a
  credential into a task log would be the leak the check exists to prevent. What
  the check proves is narrow and the right thing to prove: no value that is in
  this box's secrets directory is anywhere on the disk that becomes the image.

## The platform bundle

- **The fast-moving part of a box is mounted, not baked into the image.**
  `boxd`, `boxctl` and the executors change far more often than the OS and the
  toolchains, and a project image derived from a base image is nobody's to
  rebuild: whoever owns it will not rebuild it for a `boxd` fix. So the platform
  leaves the image and becomes a versioned bundle -- an archive with a
  `manifest.json`, `bin/boxd`, `bin/boxctl` and the tool directories -- that the
  node downloads by sha256 and mounts read-only at `/opt/box` before the guest
  boots. Lima itself does this for its own guest agent: `lima-guestagent` arrives
  through cidata on every boot instead of being baked into the image, which is
  why a Lima upgrade does not need a new image either. The image keeps the OS,
  the toolchains and a `boxd.service` that runs `/opt/box/bin/boxd` and requires
  the mount (`RequiresMountsFor=/opt/box`).
- **A bundle is a directory per digest, and a directory never changes.** The
  cache is `<image_cache_dir>/platform/<sha256>/`, unpacked once, written as a
  temporary directory and renamed into place with a marker file last, so a half
  download or a half unpack is never mounted. The guest mounts that directory
  with `cache: fscache` on qemu (the directory is immutable, so caching every
  read costs nothing) while the task files keep `cache: none` (the driver
  replaces files in them and the guest must see the new one at once). A new
  bundle is a new directory: there is no cache to invalidate and no upgrade in
  place. qemu mounts over 9p, which is Lima's own default there and what the task
  files already use; vz gets virtiofs, which is all it has.
- **A mount, not a data disk and not a copy.** An installed copy would need the
  network in the guest and would drift from what the node pinned; a data disk
  would be per box and would have to be filled at every start; a copy after boot
  is what the task files did up to v0.1.13, and boxd then started before its own
  files arrived. A mount exists from the guest's first second, after a reboot of
  its own, and is read-only, so a box cannot change the platform it runs.
- **An image says it expects a bundle, twice.** `"platform": "mount"` in the
  image's `/etc/box/image.json` and the same field on its entry in the index.
  The CLI adds the `platform` block to the job only for such an image, so an
  older image keeps running its own `boxd` and its own `/opt/box` is not hidden
  by a mount. Without the block the driver behaves exactly as it did before the
  feature existed; the mount is one optional entry among the task files.
- **A changed block is a changed instance configuration.** The block names the
  bundle, so `box upgrade` is a re-registered job whose block names another one.
  The driver compares the bundle the instance mounts (read out of its
  `lima.yaml`) with the one the task asks for, stops the box if they differ,
  writes the mount and starts it again -- the same stop-edit-start path a resize
  takes, and for the same reason: Lima cannot change a running instance. The
  file is edited as a document rather than re-rendered from the spec, because
  Lima adds fields of its own when it creates an instance (the local ssh port,
  the image digest, the host resolver) and losing one of them breaks a boot.
  `limactl edit` cannot help here: it takes cpu, memory and disk only.
- **The bundle is unpacked as an unprivileged user's data.** Every entry is
  checked before it is written: a path that escapes the root, an absolute path,
  an absolute symlink, a symlink pointing outside the bundle, a device or a fifo
  is refused and the whole bundle is discarded. The guest reads that tree, and a
  path outside it is a path into the node. The three symlink shapes the
  contract asks for (`bin/boxctl -> boxd`, `bin/<tool> -> ../tools/...`) are
  relative and stay inside.
- **Rejected: shipping the bundle inside the image.** It would put the
  fast-moving part back where a derived image cannot follow it, and every
  executor update would again be an image build, a publish and a box recreate.

## Deploy

- **One release directory per version and one symlink.** The Nomad client
  configuration names the plugin paths through `/opt/box/current`, so the paths a
  running client loads never change, and a rollback is a symlink flip between
  installed releases. A new version installs next to the live one and the symlink
  moves only when the install is complete and verified against the release
  checksums.
- **Two roles, and only one writes the client configuration.** `box_node` owns
  the release, the binaries, the volume settings file and the runtime user;
  `nomad_client` owns all of `/etc/nomad.d`, the secrets, the service and the one
  restart, and renders the driver's plugin block from `box_node`'s variables.
  One place decides what the client runs, one place validates it, one restart.
- **A client restart must not kill the boxes.** The Linux unit uses
  `KillMode=process` and the macOS plist uses `AbandonProcessGroup`, so the
  restart signals only the Nomad process and the hostagents and VMs survive it.
  Nomad then re-attaches through `RecoverTask`.
- **The release version must be semver.** Nomad refuses a plugin whose reported
  version is not semver ("malformed version"), so a build from `git describe` with
  a prefix is not installable; `build.sh --version` or a `vX.Y.Z` tag is.
- **The macOS client is a LaunchDaemon as root.** Launchd's per-user agents need
  someone logged in, and the client must be there regardless; with `vz` the VM
  lives inside the hostagent, which the driver starts as the runtime user. So the
  split is the Linux one.
- **A NixOS node is refused unless asked.** Writing `/etc/nomad.d` on NixOS is
  lost at the next `nixos-rebuild switch`, so the role stops and the operator puts
  the same keys in the machine's Nix configuration.

## What was measured

The measurements behind these decisions come from one Linux host: lima 2.2.0,
qemu 11.1.2, Nomad 2.0.7, systemd 262 with cgroup v2, `/dev/kvm`, and prime-agent
0.9.8. Everything about macOS, arm64 and other versions is written from those
results, not measured on them. `docs/known-limits.md` says what that leaves open.

The platform mount was measured end to end on that host, without Nomad: the
driver's own code (`box.Manager`) booted a real Lima VM from an Ubuntu 24.04
minimal cloud image and a hand-made bundle (`manifest.json`, `bin/hello`,
`bin/boxctl -> hello`), and `cat /opt/box/manifest.json` in the guest printed it.
The guest showed `/opt/box` as `9p (ro,relatime,cache=0x8f)` -- read-only and
fscache -- next to the task files' uncached 9p mounts, and `touch /opt/box/...`
was refused with "Read-only file system". A second, different bundle with the
same block changed made the driver stop the box, rewrite the mount in the
instance's `lima.yaml` and start it again, and the guest then read the new
`manifest.json`; the old bundle stayed in the cache. What was *not* measured:
any box under Nomad, a real bundle with the real executors, arm64, and `vz`.
