# 0001: A Nomad driver backed by unmodified Lima

Status: prototype. No production migration or deployment.

Maintain only this Nomad driver and the separate Claude plugin. Lima, QEMU/VZ,
Ubuntu, guest systemd and Docker are upstream dependencies. Do not introduce a
REST daemon, a dispatcher, a scheduler or a custom runtime/kernel fork.
The old BoxLite driver is a read-only reference and remains installed while its
existing workloads finish. This plugin identifies as lima and has separate binary,
configuration and state paths.

Use the official Nomad DriverPlugin/gRPC SDK and exec official limactl without a
shell on the host. Treat list --json as structured runtime metadata; create, start,
shell, copy and stop are upstream commands. A private operator-owned Lima home and
durable task ledger bind allocation/task identity, instance name, owner marker,
configuration hash, guest machine/boot identity and workload result. Persist intent
before non-repeatable operations. Never recreate or re-execute on an uncertain
launch or recovery. Refuse stopping a foreign/replaced/changed instance.

The guest workload is a detached per-task process supervised by a fixed driver
bootstrap, not a new control service. Its stdout, stderr and atomic result remain
on the guest disk. A durable exclusive launch marker makes retries fail closed.
Driver shutdown stops monitoring only, not the VM/workload. Stop is explicit;
Destroy retains disks and diagnostics even with force=true. Disk purge is not
implemented. Final artifacts must be exported by the plugin before optional future
retention actions. No automatic close/idle GC is enabled.

Disable host mounts, personal SSH public keys, SSH agent/proxy forwarding, Rosetta,
and automatic port forwarding. Use a pinned operator template/image, native Linux
guest architecture and upstream VZ on Darwin or QEMU/KVM on Linux. macOS guests
remain experimental upstream and are unsupported in this first driver. Host OS
attributes and guest OS are different concepts.

Nomad selects nodes and accounts CPU MHz, RAM and group ephemeral disk. Validate
requested guest resources against actual Nomad reservations and per-core compute;
include explicit operator RAM overhead. Check actual free disk before create,
including retained disks; never impose a global hard-coded VM count. The Driver SDK
does not pass shared group EphemeralDisk to TaskConfig, so document and test the
jobspec reservation rather than inventing a task disk field.

Logs copied to Nomad are best-effort at-least-once across an abrupt crash; the
last chunk can replay because Nomad FIFO has no transactional acknowledgement.
Authoritative complete logs/results remain in the guest. Transport errors and
unknown workload state must never become successful completion. Stats must identify
what was measured, never substitute allocations for observed utilization.

Before production: real plugin SIGKILL/recovery, no duplicate workload, stop ownership,
resource release, interrupted create/launch, exec/transfer and native Nomad harness
acceptance on both platforms. CLI-exit survival alone is not crash acceptance.
Default Lima egress/host-loopback/LAN access is broader than the previous allowlist;
this driver must not claim network isolation. Solve that with a separately reviewed
upstream/host network policy, not a guest-root-editable firewall promise. No host
firewall/VPN changes are authorized by prototype tests.

## Upstream AI/sync guidance

Reviewed https://lima-vm.io/docs/examples/ai/ . Upstream shell --sync (>=2.1)
uses rsync on host and guest and prompts for accept/review. It is not assumed to
work unattended through Nomad, nor to make a remote coordinator's path exist on
the Lima host. sync-exclude/.limasyncignore are documented >=2.3; checked stable
2.2.1 must not advertise them. The initial backend uses official copy over Lima's
SSH transport with explicit paths, caller-declared files and checksums. No automatic
worktree sync, secret forwarding or acceptance of worker edits is implemented.
Coordinator-to-Nomad-host transfer remains a future Claude plugin action; the driver
never exposes the coordinator's home directory to a guest.
