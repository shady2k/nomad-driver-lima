# Known limits

What is missing, fragile or unproven. Each item says what it means for you and,
where one is known, what would close it. Nothing here is a promise to fix it.

## Platform coverage

- **The measurements come from one host.** lima 2.2.0, qemu 11.1.2, Nomad 2.0.7,
  systemd 262 with cgroup v2 and `/dev/kvm` on one Linux machine. A different lima
  or qemu version can change the process model, the pid files or the guest agent's
  behaviour. Only the Linux/qemu path is measured end to end.
- **macOS and Apple Virtualization are rendered, not run.** The launchd plist,
  the arm64 configuration and the `vz` block are validated as files on Linux.
  `launchctl bootstrap` in the system domain, a `kickstart -k` that leaves a box
  alive, `plutil -lint` and `vz` itself have never been exercised. Closing it
  needs a Mac.
- **arm64 has no end-to-end test.** `box-driver`, `box-volume` and `boxd` cross-build for `linux/arm64` in
  every release, but no driver or CLI suite has run a box on arm64.
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
- **Credentials at rest are in the Nomad variable**, stored in the cluster's
  state. This project does not encrypt it; mTLS and ACLs are the protection.
- **ACLs were measured on a cluster without them.** The workload policy is
  applied per box and the operator policy is tested against Nomad's ACL engine,
  but the whole path (a box job reading the variable under real ACLs) was not run
  end to end. A box created after `acl.yml` needs its workload policy attached
  once (`deploy/acl/apply.sh workloads <id>`); until then its executors report
  "not configured".

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

## Lifecycle and operations

- **`box rm` then `box create` with the same id fails for a while.** Nomad keeps
  the deleted volume record until its garbage collection threshold ("volume name
  already exists on this node"); a new id works at once.
- **Box ids are global to a node, not per volume.** Two jobs that name one
  `box_id` on the same node share one VM, which is not validated.
- **`disk_gib` is not checked against free space.** qcow2 is sparse, so a size
  check is not obviously right, and a box can fill its node's volume filesystem.
- **Orphans are reported, never removed.** A Lima instance with no task appears
  in the driver's fingerprint and in `box capacity`; how an operator dismisses it
  is still an open question.
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
- **The acceptance run needs a real cluster.** `deploy/accept.sh --dry-run`
  prints its ten steps without a cluster, but the run itself needs nodes, an
  image and executor credentials.
