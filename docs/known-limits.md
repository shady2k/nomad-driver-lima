# Known limits

What is missing, fragile or unproven. Each item says what it means for you and,
where one is known, what would close it. Nothing here is a promise to fix it.

## Platform coverage

- **The measurements come from one host.** lima 2.2.0, qemu 11.1.2, Nomad 2.0.7,
  systemd 262 with cgroup v2 and `/dev/kvm` on one Linux machine. A different lima
  or qemu version can change the process model, the pid files or the guest agent's
  behaviour. Only the Linux/qemu path is measured by the test suites.
- **macOS and Apple Virtualization run on one Mac, by hand, not in a test.** One
  Mac (an M4, arm64) runs the real Nomad client under launchd with `vz` boxes:
  boxes were created and deleted on it through Nomad, read their executor key
  from Vault, and ran a prime session whose Python tool worked (driver v0.1.6,
  an image built from the recipe). In CI the launchd plist and the arm64
  configuration are still checked as files, and the two role fixtures run on a
  GitHub `macos-15` runner with a recording `launchctl` and a stub `limactl`. A
  client restart (`kickstart -k`) with a box running has not been measured, and
  nothing reruns the Mac path when the code changes: a Mac in CI would.
- **A rare hang of Lima's readiness checks on a Mac, cause unknown.** Once, in a
  direct `limactl start` under `vz` (no Nomad, no driver), the first SSH check
  passed and the second stayed on an open connection until it was stopped; 20
  later starts, over usernet and over vsock, did not repeat it. Under Nomad such a
  start fails after Lima's own wait (10 minutes) and the task's restart policy
  starts it again. The guest's own logs of that run were lost, so nothing here says
  why; a repeat with the instance kept would.
- **arm64 has no automated end-to-end test.** `box-driver`, `box-volume` and
  `boxd` cross-build for `linux/arm64` in every release, and arm64 boxes run on
  the Mac above, but no driver or CLI suite runs a box on arm64.
- **A guest is Linux only.** A box on a Mac is an arm64 Linux VM; nothing here
  boots a macOS guest, so a macOS build is checked by CI on a macOS runner, not
  in a box.
- **NixOS is refused by the client role.** The role writes `/etc/nomad.d`, which
  on NixOS is lost at the next `nixos-rebuild switch`, so the play stops unless
  `nomad_allow_nixos` is set. A NixOS node needs a hand-written configuration.
- **One box at a time in the tests.** Every suite boxes at most one VM (up to
  2 vCPU / 3 GiB) on one local node. Multi-node placement, several boxes on one
  node and volume capacity pressure are untested.

## Security

- **All boxes share one set of executor credentials.** They are one Nomad
  variable, so a compromised box can use them, and so can anyone allowed to read
  that variable. Per-box credentials would need a per-box variable and a policy
  per box.
- **The CLI token is close to platform-root.** `box-operator` can `alloc exec`
  into any box, submit and stop jobs, create and delete host volumes, and rewrite
  the executor credentials. There is no read-only role.
- **No network isolation.** A guest's traffic leaves through the node's user-mode
  NAT; there is no per-box firewall, no egress policy and no private network
  between boxes.
- **A box is not a boundary against its node.** The VM is a process of the node's
  runtime user. A guest escape is a host compromise.
- **Root inside the box, and one disk for all sessions.** `boxd` and every agent
  session run as root in the guest and share its disk. Sessions are separated by
  process, not by user or by filesystem.
- **The boxd socket is world-writable.** `/run/boxd/boxd.sock` is mode 0666 so an
  unprivileged user in the guest can run `boxctl` without sudo. Any process in the
  guest that can reach the socket can drive every session. A per-user socket, a
  group or a sudo rule would close it.
- **Permission requests are auto-approved.** An agent's
  `session/request_permission` is answered with the first allow option, because a
  box has no human to ask, so an agent can run a risky tool call unattended. A
  deny-by-default policy or a broker outside the box would close it.
- **The guest journal needs passwordless sudo.** The default guest log command is
  `sudo -n journalctl -u boxd`, because the task user cannot read another user's
  journal. Putting the box user in `systemd-journal` in the image would close it.
- **On macOS the rendered key passes the node's disk.** Nomad renders a task's
  `secrets/` directory into the allocation directory. On Linux, with the client
  running as root, that directory is a tmpfs; on macOS it is a plain directory
  under the client's `alloc_dir`, on the disk, for as long as the allocation
  lives, and a crash can leave it there. The driver adds no copy of its own: it
  streams the files into the guest. Putting `alloc_dir` on a RAM disk would
  close it, at the cost of the allocations' local logs and files after a reboot;
  it has not been tested.
- **A key in a box is readable by everything in the box.** The executor gets it
  as a file on the guest's tmpfs or in its environment; the executor, its
  children and root in the guest can read it, and so can root on the node (the
  guest's memory is a process of the node). Keeping it off the disks protects it
  from a lost or copied disk, not from the box itself. Scope the Vault role to the
  one secret and give boxes a key of their own that can be revoked.
- **Credentials at rest are in the Nomad variable**, stored in the cluster's
  state. This project does not encrypt it; mTLS and ACLs are the protection.
- **ACLs were measured on a cluster without them.** The workload policy is
  applied per box and the operator policy is tested against Nomad's ACL engine,
  but the whole path (a box job reading the variable under real ACLs) was not run
  end to end. A box created after `acl.yml` needs its workload policy attached
  once (`deploy/acl/apply.sh workloads <id>`); until then its executors report
  "not configured". The first real run found the operator policy missing
  `host_volume "box-*"`, and the driver declaring no filesystem isolation, which
  made Nomad ask for `alloc-node-exec` on every guest command (both fixed in
  v0.1.5).

## A guest that stops answering

- **The stalled box was not reproduced under a test.** The twenty minutes of
  qemu inflating clusters, the missing reply from every `boxctl` call and the
  absent memory figure come from one real node, not from a suite. The
  uncompressed disk was measured on a Lima VM here: an 8 GiB image with 98%
  compressed clusters gave a box disk of 97.35% compressed clusters when the box
  started from it (the fallback path, `no-such-qemu-img`), and 0.00% when the
  box started from the uncompressed copy; `qemu-img check` is what counted them
  (11.1.2). Whether a box then reads its disk faster, and whether the guest was
  really out of RAM, were not measured - and a unit test does the same
  comparison with the real `qemu-img`, skipping a host that has none.
- **The uncompressed copy costs cache space per image, and only the copy is
  cleaned up.** It sits next to the download in `image_cache_dir`, and the driver
  deletes it once nothing has used it for a week -- or for an hour on a node that
  is below its disk reserve -- but the downloads, the baked records and the
  platform bundles stay: they are the cache. A copy a box disk names as its
  backing file is never deleted, and a copy that cannot be proven unused is kept
  (a disk `qemu-img` cannot read, or a node whose instances cannot be listed).
  A box disk this driver makes has no backing file at all (measured with
  `qemu-img info`), so in practice a copy only has to be old.
- **A full host disk is qemu's io-error, and nothing here resumes the VM.** When
  the filesystem a box disk lives on fills, qemu stops the VM and reports
  `io-error`: Nomad and `box ls` go on saying the box runs, and the guest simply
  stops answering. `box status`, `box ls`, `box wait`, `box events -f` and the
  error of `box run` now say what qemu reported, with the free space of the node,
  and a task event marks the transition -- but the driver does not resume the VM,
  because that is the operator's decision. Resume it by hand:

  ```
  printf '{"execute":"qmp_capabilities"}\n{"execute":"cont"}\n' |
    socat - UNIX-CONNECT:<lima_home>/<box>​/qmp.sock
  ```

  (QEMU serves one QMP client at a time, so a `socat` that hangs is Lima's
  hostagent holding the socket; the driver's own probe gives up after two
  seconds for the same reason.) Free space on the node first: a resumed VM whose
  disk is still full fails its writes and the guest's filesystem takes the error.
  Measured on a real node: after the resume the guest ran on with the same boot
  id, the same PID 1 and an unbroken uptime, and its write completed.
- **The disk reserve is a number, not a quota.** The driver keeps a node's free
  space above `disk_reserve_gib`/`disk_reserve_percent`, and `box create`,
  `box start` and a growing `box resize` are refused below it -- but a box that
  is already running writes until the filesystem is full, and nothing stops it
  before qemu does. There is no per-box disk quota and no way to reserve space
  for a box that is not running.
- **The free space the driver reports is the Lima home's, not the volume's.** A
  node whose box disks live on a filesystem of their own (a separate
  `host_volumes_dir`) is measured by its Lima home: the reserve and the free
  number are that filesystem's. A box whose volume is somewhere else can fill it
  while the node still reports room.
- **A guest that does not answer is only bounded in three commands.** `box
  status`, `box top` and `box session ls` give up after 20 seconds. `box wait`,
  `box send`, `box run` and `box exec` still wait as long as their work does, and
  an abandoned `boxctl` call keeps running in the guest until boxd answers it:
  the client-side stream ends (Nomad's exec session closes with its context), the
  guest side is boxd's own.
- **earlyoom's choice is proven, the recipe's build VM does not run it.** On a
  2 GiB Ubuntu 24.04 VM (lima 2.2.0, qemu 11.1.2) earlyoom 1.7 with these
  arguments killed a 1.5 GiB process at 10% available memory, the journal carried
  "low memory! at or below SIGTERM limits" and "sending SIGTERM to process ...",
  and the current `boxd` wrote both lines to its own log as "guest earlyoom: ...".
  `images/provision.sh` enables the unit, starts it to prove the drop-in parses,
  then stops it for the rest of the build, so the build VM never runs with a
  process killer racing its installs - and a *box's* first boot is the first time
  the enabled unit runs for real.
- **earlyoom protects names, not roles.** `--avoid` matches the process name, so
  a `boxd`, `sshd`, `systemd`, `lima-guestagent` or `qemu-ga` process is never
  killed; anything else in the guest can be, including a session's executor and
  the turn it is in. That is the point (the alternative is a guest that cannot be
  reached), but a box whose executor holds the memory a build needs loses that
  executor, not the build's compiler.
- **A node whose client still caps `kill_timeout` at 30 s powers boxes off
  hard.** The deploy kit's `nomad_client` role now writes `max_kill_timeout =
  2m`, and the job asks for 90 s; a hand-written client configuration that leaves
  Nomad's 30 s default keeps cutting `limactl stop` off, and the task event says
  so ("Graceful stop of box ... failed ... signal: killed"). A stop still has no
  test: the 90 s ask and the 2m cap are configuration, and a guest's shutdown
  takes as long as its services do.

## Platform bundles

- **Unused bundle directories stay in the cache.** A bundle is one immutable
  directory per sha256 under `<image_cache_dir>/platform/`, and nothing removes
  one: a running box may mount any of them, and a bundle is shared by every box
  on the node, so a deletion heuristic is not obviously safe. The cost is the
  size of the unpacked archive per bundle version (a few tens of MB); an
  operator who needs the space removes a directory that no `lima.yaml` of a
  running instance names. `box` has no command for it.
- **A bundle is trusted by sha256 only.** Like an image, it is pinned by digest
  through the index: no signature, no transparency log. Whoever can write the
  index decides what `boxd` and which executors run in every box.
- **The driver mounts what the task names and checks nothing else.** It never
  asks the index what the box's architecture or its image expects: a hand-written
  job can point a task at an archive for another architecture, or at one whose
  `manifest.json` names another version than the block. The mount then does not
  boot (`boxd.service` fails on an incompatible binary) and the task log says
  so; the CLI is what keeps the two in step.
- **The mount is only there for an image that declares it.** An image built for
  bundles carries `"platform": "mount"` twice (in `/etc/box/image.json` and on
  its index entry). An image without the field gets no block from the CLI, and
  a hand-written task that adds one hides the image's own `/opt/box` under the
  mount.
- **`box upgrade` restarts the box.** The bundle is a mount of the instance's
  configuration, so a new one means stop, write, start: the VM is down for a
  boot, and an in-flight session turn dies with it. Plan an upgrade like a
  resize, with the sessions idle.
- **The guest's `boxd.service` needs the mount, and nothing waits for it.** The
  unit has `RequiresMountsFor=/opt/box`, so a box that starts without the mount
  fails instead of running an image's stale `boxd` -- but the failure is only
  visible in the task log and `box status`, not in the box state.
- **The mount is 9p on a Linux node.** The executors are started from the
  mount, so a large binary is read over 9p on its first run (with fscache, once).
  It was not measured against a run from the box disk, and no bundle of the
  real executors was measured at all: the end-to-end check uses a hand-made
  bundle.

## Images

- **This project publishes no image, and never carries an image binary.** It keeps
  the recipe (`images/`) and the automation that builds, checks and uploads one;
  the image, its store and its index are the operator's, and the recipe's build
  takes the operator's site addresses (mirrors, store, token) as settings.
- **An image of your own is checked only by what you run against it.**
  `images/verify.sh` boots a built or published image and checks it end to end --
  boot, cloud-init, `boxd.service`, `boxctl info`, the recorded tool versions, the
  tmpfs credentials, a reload, no baked secret, the grown root filesystem -- and
  `guest/no-secrets.sh` plus `boxctl info` are the two things to run against an
  image that was not built from the recipe. Neither proves what an image does
  under a workload: that is what booting a box shows.
- **The recipe is one shape of image.** It pins Ubuntu 24.04 and its tool versions
  (`images/provision.sh`); an image that needs another distribution, another
  kernel or a smaller tool set is the operator's own build against the contract.
- **An image is trusted by sha256 pin only.** No signature, no transparency log:
  whoever can write the index decides what every box boots.
- **An image that is too small is found at create, not before.** Lima refuses to
  shrink a disk, so a box asking for less than the image's virtual size fails at
  create with `disk shrinking is not supported`.
- **A published version cannot be withdrawn** by any tooling here: the store's
  own permissions decide whether an operator can delete a file, and boxes hold
  the digest of what they pulled.
- **A bake stops the box, and it takes minutes.** The seal runs in a clone of the
  box's disk, so `/work` and every session are still there when the box starts
  again, and a box that was stopped stays stopped. It is not usable while the bake
  runs: the job clones the disk, boots the clone, seals it and compresses the
  result.
- **A bake needs room on the node for a second copy of the disk and for the
  image.** The clone is a full copy of the disk file, written beside it in the
  node's Lima home, plus about 1 GiB a booted clone writes before the seal trims
  it; the compression writes a compressed disk of at most about the disk's size
  into the image cache. A node that cannot hold both refuses the bake before
  anything is stopped, and the refusal says how much room there is and how much
  each needs. The two needs share one filesystem when the image cache is under the
  Lima home, and each must fit on its own when it is not.
- **The clone path is measured on qemu only.** `limactl clone` was measured with
  Lima 2.2.0 on qemu: a full copy of a stopped instance's disk, a new hostname, no
  snapshot. The same bake on a Mac's vz has not been run end to end, so run it
  there once before you rely on it.
- **A draft lives on one node.** It is a file in that node's image cache, so a
  box is created from it there: `box create --image NAME-N` pins the box to that
  node, and `--node` must name the same node or be left out. A node with no room
  for the box fails the create, so a draft on a full node waits until something
  there stops. Publishing is what makes the image available on every node.
- **Drafts are never pruned.** The image cache keeps every baked image and its
  record, and `box images` lists it until a store publishes the same image and
  architecture -- after that the published entry is the one row: nothing here
  expires an image, and a full cache is the operator's to clean. A cache entry removed by hand is a draft
  whose record stays on the node: creating a box from it fails, and the driver's
  task event names the image it could not find.
- **An arm64 box bakes an arm64 image, and a disk is not portable.** The bake job
  is placed by the node's architecture, so an image is always the architecture of
  the node that baked it, and a box created from it on another architecture is
  refused as any image of the wrong architecture is. There is no conversion
  between them.
- **A published image expires by the store's own policy, not by this tooling.**
  Nothing here deletes a published file: the publish task only writes, and the
  token in `nomad/jobs/box-store` is the credential the operator supplied for that.
  A store credential without a delete right cannot remove an image, so retiring a
  version is a retention rule in the store, or a delete with a credential that has
  the right. Until then the version stays readable, and a box created from it
  keeps working: it holds only the digest of what it pulled.

## When the connection is lost

- **A turn's state can be unknown.** When `box wait` runs out of `--timeout`
  with contact still lost it exits `5` rather than `4` or `1`: the turn may have
  finished while the box was unreachable, and it may still run. The feed's cursor
  (`box events --since`) is what answers the question once the box is back, and
  nothing in the CLI can answer it before that.
- **`box run` has no answer after a lost stream.** A command that was cut off
  cannot be asked whether it ran: it might have finished its work, left half of
  it, or not started. The command exits `5` and does not send it again; what ran
  is in the box's own log (`box logs -f`) and in `box run <id> -- ps -ef` when the
  box answers.
- **A log resumed by byte offset assumes the file only grew.** `box logs -f`
  resumes each of the task's two log files at the byte it read to. Nomad rotates
  a task log at 10 MiB, and a rotation while the connection is lost makes that
  offset point into a file that was truncated: the resumed read then starts at
  the wrong place or ends early. A follow that spans a rotation is read again
  from the start with `box logs <id>`.
- **A client away for more than ten minutes ends the box; a shorter absence does
  not.** Nomad notices a missing client after 20-30 seconds and, because the box's
  group sets `disconnect { lost_after = "10m", replace = false, reconcile =
  "keep_original" }`, marks the node `disconnected`: the allocation turns `unknown`
  and stays where it is. A client back inside those ten minutes re-attaches to the
  same allocation and the same VM. Measured on Nomad 2.0.7 with a root client: 20 s
  and 118 s away, SIGKILL and SIGTERM, and with the plugin binary replaced meanwhile;
  the qemu pid and the guest's uptime did not change. After 13 minutes away the
  allocation was lost at the ten-minute mark, the VM ran on until the client
  returned, and the client then stopped it: a new allocation started on the same
  disk seconds later with a fresh guest. A job change is a different thing. It
  replaces the allocation whether or not the client was away ("alloc is being
  updated due to job update"), so a `box upgrade` sent during an absence runs at the
  client's return. Re-registering an unchanged job does nothing.
- **A job change during an absence can fail its first start.** In the one run
  measured, the replacement allocation's `limactl start` met the host agent the
  old allocation's stop had just killed (`host agent process has exited: signal:
  killed`), Nomad marked that allocation failed and placed another one, which ran.
- **A bake or a publish has no disconnect window.** Their jobs carry no `disconnect`
  block, so by Nomad's code a client away for about 30 seconds loses that allocation
  (measured with a box job at `lost_after = 0`, not with a bake). The command reports
  a failed job, and the box's own disk is untouched.
- **A lost connection is only retried by four commands.** `box wait`,
  `box events -f`, `box logs -f` and `box run` know the difference between a box
  that is gone and a connection that is not there. Every other command reports
  what it saw: `box status` and `box top` are short and are meant to be asked
  again, and `box send` either reached the guest or did not, which its sentence
  says.
- **A box replaced by another allocation is not followed.** `box wait` asks for
  the turn by number after a loss, and the number lives in the box's session
  store, so a box whose allocation Nomad replaced under a new id — a resize, a
  re-registration — is followed for its state but the guest call has to find the
  same guest. A box that was restarted loses its running turn: that is not a lost
  connection and is reported as the turn it is.

## Sessions and executors

- **Resume works for prime only.** opencode keeps its own transcripts and the
  adapter does not pass a resume flag, so `executor_session` stays empty and an
  executor restart starts a fresh session; opencode does advertise
  `session/load`, which a later change could use.
- **The `claude` adapter was never exercised**, because its binary is absent on
  the machines the project was built on.
- **The ACP findings are for prime-agent 0.9.8.** One session per process, no
  `session/load`, no `session/cancel` and the settings-file behaviour were
  verified on 0.9.8 while the image ships a 0.10.x guest.
- **Session density is a floor, not a measurement.** About 180 MiB per session
  implies 4 to 5 sessions in a 1 GiB box; that case is not covered by a test.
- **One in-flight turn per session.** A second `send` while a turn runs answers
  `busy`. There is no queue.
- **An executor that keeps working after a failed turn is only watched, not
  fenced.** boxd sends `session/cancel` when a turn ends in an error from the
  executor, and records what it still sends (`executor_active`), and `box session
  new` and `box send` warn when another session works in the same directory. None
  of that stops the executor: an executor that ignores `session/cancel` goes on
  until it stops by itself or the session is closed. A file lock or a git index
  two writers touch is still two writers.
- **A rotation is compared by content.** boxd restarts an executor when the
  credential files' content changed, and the driver decides by the task files'
  content, so a rotation that writes the same bytes restarts nothing — and a
  rotation of a key the provider has already invalidated fails the next turn with
  the provider's own error, which is what it is.
- **A resize kills an in-flight turn.** Sessions should be idle when a box is
  resized: the turn dies with the VM.
- **A box gets no extra agent packages and no skills.** A package cannot travel
  in a 64 KiB Nomad variable, so only the chosen provider's credential and
  definition fit. A second volume, an object store or a package inside the image
  would close it.
- **A model's size comes from an addon or from the staged definition.** A box
  asks its provider for the models of its key at every session, but an
  OpenAI-compatible `/models` does not say how large a model's context is: an
  addon catalog (`docs/guest-image.md`) or the staged definition has to, and a
  model neither sizes runs with the executor's default.
- **The box's default user reads the credentials.** They are a 9p mount of the
  host's files, which carry the runtime user's uid, and the guest's default user
  has the same uid. That user has passwordless sudo, so a root-only copy would
  not keep it out either; an agent in a box can read its own executor's key.
- **`--repo` clones without credentials.** A box has no git credentials of the
  caller's, so `box prepare --repo` reaches public repositories and those the
  box's network may read anonymously. A private one goes in with `--from` (a
  checkout, or `git bundle create` and clone from it in the recipe).
- **A preparation is a full clone.** There is no shallow or partial clone: a
  repository with a long history takes as long as its pack, which is minutes on
  a slow route.
- **prime runs no extension code, so per-request headers are static.**
  prime-agent 0.10.2 packages carry skills, prompts and themes; an extension's
  code that adds a header per request never runs. An addon catalog's `headers`
  go into the provider definition instead, one value for every session of the
  box: a gateway that routes by a session header (`x-opencode-session`) sees
  one session per box, not per `box session`.
- **Nomad's graphs of a box are still empty on the cluster.** The driver
  samples the VM processes and the tests see the samples, but the cluster's
  allocation stats have none and no error says why. The task events say when
  Nomad first asked for a box's resource usage and when the first sample went
  out, which is where the next look starts.
- **A box whose executors hold a template item needs Vault on the node.** The task
  carries a `vault` block and a template that reads it, so a client without a
  `vault` block fails the allocation instead of starting the box. A variable with
  plain items only is unaffected: those jobs carry no Vault block.
- **A Vault that cannot answer delays a new box.** A template item reads Vault
  before the task starts, so the box waits, and Nomad kills it at its healthy
  deadline if Vault never answers. A box that is already running keeps the files
  it has: `change_mode = "noop"` means a token it cannot renew does not restart
  the VM.

## Lifecycle and operations

- **A box's description is a Nomad variable, and Nomad holds 64 KiB per
  variable.** The rendered job is a few KiB, so a box fits with room to spare, but
  a job with hundreds of executor templates does not: `box create` refuses such a
  job before it creates the disk, with the size and the limit, and nothing is left
  behind. `box stop` and `box start` refresh the executor files of a box without
  storing a bigger job.
- **A box made before boxes kept a description cannot survive the job GC.** Its
  job is the only record of how it was made, so once Nomad collects it the box is
  an `orphan`: `box ls` and `box status` say so, `box start` refuses it, and `box
  rm <id> --yes` deletes the disk. There is no way back for such a box, by design.
- **A dynamic host volume keeps the capacity its plugin reported at creation.**
  Nomad's register path never asks the plugin again and its state store writes the
  capacity the request carries, so `box resize --disk-gib` grows the disk in the
  driver and leaves the volume record at the old size. `box status` prints the
  disk's real size and, when the two disagree, says which number is which; nothing
  schedules on a host volume's capacity, so the stale record costs a confusing
  `nomad volume status` line, not a placement.
- **`box rm` then `box create` with the same id fails for a while.** Nomad keeps
  the deleted volume record until its garbage collection threshold ("volume name
  already exists on this node"); a new id works at once.
- **Box ids are global to a node, not per volume.** Two jobs that name one
  `box_id` on the same node share one VM, which is not validated.
- **`disk_gib` is not checked against free space.** qcow2 is sparse, so a size
  check is not obviously right, and a box can fill its node's volume filesystem.
- **Orphans are reported, never removed.** A Lima instance with no task for ten
  minutes, or without its disk, appears in the driver's fingerprint; the operator
  removes it on the node with `limactl delete`. No command of this project does it.
  A stopped box looks exactly like that to the driver, so `box capacity` answers as
  `box ls` does: a box with a job or a stored description is named with its state
  there ("no box task, and the cluster knows this box as stopped"), and only a VM
  that is neither is an orphan.
- **The VM's pid is not a driver attribute.** Nomad 2.0 consumes no
  `DriverAttributes` at all (its `TaskStatus` has none since `TaskHandle` was
  removed), so the pid an attribute would carry is invisible. It reaches a reader
  as a task event instead: "Box <id> is running (pid N)" always, and "VM process
  of box <id>: pid N" when the client sets
  `telemetry.publish_allocation_metrics`, which is also what makes the driver
  sample the VM at all. `box status <id>` prints the recent task events.
- **Nomad keeps a node attribute the driver stops sending.** It removes one only
  when the new value is empty, so the driver sends a gone box's keys empty once
  and `box capacity` reads the driver's own attributes, not the node's. Keys a
  driver before v0.1.7 left on a node stay in `nomad node status -verbose` until
  that client restarts; nothing reads them.
- **Stale allocations accumulate.** A job stopped and started often leaves
  several stopped allocations naming one box; the driver tolerates them and only
  Nomad's garbage collection removes them.
- **The plugin watchdog is a workaround.** Nomad 2.0 does not reattach a
  restarted client to a running external plugin, so the plugin stops when its
  client goes away. It can go when Nomad fixes reattachment.
- **A running macOS box keeps its task files where it mounted them.** A share
  does not follow a replaced directory, so a box running when the driver is updated
  keeps a plain directory in the temporary directory until its next start, when it
  becomes a link into the task's private directory.
- **On macOS a VM's graphs depend on finding its process.** A `vz` VM runs in a
  Virtualization.framework XPC service that no Lima pid file names, so the driver
  finds it by the box's disk among the files it holds open (`lsof`, every 30 s
  until found). Without it, Nomad shows only the hostagent. Its resident size can
  exceed the task's memory: macOS has no cgroup, and nothing enforces that limit.
- **Capacity's uncounted numbers are a convention.** `box capacity` subtracts
  the node's reservation per vCPU and reads everything from the driver's periodic fingerprint,
  so a stale fingerprint either double-counts a VM or misses it.

## Deploy

- **There is no Nomad server role.** The kit brings up clients that join servers
  you already have; server configuration, ACL bootstrap and hardening are out of
  scope.
- **Old releases are pruned by hand**: `rm -rf /opt/box/releases/<version>`.
  There is no retention policy.
- **The deploy tests do not restart a service.** They assert the rendered unit,
  the rendered plist and that `nomad config validate` accepts the configuration;
  a real restart is what `deploy/accept.sh` does on a real node.
- **A Mac's CPU has to be declared by hand.** Nomad 2.0.7 fingerprinted a 14-core
  M4 as 48 MHz in total, so a Mac with the default reservation offered no CPU and
  nothing could be placed on it. Nomad cannot read the real clock, and the kit does
  not guess: set `nomad_client_extra: {cpu_total_compute: <MHz>}` for the Mac
  (`deploy/ansible/inventory.example/group_vars/box_nodes_mac.yml` shows the shape,
  with how to pick the number).
- **The acceptance run needs a real cluster.** `deploy/accept.sh --dry-run`
  prints its ten steps without a cluster, but the run itself needs nodes, an
  image and executor credentials.
