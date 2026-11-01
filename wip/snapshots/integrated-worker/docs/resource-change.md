# Stopped resource changes: disk, vCPU and RAM

This is a separate source candidate based on the reviewed resume handoff. The
resume branch/release remains unchanged. No live VM was resized, stopped, started,
adopted or replaced by this task. Rollout and live acceptance are not authorized
by these source tests, and the candidate is not production-ready.

## Verified native support and interruption boundary

The version reviewed is **Lima 2.2.1**, built-in QEMU or VZ, Linux guest. Its
[`edit` command](https://github.com/lima-vm/lima/blob/v2.2.1/cmd/limactl/edit.go)
rejects a running instance. That applies to CPU, memory and primary disk changes.
There is no online/hotplug promise here, including no direct QMP/HMP bypass.

| Resource | Minimal native path | Reduction |
| --- | --- | --- |
| vCPU | Edit stopped VM configuration; new boot applies CPU topology | Permitted after stop, subject to native/guest/operator limits |
| RAM | Edit stopped VM configuration; new boot applies memory | Permitted after stop, subject to native/guest/operator limits |
| Primary virtual disk | Edit configured total GiB; ordinary next start resizes image | **Never shrink** |
| Root partition/filesystem | Guest image's native boot/cloud-init growth, then independent read-only verification | No shrink, formatting or generic repair |

Lima's common [`Prepare`/`prepareDisk`](https://github.com/lima-vm/lima/blob/v2.2.1/pkg/instance/start.go)
handles primary image growth at start, after the selected driver creates/ensures
its disk. Editing configuration alone is not physical disk growth. The
[image utility](https://github.com/lima-vm/lima/blob/v2.2.1/pkg/imgutil/proxyimgutil/proxyimgutil.go)
uses QEMU image operations or its native fallback. `limactl disk resize` targets
managed **additional disks**, not this primary-instance disk contract.

For CPU/RAM, [QEMU builds startup arguments](https://github.com/lima-vm/lima/blob/v2.2.1/pkg/driver/qemu/qemu.go)
and [VZ constructs its VM configuration](https://github.com/lima-vm/lima/blob/v2.2.1/pkg/driver/vz/vm_darwin.go)
from the desired values. Host/hypervisor-specific ceilings remain upstream
constraints; the plugin does not invent a universal maximum or claim hotplug.

Lima's [cloud-init user data](https://github.com/lima-vm/lima/blob/v2.2.1/pkg/cidata/cidata.TEMPLATE.d/user-data)
requests automatic root partition growth. Whether a particular prepared image
actually grows its root filesystem is a separate acceptance question. The driver
runs **no** growpart, resize2fs, xfs_growfs, partitioning, formatting, mount change,
qemu-img or truncate commands itself.

## Agent interface

Request absolute desired resources, not additive increments. A reason, fresh
worker session and active native creator record are required:

```sh
nomad-workers change-resources \
  --source-allocation "$STOPPED_ALLOCATION" --session "$NEW_SESSION_UUID" \
  --creator-record "$FRESH_CREATOR_RECORD" --reason 'build stage needs more RAM' \
  --vcpus 4 --memory-mib 8192 --disk-total-gib 40

nomad-workers resize-disk \
  --source-allocation "$STOPPED_ALLOCATION" --session "$NEW_SESSION_UUID" \
  --creator-record "$FRESH_CREATOR_RECORD" --reason 'dependency workspace growth' \
  --target-total-gib 40
```

Omitted dimensions retain their current totals. CPU/RAM can decrease; disk cannot.
A no-op target is rejected before a new allocation. Repeating the same request
session after a lost submission response is lookup only. A changed desired target
or reason under that UUID is rejected. No prompt is submitted automatically.

The source must already be terminal/stopped with the exact driver stop/revoke
receipt. An active source is refused, never interrupted implicitly. Before asking
to finish/stop active work, export/checkpoint files/results, state the required
interruption and obtain authorization for that concrete operation. Retain unknown
model/exec outcomes. Existing source ownership and data remain intact while the
owner decides. Changing resources is not approval for a live stop or rollout.

The caller publishes current/desired/reason and a capacity preview, checks the
original node and its capability attribute, and submits one new batch allocation
pinned there. The job additionally requires `driver.lima.resource_change.stopped`.
The feature is advertised only for reviewed Lima 2.2.1. Older drivers cannot be
silently selected. Existing template/image pins stay immutable; this changes no
catalog minimum, global quota or infrastructure address.

## Allocation, ledger and physical layers

The new job's native reservation covers **desired** vCPU × current node compute
per core and desired guest MiB + VM overhead. The driver independently verifies
that reservation and desired state before editing hardware. Nomad ephemeral disk
reservation is not the Lima root disk and is not rewritten to impersonate one.

Under the existing generation/source/node admission locks, the successor intent
reserves desired CPU/RAM and the whole desired retained disk once. Admission adds
only the disk delta to existing retained disk usage, checks per-guest/node operator
limits, and checks actual free space on the disk's backing filesystem for delta
plus the configured floor. Free-space checks are checkpoints, not preallocation
or a guarantee against unrelated later host writes. Source and pending successor
are not double counted. Unknown outcomes retain the desired reservation.

Only after durable owner transfer does the new generation call native
`limactl edit --tty=false`, with exact integer CPU/MiB/GiB values and **no --start**.
Memory uses the native `--set` expression in integer MiB to avoid fractional-GiB
rounding. A semantic hash covers every non-resource setting. Changed ownership,
mounts, image or any unrelated setting blocks completion. An fsynced config audit
binds source/desired values, reason, generation identity and resulting config hash.
Lost edit replies reconcile by fresh endpoint observations; unchanged old config
can retry the exact edit, desired config can finish its audit without editing
again, and a third/partial/unreadable config remains unknown. No quota rollback or
shrink is used as compensation. Missing/corrupt audit blocks admission.

Ordinary Lima start then grows the image. After same-machine/new-boot verification,
read-only `findmnt`, `lsblk`, `/proc/meminfo`, CPU count and statvfs observations
separate these layers:

- Lima configured CPU/RAM/disk and native Nomad reservation.
- Guest CPU count and RAM observation (MemTotal excludes kernel reservations).
- Guest block-device total bytes and root partition bytes.
- Root filesystem bytes, type and unchanged filesystem UUID.
- Free host filesystem space, which differs from sparse image virtual capacity.

Disk growth currently requires a previously observed direct root partition on one
disk, ext4 or XFS, a stable filesystem UUID and working read-only utilities.
LVM, encryption, overlay, btrfs/subvolume, unsupported layouts/tools, additional
disks and macOS/Windows guests are outside the minimal contract. Existing receipts
without the new root observation cannot authorize disk growth; CPU/RAM changes do
not require root growth. Native VZ ASIF and other nonstandard disk layouts have no
live acceptance claim; use the approved ordinary Linux disk layout only.

Before credential delivery or workload launch, observed disk total must equal the
requested total, partition growth must cover delta minus at most 16 MiB alignment,
and filesystem growth must cover at least 90% of delta while preserving type/UUID.
These conservative acceptance thresholds can reject an otherwise valid unusual
layout; they never silently approve an unchanged rootfs. CPU count must match and
MemTotal must be within the stated desired-memory observation window. A bounded
30-second read-only wait permits native boot growth; no repair command follows.

A failed pre-credential resource boot is stopped only under the new generation's
ownership/config proof, with a bounded cleanup attempt. This is cleanup of the
new failed attempt, not interruption of an active source. If stop cannot be
confirmed, the journal records cleanup pending and desired reservations remain.
Power/process loss can still leave an unknown attempt requiring explicit operator
reconciliation; the feature never reports hardware/rootfs success from YAML alone.
Post-launch unknown outcomes retain the resume no-replay rules. Exact same-task
retry can reconcile a pending transaction; a different allocation cannot steal it.
Do not resurrect a completed allocation through RecoverTask.

## Handoff and acceptance gate

Source tests cover growth, CPU/RAM reduction, exact odd-MiB arguments, actual
reservation mismatch, quota/free-space rejection, immutable transaction/audit
loss, lost edit reply, crash after audit before state commit, wrong hardware/rootfs
or filesystem identity, unrelated config changes, same data-file retention,
source-handle fencing and retry without duplicate edit/boot/workload. Guest
observation fixtures run the actual embedded Python with read-only mock devices;
no live VM resize is implied.

Before deployment: reconcile with core profile work, review this separate branch,
confirm current user VM disposition and obtain exact rollout/interruption
approval. Preserve the resume ledger compatibility and prior guest-manifest gate. Do not
run an older resume-only driver against resource-change ledgers or roll their
state back to make old binaries appear compatible.
Use a separately approved disposable VM and verify QEMU and VZ independently:
marker/checkpoint hashes before/after, CPU/RAM hardware and native reservations,
virtual disk/partition/filesystem layers, token/ACP fresh identity, quota once,
old-handle rejection, pending-outcome recovery, and confirmed failed-boot cleanup.
No production readiness claim is made until those live results exist.

Independent read-only review of this extension found and fixed three additional
boundaries: native recovery now requires committed launch/config/hardware proof;
a verified-but-pre-credential failure uses an irreversible credential-delivery
journal boundary and safely stops only the new attempt; caller request limits
are enforced before submission. The reviewer independently reproduced both
recovery/cleanup failures, then verified their fixes, focused race tests and the
resource-change caller fixtures. No blocking source findings remained. This is
source review, not certification of live hypervisor, filesystem or token behavior.

Final local verification: `go test ./...` passed; the focused Go race suite for
resource changes and resume/fencing passed; all 188 plugin Python tests passed.
The source audit used the official v2.2.1 archive (SHA256
`d551efb52115ba006c1052d5c929e4d3afac363c78c9cfca6975af1f85c1426a`).
No upstream binary was installed and no hypervisor action was run against a live VM.
