# Consolidated VM management candidate 0.1.50

Source base: reviewed caller 0.1.48 1a351c97. This candidate is not installed.
Core recovery rollout remains separately owned; no production caller installation,
VM change, paid model call, private upload, driver restart or access grant is part
of this source work.

## Contract and evidence

| Capability | Candidate implementation | Remaining acceptance |
|---|---|---|
| Stable VM ID, global multi-architecture discovery | Existing .47 contract retained; capacity summary covers every node | Fresh installed-source receipts after core recovery |
| Fast list/status | Four parallel observations, total transport deadline, per-VM unknown/errors; never sessions watch | Real four-VM latency under failure/partition |
| Recent exact-request progress | Official ACP reader, at most 256 KiB, no guessed anchor or generic heartbeat as exact-request proof | Linux/Mac existing journal fixtures; public events/tail facade remains absent |
| Explicit wait | Separate bounded same-VM observation; timeout does not cancel | Real transport deadline acceptance |
| Finite shell independent of ACP | Native Nomad allocation-exec, existing rights, managed operation receipt | Linux/Mac shell runtime acceptance; interactive SSH is not provided |
| Upload | Existing S3 multipart 8 MiB, source/remote SHA+size, bounded buffers, resume offsets, atomic publication, explicit safe tar extraction | Actual exchange + both guests, interrupted transport; status/reconcile/abort implemented; incomplete extraction staging requires reconciliation |
| Zero activity / shell-only finish | Typed management checkpoint, exact revision/identity, known terminal operation/upload ledger, verified export | Both platform drivers with actual exchange and diagnostic logs |
| Destroy with disk/quota release | Not implemented; finish reports disk_deleted=false | Deliberate port of reviewed purge protocol to current drivers and typed non-model checkpoint |
| Resize | Assessment only; previous implementation stopped-only, not hot | Backend current-driver port, lineage/accounting, VM-ID plan/apply and platform tests |
| Download / general recover facade | Existing legacy verified results and core operator rebind retained | New ergonomic VM-ID commands not implemented |

## Required end-to-end gate

Request exact approval for one owned minimal test VM at a time on each platform
only after capacity permits. Preserve all user workloads and current ACLs.
Use synthetic 44/67 MiB data, not private code. For Linux then Mac: create,
list/status, shell before ACP initialization, upload/hash/extract, repeat same
intent after part/ACK loss, export and shell-only finish. Verify exact manifests,
retained disk result and no false quota-release claim. Use synthetic official ACP
runtime fixtures for history/cancel/rebind without model prompts; any real runtime
check must not submit a model request. Unknown dispatch remains lookup-only.

Destroy and resize require separate driver integration review before those checks
can be added. Old driver finalize-v1 requires a model request identity; do not
fabricate one for an exec-only export. Introduce an explicitly typed compatible
checkpoint proof, preserve source/attempt/owner binding and verified S3 manifest,
and refuse purge when the driver cannot prove support. Test stopped disk absence
and returned quota rather than accepting a stopped VM as deletion.

Resize must preserve the existing disk and current cold-recovery rules, validate
real CPU/RAM/disk capacity and Nomad reservations, and expose source/successor
lineage. No implicit stop and no hot-resize promise. Previous retained-resume
source cannot be merged wholesale over current Darwin attachment recovery.

## Known safety-preserving limits

A failed/unknown upload fences finishing. Exact-ID status/reconcile/abort do not
replay bytes. Abort removes only the recorded private unpublished inode and
retains published outputs and exchange objects. Incomplete extraction staging
still requires reconciliation; never clear receipts or delete arbitrary partials
to make finish pass. A journal suffix without a real request anchor remains unknown;
VM/process liveness does not replace exact-request evidence. A transport deadline
bounds caller observation even when guest filesystem I/O stalls. Consequently
this source milestone is not a declaration that the entire requested lifecycle is
production-ready.
