# nomad-driver-lima

Local prototype of a Nomad `lima` task driver using the **official Lima CLI**.
Lima, QEMU/VZ and guest distributions are unmodified upstream dependencies.
There is no REST server, scheduler, companion service or runtime/kernel fork.
This repository is independent of the existing BoxLite driver so running workloads
can coexist during a reviewed transition. It has not been deployed to production.

Read [the architectural decision](docs/adr/0001-upstream-lima.md).

## Implemented

- Start, wait, inspect, signal, stop, destroy-with-disk-retention and recover.
- Durable allocation/task/owner, instance config, machine and boot identity.
- Exclusive launch intent and per-task process lock; uncertain commands are not
  replayed. Recovery attaches to the same guest process rather than calling start.
- Real guest exit codes and atomic guest result; bounded stdout/stderr forwarding
  to Nomad. Complete logs remain on the guest disk. A crash can replay the last
  forwarded chunk because the Nomad log FIFO has no transactional acknowledgement.
- Official `limactl shell` non-TTY exec/streaming and `copy` transfer primitive.
- PTY exec and terminal resize through the instance-generated official SSH config;
  Lima-managed key only, no agent forwarding or personal SSH configuration.
- Native architecture Linux guests, host mounts/personal keys/agent forwarding
  disabled, operator-pinned template and exact stable Lima version.
- CPU/RAM reservation validation using Nomad agent compute, explicit guest RAM
  overhead, physical free-space guard. Group ephemeral disk belongs in the jobspec.
- Fingerprint and lifecycle events. Network isolation is explicitly **false**.

## Deliberate limitations

**Not production ready.** Stats sample the owned host VM process: RSS bytes and
CPU percent (100% = one core), with Nomad CPU ticks expressed in MHz. They do not
represent guest free memory, command-only usage, or helper process overhead.
Sampling stops on missing/changed process identity; no synthetic zero samples.
macOS guests,
workload environment/secrets injection and automatic artifact export are not yet
implemented. A transport failure stays unknown and is not successful completion.
Early ambiguous startup preserves the instance and ledger for reconciliation.
Automatic idle close, disk purge, broad file sync and automatic acceptance of edits
are not implemented. `force=true` never grants disk deletion.

Default Lima networking can reach host loopback/LAN; this prototype does not claim
the former BoxLite egress allowlist. An additional reviewed **upstream/host network
policy** is required before untrusted production workloads. No host firewall or VPN
changes are made by this driver. Linux requires an unprivileged runtime owner with
KVM access and official QEMU/OVMF. A root Nomad agent must configure an existing non-root `runtime_user`. The driver
drops UID/GID and supplementary groups only for official limactl child processes;
it never invokes sudo or creates accounts/grants. Its private Lima home must already
belong to that account and its template must be readable by it. Unprivileged drivers
can only select their own identity. This path has unit coverage; live root-agent
acceptance remains pending.

`limactl shell --sync` is upstream interactive review functionality, not a headless
remote coordinator protocol. The driver uses explicit official copy. Sync excludes
and `.limasyncignore` are documented for 2.3, not stable 2.2.1.

## Local development

The pinned Nomad SDK source revision matches the tested Nomad 2.0.7 revision
`9dcbdc5e64ecc4ec63e42b225af729c87c87cf83`; the Go module's historical pseudo-version
name must not be mistaken for the deployed Nomad version.

```sh
go test -race ./...
go build -trimpath -o bin/nomad-driver-lima ./cmd/nomad-driver-lima
```

Tests use a fake official CLI and a real Nomad SDK gRPC adapter. Real VM tests are
opt-in and require an isolated operator JSON file via `NOMAD_LIMA_LIVE_CONFIG` and a
private `NOMAD_LIMA_LIVE_ROOT`; they must never target production state. No paid model
is called. Acceptance evidence is installation-local and is not committed here.

No GitHub repository or public release has been created. No license has been chosen
in this new repository yet; that decision must precede public distribution.

## Prototype acceptance

Linux amd64/QEMU acceptance with official Lima 2.2.1:

- A SIGKILL of the driver process preserved its guest. Recover attached to the same
  machine/boot identity, command launch count stayed one, actual exit 7 and artifact
  SHA were verified. Disk retained after normal stop.
- A root test driver ran official Lima as the existing unprivileged runtime user;
  host VM RSS was nonzero, SSH PTY resize returned 37x101, stdin EOF reached
  a guest `cat`, and exec returned exit 7. A termios/early-EOF regression was found
  and fixed before this acceptance; failed attempts were stopped with disks retained.
- An isolated Nomad 2.0.7 dev-agent bound to loopback, with its own data/plugin dirs,
  discovered the healthy driver, scheduled a real KVM task, received stdout/stderr
  through native Nomad logs, observed exit 0, and destroyed the task while retaining
  the stopped VM disk. It did not join or restart production.

The long initial dev data path exceeded Nomad's optional task API Unix socket path
limit. Jobs and logs still passed, but operators must use a short data_dir before
relying on that socket. First harness attempt used the wrong executable stanza name;
no task was launched in that attempt. These are not hidden acceptance passes.

Mac VZ acceptance also passed driver SIGKILL/recovery without duplicate launch,
real exit/logs/SHA, Stats PID ownership, PTY resize/EOF and Ctrl-C (exit 130).
VM process RSS excludes separate helper processes. VZ can run the VM in the hostagent process,
so its process RSS may include that agent; macOS virtualization accounting differs.
VZ process RSS can substantially undercount guest RAM and must not be used as guest
memory usage or to replace Nomad guest-RAM reservations. No assertion of equivalent
QEMU/VZ accounting or production network isolation is made. See
[the unapplied network proposal](docs/network-policy.md).

The next integration stage is described in [one plugin adapter](docs/plugin-integration.md).

Mac VZ also passed a separate unprivileged Nomad dev-agent task, native logs, normal
finish and retained-disk cleanup. An upstream Apple Silicon CPU unit detection bug
was reproduced read-only; see [CPU accounting](docs/cpu-accounting.md). The driver
keeps the scheduler's native scale rather than silently correcting live capacity.

Metric scope is also machine-readable in fingerprint attributes:
`driver.lima.metrics.memory.scope=host_vm_process_rss`,
`driver.lima.metrics.memory.guest_usage=false`,
`driver.lima.metrics.memory.complete_vm_memory=false`, and CPU scope/basis attributes.
Task samples identify the measured host PID. Only RSS is marked measured for memory;
unmeasured standard fields must not be presented as zero guest RAM usage.

Retained disks can be resumed through a new, node-pinned allocation; see the
[resume contract and rollout boundary](docs/retained-resume.md).
