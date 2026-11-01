# Nomad workers: one Claude plugin, native Lima adapter

Installation is operator-controlled through `install/caller.yml` and a frozen
local marketplace release. It does not modify an already loaded legacy plugin. Native Nomad
and official Lima/Prime/acpx are dependencies. No custom bridge or service is added.

This narrow CLI attaches to an operator-prepared **existing Lima allocation**. VM
provisioning and guest dependency installation are separate deployment work. The
allocation must have `Job.Meta.nomad_workers_owner` equal to the configured owner
UUID and a task using `driver = "lima"`. One adapter session is bound per allocation.

Coordinator: Python 3, native `nomad` CLI, private local receipt directory.
Guest: Node 24 with `process.execve`, official acpx **0.21.0**, official Prime daemon,
and these two read-only plugin assets: `guest/adapter.mjs`, `guest/launcher.mjs`.
Paths, namespace, job/task/allocation, provider/model/output policy and version
references belong in external operator config. No infrastructure endpoint, key or
project repository is baked in. No credential provider is read by this adapter.

## Shared resources and stage sizing

Available capacity belongs to all agents; it is not a per-session allocation.
For DNS/connectivity/tool inventory, start with a small compatible guest:
**2 vCPU, 4096 MiB guest RAM, max(8 GiB, template minimum disk)**. The newer 8 GiB base and older 20 GiB v3 base keep their own catalog minima; never request 8 GiB for a 20 GiB image. Use less when
sufficient. These are stage recommendations, not hard limits: justify larger
requests from real compilation/test/E2E requirements and account for concurrent
agents. Guest RAM excludes the separately reserved VM overhead.

Reuse a suitable owned session and finish it promptly after verified result export.
Hot resize is not supported by this interface. To change size, checkpoint/export,
finish, then create a new correctly sized session; preserve unknown outcomes and
failed exports. Retained disks continue consuming quota until separately approved
supported purge. This guidance changes no quotas or running VM resources.

## Flow

```sh
python3 plugin/scripts/workerctl.py --config /private/operator/lima.json create --session SESSION_UUID
python3 plugin/scripts/workerctl.py --config /private/operator/lima.json submit --session SESSION_UUID --request REQUEST_UUID --prompt-file -
python3 plugin/scripts/workerctl.py --config /private/operator/lima.json watch --session SESSION_UUID --request REQUEST_UUID
python3 plugin/scripts/workerctl.py --config /private/operator/lima.json collect --session SESSION_UUID --request REQUEST_UUID result.txt report.json
python3 plugin/scripts/workerctl.py --config /private/operator/lima.json result --session SESSION_UUID --request REQUEST_UUID
python3 plugin/scripts/workerctl.py --config /private/operator/lima.json finish --session SESSION_UUID --request REQUEST_UUID
```

Prompt text travels on stdin, not command arguments or logs. Durable submit intent
is written before native Nomad exec. Repeating the same request UUID returns its
receipt rather than sending again; changing its text is rejected. A lost response
is unknown, never permission to retry. Watch resumes from the last saved upstream
cursor and never submits a prompt. Cursor replay is at least once; it is not replay
of model/tool execution.

Only initial explicit create and first submit issue a one-use agent-launch permit.
The launcher atomically consumes it, then execs the official agent without parsing
ACP or proxying stdio. Initial empty ACP setup may be replaced by acpx's first
queue-owner connection before any user prompt. Thereafter no new agent launch is
authorized. Later turns use public `createSharedAcpRuntime` with same-session-only
semantics. Owner death or a reconnect race fails closed before a new Prime launch.
This is a version-pinned bootstrap contract; upgrades require fixture acceptance.

The official acpx owner stays alive with TTL 0 inside the guest. Its `alive` status
means owner health, not model progress. Watch uses turn events/results. Explicit
owner loss retains local receipts and guest files; unknown turns prevent automatic
finish. There is no automatic session recreation, prompt retry, idle close or purge.

Only explicit bounded relative artifact paths are fetched. Hidden/credential-like
paths and symlink escapes are rejected. Size and SHA are verified; files and an
atomic manifest are committed on the **coordinator local disk**, not S3 and not an
independent backup. `finish` rechecks the manifest, current guest file hashes,
allocation ownership and other active allocations before native job stop. `result`
continues to verify local files after stop. No model or worker code runs on the
coordinator. S3 is not required or accessed by this path.

## External config contract

See `config.example.json` for shape only. `guest.runtime_contract` records the
operator's Prime/provider model capability declaration. The official agent
must already be configured accordingly outside the plugin; the prototype reports
`effective_provider_config_verified=false` and does not pretend to enforce that
budget. It never copies provider keys. `guest.model` optionally selects an advertised
model through acpx. A real Prime/provider canary is still required before use.

## Acceptance and limits

```sh
python3 -m unittest discover -s plugin/tests -p 'test_*.py'
LIMA_ADAPTER_FIXTURE_IMAGE=EXISTING_LOCAL_WORKER_IMAGE python3 plugin/tests/offline_acceptance.py
```

The opt-in fixture uses real acpx with a mock ACP server, and mocks Nomad exec/status/
stop via its own network-none Docker container. It proves transport orchestration,
not live Prime/Lima/provider acceptance. No paid call or new VM is involved. It
covers disconnect/cursor reconnect without duplicate prompt, a strict subsequent
turn, owner death/no resubmit, sealed launch gate, path rejection, manifest-before-
finish and verified result retrieval after mock stop.

Production rollout, host network rules, credentials and paid canaries are outside
this staging change. Existing real driver KVM/VZ and native Nomad dev-agent tests
are recorded separately; they are not relabeled as this adapter's live acceptance.

## Prepared catalog path and source changes after the initial staging slice

See [catalog integration](../docs/catalog-integration.md) for dynamic
`templates/capacity/request/request-status/propose` and the operator config example.
The existing allocation adapter above remains the legacy path; its earlier
"S3 unused" statement describes that original slice, not the new optional backend.
Configured S3 now reuses the existing exchange/multipart commit protocol. A trusted
writer must already be approved/configured; source support grants no access.

Catalog-created targets require diagnostic export: full native stdout/stderr from
file zero, offset/rotation continuity, retained TaskEvents, safe redaction and an
immutable capture index bound to allocation/attempt. All snapshot files are included
in the same receipt as results. It is complete through the recorded capture cutoff;
receipt/close messages emitted after that cutoff remain in native Nomad logs, not
retroactively inside an earlier content-addressed receipt. Missing history, quota,
ACL failure or a partial stream blocks finish. No tail-only success claim.

Read-logs uses the official `/v1/client/fs/logs/ALLOC` API via native `nomad operator
api`, not arbitrary guest files or broader read-fs access. If existing ACL denies
this read, report the exact blocker; do not create a token or expand policy.
The reused generic redactor handles credential labels, JWTs and URL query tokens;
it is not an exhaustive detector for arbitrary unlabelled secrets printed by user
code. Plugin-generated lifecycle records never contain prompt/config/key payloads.

`hooks/hooks.json` uses native Claude SessionStart/SessionEnd and Bash PreToolUse
observations. It writes only native session ID, random instance/generation and event
times to private plugin state; no transcript/tool/prompt is persisted or read. Pass
its path as `request --creator-record PATH`. Startup/resume changes generation;
compaction preserves it. An old generation cannot submit new work. Watch/readback
still recover an existing request without prompt retry. Hooks never renew remote
VM leases in the background or stop anything. They are observations only: same-user
record tampering and a stale SessionEnd race cannot be authenticated away, so owner
remains unconfirmed. Hooks have been fixture-tested, not activated in a live Claude.

Driver reconciliation is report-only: attached lifecycle state appears in native
InspectTask/TaskEvents; an existing driver timer checks unattached owned VM ledger
entries and writes private `state_dir/reports/` evidence without stop/delete. Polling
and process health do not renew owner observations. Active/unknown work excludes
idle; owner absence remains separately visible. Replayed identical request activity
does not extend its observation. Native quota attributes now expose configured
node policy; zero means no additional driver cap, never unlimited physical capacity.


## Native caller installation

`install/caller.yml` installs a pinned local marketplace using the existing native
Claude CLI. Installation config (endpoints, approved catalog digest and existing
auth file references) stays outside this repository. No token is copied, granted
or printed. `nomad-workers-native` uses the existing caller identity only inside
the native Nomad process; it does not inherit arbitrary provider credentials or
TLS-bypass settings. `nomad-workers` selects the operator catalog config by default.
For an allocation returned by request-status, supply `--config TARGET_CONFIG`.
Native SessionStart hooks attach the originating session record in a **new** Claude
session; installation does not reload existing sessions or stop any VM. The old
cache stays available for existing workloads. S3 writer must be separately verified;
missing writer binding is a finish blocker, never a silent local-storage fallback.

## Managed guest operations

`operation-start/status/logs/wait/cancel` use native owned allocation exec; a finite
per-operation process runs inside the guest. There is no control daemon/service.
Each private journal is bound to allocation attempt, originating session/generation,
machine and boot identity. Durable start intent plus an exclusive guest journal
prevents duplicate launch after lost responses or concurrent requests. Status and
logs reconnect to the existing ID. No operation is resubmitted automatically.

The operator's separate `operations.json` (see `config.operations.example.json`)
sets foreground/default/maximum deadlines, heartbeat and log quotas. This avoids
changing immutable existing VM config bindings. Example defaults are foreground
60s, managed 1800s, maximum 7200s; these are operator policy, not Nomad/Lima limits.
`operation-wait --wait-seconds 30` is a bounded read, not a work deadline. Only an
explicit own-operation cancel or its deadline signals its owned process group.
Terminal exit code and redacted output are fsynced; failed/partial work is reported.
No argv or general environment is saved in the journal or native logs.

A start acknowledgement is not completion. `launching/running` stays active;
unknown or changed ownership/boot never becomes success and cannot be blindly
retried. Running/unknown or output-truncated operations block finish. Export checks
terminal log SHA/size, includes state/output in existing diagnostic/S3 receipts,
and preserves the VM if collection or durability fails.

Current v3 guests already consume compatible named operation stages, ID, elapsed
and byte counts. Schema 2 in `guest/task-main.py` additionally admits explicitly
bounded/redacted operation output and exit metadata. This source file is for a NEW
versioned image: caller installation does not replace any running guest asset.
Native v3 output remains available with `operation-logs`; do not claim full text is
mirrored by an old consumer. A newly updated caller can use managed operations in
an existing VM with its still-valid owner/generation, without restarting Prime.
Never signal or adopt an earlier unmanaged background installation automatically.


## Preparation before project work

Phase 1 prepares a minimum-sized compatible environment: dependencies, bounded
network/tool checks, versions and reproducible recipe. It includes one synthetic
model request/response smoke using an appropriate approved catalog model, with no
sensitive project data and no retry after an unknown result. The plugin does not
require a dollar amount or separate monetary approval; gateway billing/quotas stay
unchanged. Coordinator and RLM worker models may differ.
Nomad running, Prime started and ACP ready are runtime readiness, not project-task
success or proof of a model response. Missing configured model access is reported as a blocker.

Include a reusable-environment assessment and candidate project-purpose template
in the initial Phase 1 plan. After successful checks, the environment-ready report
must explicitly offer a versioned dependency-only draft (for example `example-project-dev`)
using `prepare-template` / `template-draft`, or identify an existing accepted exact
version to reuse. If draft preparation is already requested, create its metadata
packet from verified artifacts and show the SHA; no repeated permission question
for that same step. Then stop before project work. Blocked checks mean incomplete
evidence, not a ready template. No automatic source-VM stop, disk capture/sealing or
catalog publication; those remain separately confirmed concrete operations.
Actual implementation/build/project tests belong to Phase 2 after explicit task
transition/acceptance. Template proposals are separate from operator-approved
catalog publication. Retain the prepared VM/data while the owner decides; this
boundary does not authorize automatic stop, purge or publication.

Initiator records live outside uninstall-managed plugin data, with immutable
generation history. `SessionStart(source=resume)` still produces a new observed
generation; an upgrade is not an implicit takeover or repair of a missing owner
record. Never synthesize hook events. A deliberately retired environment is followed
by a fresh worker session, not automatic recovery of its old binding.

Checkpoint exports and final closing receipts are separate. New work after a
verified intermediate export is allowed after user authorization and invalidates
that export as a closing candidate. Fresh snapshots can reuse a terminal model
request without a new model call. `finish --snapshot` fences new work durably before
closing and never accepts a stale revision. See `docs/checkpoints.md` in the driver
repository. This correction remains source-only until coordinated distribution.

## Model catalog and selection

See docs/model-selection.md in the source repository. Source0.1.9 exposes one
gateway catalog, exact coordinator selection and independent native RLM worker
selectors. It needs a new versioned guest image with tools.prime_model_catalog=1;
no caller installation can hotpatch a v4 guest. No mandatory monetary question.


## VM-ID interface (0.1.40 candidate)

Ordinary commands use one stable worker UUID returned as `vm_id`:

```sh
nomad-workers templates
nomad-workers capacity
nomad-workers vm create --template TEMPLATE --template-version VERSION --task-title TITLE --vcpus 2 --memory-mib 2048 --disk-gib 8
nomad-workers vm connect VM_ID
nomad-workers vm send VM_ID --text 'Task text'
nomad-workers vm reconnect VM_ID
nomad-workers vm history VM_ID
nomad-workers vm status VM_ID
nomad-workers vm list --mine
nomad-workers vm finish VM_ID path/to/result
```

The installed wrapper passes global discovery separately from immutable bound
configs. Templates, capacity, models and new requests see approved Linux and ARM
sources without changing a running VM's binding. Architecture defaults to auto;
`vm create --model EXACT_ID` selects an approved coordinator model when needed.

Native SessionStart exports a private creator-record reference. PreToolUse uses
native `updatedInput` to prefix each Bash invocation with its own immutable
context reference; it returns no permission decision. Command input is forwarded
without being logged or persisted by the hook. Native tool IDs distinguish
parallel calls; a shared last-tool ID is never used. Repeated create/send under
one call uses the original intent and never replays an unknown dispatch. Use one
create/send per Bash tool call. Old sessions can continue using the legacy CLI;
missing native context blocks new facade dispatch rather than guessing identity.
This uses the documented [native hook protocol](https://code.claude.com/docs/en/hooks).
If another hook overwrites the input/context, the facade refuses dispatch.

List queries Nomad and verifies the exact configured owner plus current native
initiator generation and Lima worker metadata. Local cache absence does not hide
a VM, but cannot authorize adoption or reconstruct missing provenance. History
pages/cursors and request IDs stay internal; `--request` is optional for selecting
one historical task. Multiple current allocations/tasks fail closed. Reconnect
observes an existing task and never sends another prompt.

**Current finish stops and retains; it does not destroy.** After verified export,
it reports `workload_stop_confirmed` and `disk_deleted=false`. Disk quota is not
claimed released. Failed export/unknown activity keeps the VM; repeating finish
reuses its durable export IDs. Integrating the separately reviewed finish-v1
driver protocol is a follow-up prerequisite for a truthful destroy operation.
No new ACLs, driver changes or automatic disk deletion are bundled here.
Legacy command syntax remains available.


### Observing a long model turn

Read status and visible history on the same VM before repeating long waits.
Identify the last observed event/tool state and history completeness, then
reattach with `vm reconnect VM_ID`. A running VM or quiet stream is insufficient
to establish progress or a lost task. Report uncertainty precisely.

Reader failures such as `TRANSCRIPT_SCAN_BOUND_EXCEEDED` do not prove that the
turn failed. Preserve its binding and dispatch receipt; repair observability
without resubmitting, cancelling or restarting work. Caller 0.1.41 reads bounded journal pages with resumable scan checkpoints; follow
`has_more` even when a page contains no requested events yet. Completeness and
retention gaps remain explicit; an expired checkpoint requires restarting the
read, never the task.

### Exact model-turn cancellation

`vm cancel VM_ID` targets the known owned current task; `vm cancel-status VM_ID`
observes without re-dispatch. Multiple unfinished tasks require an explicit
request selection. ACK is not terminal: reconnect/history must observe the actual
result. Unknown transport outcomes are durable and lookup-only. No new model
request, runtime restart, secret grant or guest-image update is involved. See
`docs/request-cancel.md` for the legacy exact-request interface and safety limits.


## Same-ID creation reconciliation

`vm status VM_ID` is a read-only lookup of the saved create intent and Nomad job.
`vm retry VM_ID` reconciles that same ID with its frozen payload; create-only
`nomad job run -check-index=0` prevents overwriting an existing job. An existing
job resolves by exact job/owner/payload readback. After dispatch intent, retries
are lookup-only even on 404: the job may have been accepted and garbage-collected. No model prompt is replayed.
`vm create --vm-id VM_ID` plus identical original flags supports old facade
receipts for payload checking; changed flags conflict. Missing legacy proof of
non-dispatch does not authorize registration, even with the original flags. Never allocate a
new ID to bypass an unknown result. A network partition can still temporarily
prevent authoritative observation.

Validation errors are reported before registration, including the required
purpose/project format: `[a-z0-9][a-z0-9-]{0,31}`. Input is never silently corrected.
After a definitive rejected pre-dispatch attempt, an explicitly corrected new
request is allowed; the old ID retains its original payload hash. Legacy
allocation receipts without a frozen job are lookup-only for safe recovery.

Global capacity keeps its legacy fields and adds `effective_available`,
`physical_disk`, observed timestamps and units. The physical availability metric
is filesystem space, not RAM. Shared caches are included in physical usage;
their independent sizes are unknown. Retained VM disks also consume quota.
Unknown values stay null. Eligibility/maintenance reasons and per-node
architecture remain explicit; resources from different nodes are not summed.
Capacity is a snapshot, not a reservation. Registration status exposes available
Nomad evaluation constraint/resource blockers; unavailable telemetry is explicit.


### Conservative post-dispatch retry fence

A persisted `prepared_not_dispatched` receipt is the only state from which an
explicit retry may perform its first create-only registration. The producer
persists `dispatch_started` before invoking Nomad. A crash after this marker,
including before network I/O, is conservatively lookup-only. Lost ACK, failed
readback and subsequent job GC never permit a second registration. Legacy
submit_unknown receipts without a dispatch stage are also lookup-only.

CAS is not durable exactly-once idempotency across job deletion. The caller does
not claim that guarantee; a server-side durable idempotency registry/tombstone is
still needed for automatic recovery of that ambiguous case. Safe same-ID lookup
remains available, and no new-ID bypass or model prompt replay is automatic.


## Read the whole capacity snapshot

`capacity` starts with a concise `capacity_summary` row for every node; the
existing `nodes` detail stays available. Read all rows, including eligibility,
reasons, architecture, vCPU, guest MiB, disk GiB and observation time. Selecting
`nodes[0]` reports only the first node and cannot describe cluster availability.
A full first node never authorizes finishing the current VM. Ineligible and
unknown capacity remains null, including an unrecognized catalog pin.

Architecture is optional and defaults to auto. An explicit AMD64 constraint or
an exact template version available only for Linux still excludes ARM. Choose a
capability-compatible approved version from global `templates`; auto does not
silently rewrite a template version or an existing VM's binding. No automatic
ARM creation, migration or workload termination is part of this display change.

After a confirmed owned ACP cleanup, `runtime-rebind --session VM_ID --operation RECOVERY_ID --cleanup-receipt RECEIPT_BASENAME` initializes a fresh session on the same runtime without a prompt. Unconfirmed recovery blocks new work; status and historical transcripts remain readable. Reuse the same recovery ID for lookup, never choose a new ID to retry an unknown result.
