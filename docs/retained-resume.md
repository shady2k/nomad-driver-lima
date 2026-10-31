# Retained VM resume

Resume submits a **new batch job/allocation**, pinned to the source node. It keeps
one Lima instance and disk, starts a new bootstrap command with a new worker
session, and then uses the ordinary agentplugin `create` operation to establish a
new ACP session. It does not revive a completed allocation through `RecoverTask`,
copy a prior prompt, or invoke host/operator SSH. The bootstrap command and task
Vault role are operator configuration; the contract names no runtime, provider,
artifact registry or private infrastructure.

## Agent interface

```sh
python3 plugin/scripts/workerctl.py --config "$CATALOG_CONFIG" resume \
  --source-allocation "$TERMINAL_SOURCE_ALLOCATION" --session "$NEW_SESSION_UUID" \
  --creator-record "$FRESH_NATIVE_CREATOR_RECORD"
python3 plugin/scripts/workerctl.py --config "$CATALOG_CONFIG" request-status \
  --session "$NEW_SESSION_UUID"
# Once request-status returns the new target_config and a running task:
python3 plugin/scripts/workerctl.py --config "$NEW_TARGET_CONFIG" create \
  --session "$NEW_SESSION_UUID"
```

`resume` never submits a model request. A caller must explicitly submit new work
after session creation. The source must belong to the configured namespace and
owner, be terminal with desired stop, and expose a `lima vm_stopped` event with
confirmed stop/revocation annotations. Those annotations are discovery hints;
the local driver independently verifies the authoritative ledger and current VM.
Older retained disks without the new receipt/node binding are refused. There is
no fallback that adopts them or fabricates a receipt.

Resume requires a fresh active native creator record and binds its agent, native
session and generation into the new job/target config. Ending or replacing that
initiator blocks subsequent work through the ordinary initiator checks.

The new job uses `${node.unique.id} = <source-node>`, zero automatic restarts and
zero reschedules. It retains the source resource reservations and template pins.
If current topology, catalog or quota policy no longer accepts them, admission
fails; the caller does not move the disk or choose another node.

## Driver contract

New optional task fields are `resume_from_key` (64 hex task key),
`resume_generation` (expected current generation), and `resume_node_id`.
The driver requires a different allocation/task/session, the original namespace
and node, exact resources/template/platform, and a fresh task token from the new
allocation's Nomad secrets file. The configured Nomad Vault role must issue
periodic, allocation-scoped tokens through the existing Nomad/OpenBao integration;
this change does not create or modify that role. Token values never enter the
journal or events. Every token exposure intent is fingerprinted in an immutable fsynced audit before
delivery. The stopped receipt binds that complete set; reuse of any ancestor
token, missing audit or receipt/audit mismatch is refused, including after restart.
Existing role configuration is not changed by the package; a compatible
periodic-role configuration is a separate installation prerequisite.

Normal stop first seals token delivery, revokes/clears guest credentials, stops
Lima, confirms stopped status and absent runtime PIDs, then fsyncs a receipt bound
to allocation, instance, worker session and generation. An externally stopped VM,
unknown revoke, missing token fingerprint or missing receipt is insufficient.
Resume rechecks actual stopped ownership, config hash and the retained writable
disk name/device/inode/size immediately before transfer. After boot, the guest
machine ID must equal the source and boot ID must differ. The new bootstrap obtains fresh credentials through the existing
Nomad token → Bao → runtime path. Session readiness remains the ordinary runtime
and ACP readiness check, never inferred from a successful VM boot.

The original Lima ownership labels are immutable creation identity. `InstanceKey`
preserves that key while each new task has its own guest workload directory and
launch intent. The authoritative current owner is the terminal row in the durable
`ResumeNext` chain, with an incremented generation. A session UUID used anywhere in this instance
history cannot be reused, preventing A → B → A from loading an old ACP record.
Old cached handles reread the
ledger before operations; transferred sources cannot stop/destroy/purge the
successor, and stale state writes cannot erase the transfer marker. Historical transferred
rows are immutable; corrupt/unreadable rows cannot be overwritten as absent.
Exec, streaming exec and external signal hold shared generation operation leases;
transfer takes the exclusive lease. Stop seals/cancels exec contexts, waits up to
two seconds for streams to drain, then takes the exclusive lease. If an upstream
operation ignores cancellation, stop returns an explicit retryable pending error
without claiming cleanup. The internal stop signal does not reacquire the lease.

## Crash and quota behavior

Under the instance-operation, source-task and node-admission locks, the driver
writes the exact successor snapshot inside the source row **before** materializing
the successor task row. The first fsynced write fences the old owner irreversibly.
Admission and capacity reporting project that snapshot if the second write is
missing, counting the same retained disk once and reserving successor CPU/RAM/VM
capacity conservatively. Conflicting successor rows fail closed.

The report reconciler materializes a missing successor row without any runtime
effect. An exact same-allocation StartTask retry uses that same intent. A different
allocation cannot take it over. If Nomad has already failed that allocation, the
intent stays reserved for explicit operator reconciliation; this implementation
does not invent allocation resurrection or automatically boot/launch from an
orphaned intent. An uncertain workload launch keeps the ordinary no-replay rule: a persisted
launching identity without a committed start is refused on StartTask retry, rather
than being attached and reported as running. A lost VM boot response can proceed
only through fresh running/identity observation; it does not dispatch another
workload. A failed log attachment after committed launch may retry attachment.

Source and successor journals preserve `resumed_to` / `resumed_from` lineage.
Driver events carry the corresponding audit annotations. Disk quota moves with
the authoritative successor and is released only through the normal explicitly
addressed purge of that successor, never through the old source key.

## Review and rollout boundary

These changes are source-only. No deployment, service restart, live adoption, VM
creation or deletion is part of this work. Review driver and plugin together.
Do not roll an active resumed ledger back to an older driver that does not
understand `ResumeNext` fencing. A separately authorized acceptance run should
use a disposable VM, verify the same instance/disk across allocations, fresh
periodic token and ACP session, old-handle rejection, and crash reconciliation.
Acceptance targets must be explicitly selected disposable workloads.
