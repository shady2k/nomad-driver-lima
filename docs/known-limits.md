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
- **A resize kills an in-flight turn.** Sessions should be idle when a box is
  resized: the turn dies with the VM.
- **A box gets no extra agent packages and no skills.** A package cannot travel
  in a 64 KiB Nomad variable, so only the chosen provider's credential and
  definition fit. A second volume, an object store or a package inside the image
  would close it.
- **The provider's model list is staged, not discovered live.** A model added to
  the provider later needs a new `box executors set` and a reload.
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

- **`box rm` then `box create` with the same id fails for a while.** Nomad keeps
  the deleted volume record until its garbage collection threshold ("volume name
  already exists on this node"); a new id works at once.
- **Box ids are global to a node, not per volume.** Two jobs that name one
  `box_id` on the same node share one VM, which is not validated.
- **`disk_gib` is not checked against free space.** qcow2 is sparse, so a size
  check is not obviously right, and a box can fill its node's volume filesystem.
- **Orphans are reported, never removed.** A Lima instance with no task for ten
  minutes, or without its disk, appears in the driver's fingerprint and as a line
  of `box capacity`; the operator removes it on the node with `limactl delete`.
  No command of this project does it.
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
- **Capacity's uncounted numbers are a convention.** `box capacity` subtracts
  1000 MHz per vCPU and reads everything from the driver's periodic fingerprint,
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
