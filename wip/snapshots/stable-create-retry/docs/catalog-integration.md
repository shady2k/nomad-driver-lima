# Incremental prepared-disk integration (source checkpoint)

## Implemented

- Driver-owned approved catalog+SHA. Requests cannot supply an image URL. Native
  arch/backend, sealed/two-boot acceptance, pinned disk digest and minimum size are
  enforced in StartTask, alongside native CPU+RAM-overhead reservation coverage.
- Digest cache: lock, bounded transfer, no redirects, temporary file, SHA/size,
  atomic publication and revalidation. Download errors do not become cache hits.
  Local file/HTTPS artifacts are supported; private signed URLs remain external
  operator config. No cache eviction or credential lookup is performed.
- Separate fresh Lima instance from prepared disk, durable allocation mapping,
  readable purpose/project name, exact creator session metadata, duplicate guest
  machine-id rejection before workload launch. Existing legacy YAML path remains
  available only when no catalog is configured. This is not `limactl clone` of a
  permanent instance; the prepared disk is injected into fresh runtime YAML.
- Per-node operator budgets are checked under process/store admission lock. A
  retained disk still consumes disk quota. Native Nomad scheduling is unchanged.
- CLI `templates`, `capacity`, `request`, `request-status`, `propose`. Capacity is
  explicitly advisory, not reservation. Heterogeneous eligible MHz are read from
  Nomad; reservation uses the highest eligible per-vCPU value with a matching
  constraint, not a manual per-host table or default NodeID. Provider/model remain
  external. Proposals are local review documents; no catalog publication command.
- Durable create intent precedes Nomad submission. On unknown response, query same
  session/job. Never resubmit from reconnect. Config/attempt drift blocks dispatch.
- Existing exchange/multipart protocol reused in `plugin/scripts/`. Optional S3 is
  lazy. Configured failure blocks export/finish, keeps local files/VM and retries
  only transfer. Committed manifest contains terminal binding to job/session/request,
  allocation/task/job-version/restart/start-time and acpx request. Readback verifies
  full SHA and backend identity. `finish` requires that receipt and guest hashes.
- Native control-stage stdout and TaskEvents before guest launch; guest main task
  emits no-model runtime-check stages and tails safe lifecycle journal for ACP,
  request, result upload, receipt and finish. No hidden install while main waits a
  misleading ready marker. Guest main task belongs to this plugin, not a service.
- Report-only lease helper: owner/session/attempt/generation, monotonic sequence and
  duplicate-event rejection. Status/watch do not renew. Report says owner_unconfirmed,
  not proven orphan, and distinguishes active/unknown work from idle. All auto-close
  and purge flags remain false; no autonomous reaper/heartbeat is installed.

## CLI flow after coordinated artifact/activation acceptance

Use `plugin/config.catalog.example.json` as shape, with private operator values:

```sh
python3 plugin/scripts/workerctl.py --config /private/operator/config.json templates
python3 plugin/scripts/workerctl.py --config /private/operator/config.json capacity
python3 plugin/scripts/workerctl.py --config /private/operator/config.json request \
  --session SESSION_UUID --template agent-base --template-version VERSION \
  --architecture amd64 --vcpus 2 --memory-mib 2048 --disk-gib 20 --purpose tests
python3 plugin/scripts/workerctl.py --config /private/operator/config.json request-status --session SESSION_UUID
# request-status returns allocation-scoped target_config; use it for existing
# create -> submit -> watch -> collect -> result -> finish commands.
```

Task display name is purpose-based; stable JobID is `worker-SESSION_UUID`, preserving
existing S3 key binding. Driver VM name is project/purpose plus deterministic task
suffix. Full owner/session/attempt/template references are in metadata and ledger.
`guest_bootstrap_command` is operator-approved, not an arbitrary agent command.
Only `{session_id}` is replaced as an argv element; no host shell interpolation.

S3 config is optional and must be explicitly `storage: {backend: s3, reader_config:
/path/approved-reader.json, writer_config: /path/approved-trusted-writer.json}`.
These are existing exchange config contracts. Writer config is ONLY on an approved
trusted upload host/coordinator identity. Workers never receive signing keys.
**No new writer access has been granted**: if this identity has only reader rights,
S3 upload remains blocked until the existing trusted upload placement is coordinated
or an exact new grant is separately approved. No fallback, shared key borrowing,
Bao operator token delivery or new upload service is implied. Catalog artifact reads
likewise use already-authorized read URLs/files, not new S3/Bao access.

## Remaining gates (not claimed deployed)

1. Real portable boot artifact/sealing acceptance and immutable metadata from image
   worker. Add main-task/adapter assets before final image digest; no per-start pip.
2. Coordinated Linux Ansible activation only by designated deployment executor.
   This task has not changed any running node, restart policy or credentials.
3. Per-VM auth bootstrap/provider settings require exact authorized delivery.
   CLI inventory needs existing Nomad node read ACL; errors are explicit, not bypassed.
4. Artifact/data/retained-disk reservation must be tied to real deployed storage
   budget. Current driver max-total-disk + physical floor are fail-safe admission,
   not kernel filesystem quotas or native allocation-dir disk enforcement.
5. Full native stdout/stderr and TaskEvents snapshot/export is implemented with
   continuity/receipt checks; upload byte-level progress is not yet emitted. Native
   stages require the frozen new main-task/adapter assets in the accepted image.
6. Report-only lease is local plugin state. Native Claude hooks and driver timer
   reconciliation are implemented, but authenticated owner liveness and authoritative
   cross-coordinator renewal remain unavailable. owner_unconfirmed is deliberate;
   no reliable auto-reclamation claim. Backwards clock/unknown outcomes retain.
7. Receipt guarantees apply to managed plugin finish. Explicit operator force-stop,
   OS/node loss and secret-bearing raw worker output remain documented boundaries.
8. Mac compute reporting/network isolation/live rollout and real Prime/provider
   model acceptance remain separate gates; no paid calls were made here.

## Verification

Targeted Go catalog/driver/Lima tests and race mode; Python offline unit fixtures
for lost create response, changed catalog, forged/replayed lease/stale generation,
S3-off/no secret-reader import, upload failure retention, backend/attempt mismatch.
Real acpx + mock ACP / network-none bounded container proves watcher reconnect,
no prompt replay, owner-death failure, manifest-before-finish and SHA after stop.
No real VM/image export/provider acceptance is inferred from those fixtures.

## Diagnostic/lease completion checkpoint

Full diagnostic export is now mandatory on catalog-created targets and by default
when S3 is configured. The capture includes all retained native stdout/stderr from
file zero with exact Nomad END-offset semantics, contiguous rotation checks and
retained TaskEvents, redacted before storage. Capture has a recorded cutoff;
post-commit receipt/close events cannot be inside their own earlier content hash.
Missing logs/ACL/history/quota fail closed and preserve the VM. Final S3 binding
covers each diagnostic file/index, final result list and exact allocation attempt.
Missing local result cache can be restored from that receipt after VM stop.

Native hooks are source-only Claude hooks, not a live activated integration.
`--creator-record` distinguishes actual originating session ID from worker session
UUID and carries generation into the allocation binding. Hooks never invoke Nomad,
read transcripts, copy credentials, or run a heartbeat. New instance/resume rotates
generation, compaction preserves it, ended/old-generation records block new submit.
No authenticated original-agent liveness is available with same-user local hook
records: owner remains unconfirmed. Explicit cross-coordinator handoff remains a
separate integration gate. Missing/stale hooks do not cause automatic destruction.

Driver report-only reconciliation covers attached and unattached ledger entries;
InspectTask/TaskEvents and private reports expose active/unknown/idle versus owner
absence. Polling does not renew observations, replayed activity cannot renew the
same observed request, and no report branch calls stop/destroy or frees reservations.
Auto close/purge are still disabled. The timer is part of the existing driver.

Catalog mode now requires a positive operator `max_total_disk_gib` logical budget,
so sparse disk growth cannot bypass admission merely because free-space looks good.
Missing budget leaves task admission pending/unhealthy, not a fatal plugin-loader
startup. All live budgets remain unchanged; activation must supply a reviewed value.
