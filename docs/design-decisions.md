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
- **Credentials live on a tmpfs, never on the disk.** The driver writes the
  task's `secrets/` directory to `/run/box/secrets` (root-only, mode 0700) and
  boxd derives everything it needs below `/run/boxd`. Sessions and transcripts
  stay on the disk; no secret does. The image build and the guest checks fail if
  any executor credential location is non-empty.
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
  host volumes, `alloc exec`, log reads, node read and read/write of the one
  executors variable — and nothing else. A test parses the policy with Nomad's own
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
