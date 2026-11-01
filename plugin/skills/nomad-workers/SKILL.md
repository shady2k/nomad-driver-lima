---
name: nomad-workers
description: Create owned native Lima work sessions through Nomad using approved prepared templates.
---
Use the VM-ID interface for ordinary coordination. Keep one public `vm_id` from
creation until finishing. Native session/request/attempt IDs, export snapshot IDs
and history cursors are internal. Do not generate them or select bound config
files for this workflow. Legacy commands below remain available for advanced
operations and compatibility; their explicit IDs are not the ordinary workflow.

1. Use `nomad-workers templates`, `models` and `capacity` for global discovery.
   The installed entrypoint supplies the global reference, including every
   approved architecture, even when a legacy bound config was previously used.
2. Run `nomad-workers vm create --template TEMPLATE --template-version VERSION
   --task-title TITLE --vcpus N --memory-mib N --disk-gib N [--model EXACT_ID]`.
   Architecture defaults to auto. Save the returned `vm_id`.
3. Use `vm connect VM_ID` until the same VM is ready, then `vm send VM_ID --text
   TEXT`. Observe that task with `vm reconnect VM_ID`; reread with `vm history
   VM_ID`. `vm status VM_ID` is read-only. A disconnected observer does not cancel
   the task. Reconnect to the same VM; never send the text again after an unknown
   dispatch. An optional `--request` selects a particular historical task only.
4. If the ID was lost, `nomad-workers vm list --mine` queries Nomad using the
   current native initiator generation. It does not adopt another agent's VM.
   Multiple allocations/unfinished tasks are explicit ambiguity. Missing durable
   target state requires recovery; listing alone does not authorize rebinding.
5. `nomad-workers vm finish VM_ID RESULT_PATH...` exports declared files, verifies
   receipts, then stops the exact owned workload through the current backend.
   **This release retains its disk: `disk_deleted=false`. It does not fulfill
   destroy or free disk quota.** Report this remaining cleanup explicitly; never
   claim deletion, purge manually, or acquire new cleanup permissions. Failed
   export or unfinished work retains the VM. Retry finish with the same VM/paths;
   the plugin keeps snapshot IDs internally. Exec-only sessions without a verified
   terminal model task cannot use this existing finish backend.

The native SessionStart/PreToolUse hook supplies private identity references and
an immutable per-Bash-call intent context. No agent-supplied UUIDs are needed.
Only one create/send is supported per native Bash invocation: run distinct work
in distinct tool calls. Repeating one call cannot dispatch it twice; changing its
input under the same call is rejected. Parallel native calls get distinct contexts.
Without the hook context, create/send refuse to dispatch. Old running sessions
keep their legacy CLI; do not restart them or invent identity to migrate them.
No hook permission override is returned. If another hook replaces updatedInput
and removes this context, report the missing context instead of guessing.

For a long model turn, inspect observable progress instead of spending repeated
five-minute waits without reading it. Keep the same VM and task. Read `vm status
VM_ID` and `vm history VM_ID`; identify the latest visible event/tool status,
terminal evidence, and any reported completeness/retention gaps. Report the last
observed activity and what remains unknown, without exposing hidden reasoning,
raw tool arguments, credentials or private prompt contents. `vm reconnect VM_ID`
reattaches to the existing turn; it does not submit or cancel it. A ready ACP
session, running VM, elapsed time or quiet stream alone cannot establish model
progress, completion, failure or a lost request. Long work can be valid.

If progress is unclear, distinguish task state from reader state. A read/watch
timeout, `TRANSCRIPT_SCAN_BOUND_EXCEEDED`, binding error or incomplete history is
a reader/observability blocker, not proof the task is idle or lost. Preserve the
same VM and dispatch receipt, report the exact error plus last verified status,
and seek repair of the reader. Do not replay, cancel, restart or clear unknown
gates to obtain a visible answer. Do not claim full visibility when history
failed. In caller 0.1.42 and later, transcript scans are resumable: follow `has_more` and its returned
cursor even when entries are empty while older events are scanned. Observe
`scan_in_progress`, `history_complete`, and explicit retention errors. Legacy CLI
users follow the same rule with status,
request-transcript and watch against their existing binding and request.

Creation retry and model-prompt replay are different operations. If allocation
creation times out, keep the returned VM ID and run `vm status VM_ID` for a
read-only lookup or `vm retry VM_ID` for safe registration reconciliation.
Retry uses the persisted exact job and create-only Nomad CAS; an accepted job is
returned, never updated or recreated. A first registration can be attempted only
when a durable `prepared_not_dispatched` receipt proves no dispatch intent was
recorded, the exact job is absent, and frozen payload/current creator are verified. A transport/ACL failure is not absence. Never pick a new VM ID to
bypass an unknown outcome. Temporary unknown during a network partition is real;
retry the same ID after connectivity returns. Once dispatch intent is recorded,
retry is lookup-only even if GET returns 404: a previously accepted job may have
been garbage-collected. Lost ACK + GC cannot be resolved from CAS alone. Do not
claim exactly-once registration or auto-create a replacement. A server-side
durable idempotency registry/tombstone is still required for that stronger contract.

`vm create --vm-id VM_ID` with the exact original flags is also a same-ID retry;
changed payload conflicts. `--purpose` and nonempty `--project` must be 1-32
lowercase letters/digits/hyphens, starting with a letter or digit (for example
`environment-preparation`, not `environment preparation`). Validation rejection
is explicit and does not mean Nomad accepted anything. Do not normalize or change
the payload silently. Only after definitive rejection before registration may a
corrected creation be an explicit new request; check capacity first. Legacy
facade receipts without durable proof of non-dispatch remain lookup-only, even
when original flags are available; never infer non-dispatch from missing files. A legacy allocation intent without a frozen job cannot be
resubmitted automatically. Model send/reconnect never replays a prompt.

Capacity interpretation: read ALL `capacity_summary` rows. Never select
`nodes[0]` and report it as cluster availability: it is only one node. A full
Linux node does not mean an eligible ARM node is full. Unknown/ineligible rows
are explicit; do not turn them into zero or available hardware. A full first
node is never a reason or authorization to finish the current VM. Preserve it
and inspect the other nodes and current task state.

Architecture is optional (`auto` by default). Explicit `--architecture amd64`
restricts placement to AMD64. Auto does not make a Linux-only exact template
version ARM-compatible: use global `templates` to choose an approved version
whose architecture/runtime and task capabilities match an eligible node. Do not
reuse an old bound target's Linux version or change an existing VM's binding to
move it. Do not silently substitute a version or create another VM without the
user's task authorizing it.

Before requesting resources, read global `nomad-workers capacity`. Compare each
node's architecture, status, scheduling eligibility/reasons, `effective_available`
and `observed_unix` separately; do not add AMD64 and ARM64 resources together.
CPU is whole guest vCPUs, memory is guest MiB after per-VM overhead, disk is GiB
bounded by both physical filesystem free space minus its reserve and retained
worker disk quota. `physical_disk` includes shared caches and other host usage;
cache usage is not measured separately, and it is not RAM. Retained disks may
consume quota without a running VM. Configured zero caps mean no additional cap;
unknown telemetry is null, never zero or unlimited. An ineligible node (including
maintenance) cannot accept work despite apparent free hardware. Capacity is a
timestamped advisory snapshot, not a reservation. Request output/retry reports
Nomad evaluation blockers when observable; unavailable evaluations are reported
as unavailable, not as proof of no blockers. Lack of capacity must not be guessed
as the cause of an earlier validation or transport error.

Environment preparation, capacity discipline, project acceptance and artifact
requirements below still apply to both interfaces. This interface change is not
permission to start project work, models or VMs outside the user's task.

Use only the operator-installed `nomad-workers` entrypoint and approved config.
Work in TWO PHASES. Phase 1 is environment preparation, not project execution:
request a minimal compatible VM, install dependencies, run bounded network/tool
preflight, and record actual versions plus a reproducible dependency recipe.
The initial Phase 1 plan MUST include a reusable-environment assessment: compare
the approved catalog with the dependencies being prepared and name a possible
project-purpose template (for example `example-project-dev`). Do not leave this out of the plan.
Include ONE short synthetic model request/response smoke as a functional check of
the configured model access. Select an appropriate exact model from the approved
catalog for the requested task; do not ask for a dollar amount or a separate
monetary approval before smoke/work. The plugin adds no monetary cap. Gateway
quotas/billing remain unchanged. Never include sensitive project data in smoke or
retry a request with an unknown outcome. Missing model access is a reported blocker.
Use accurate repository capability ceilings, not separate fake smoke/work profiles.
Use `nomad-workers models` for exact IDs and native role selectors. Choose initial
coordinator with `request --model EXACT_ID`; live-owner changes use `model-select`
with a fresh selection UUID. Internal workers use their own native `rlm.spawn` model.
Coordinator selection and native Prime RLM worker selections are independent;
workers may each choose a different catalog model. An omitted worker model inherits
the parent in Prime; do not impose that inheritance as a plugin requirement.
Prime0.10.1 ACP has no per-turn output cap; do not invent an ACP max_tokens field.
Starting Prime/acpx or seeing Nomad running/ACP ready does not mean project work
has started or that a real model request has passed.

At the end of successful Phase 1 checks, the environment-ready report MUST include
a concrete template draft proposal, not just versions and "waiting for acceptance":
name the proposed template ID/version (for example `example-project-dev`), dependency/tool
versions and reusable scope, and explicitly offer to prepare its draft with the
existing `prepare-template` and inspect it with `template-draft`. If an accepted
catalog entry already covers the same environment, identify its exact version and
recommend reuse instead of silently skipping the assessment or duplicating it.
If draft preparation was already requested, prepare the metadata-only draft from
verified recipe/checks artifacts and show its SHA; do not ask again for that same
non-destructive step. Otherwise make the draft preparation option explicit.

Then STOP before project tasks and request environment acceptance/Phase 2 transition.
If required checks or smoke failed/are blocked, report those blockers and the
incomplete template evidence; never present an unverified environment as reusable.
Do not run the project's test suite, benchmark or task automatically. A draft is
not a boot disk or a published template. Actual source-VM stop, clean-disk capture/
sealing and shared catalog publication still require explicit confirmation for the
concrete operation. Never snapshot live credentials, project checkout/data or owner
identity. Keep the prepared VM/data intact while the owner decides; never auto-stop,
purge, capture or publish as a side effect of completing Phase 1.

This is a SHARED node used by other agents. Available capacity is a snapshot, not
this session's entitlement. Request the smallest resources sufficient for the
CURRENT stage, leaving room for concurrent agents. Do not reserve a heavy-build
machine just for DNS, connectivity or tool inventory.

For an initial network/tool preflight, recommend 2 vCPU, 4096 MiB guest RAM and
max(8 GiB, selected template minimum disk). Smaller is fine when sufficient and
compatible. These are recommendations, not hard quotas or a universal minimum.
A real large compilation, race test or E2E run may need more: state the concrete
reason and expected peak CPU/RAM/disk before requesting it. Recheck occupied/free
resources and other agents' slots; never take all visible capacity by default.
Use the existing owned VM when suitable instead of opening duplicate workers.

There is no supported hot-resize promise. If a later stage needs a larger VM,
checkpoint files/results through the existing integrity/durability contract, then
finish the old session and request an appropriately sized new one. Never replay
an unknown model/exec request or discard unexported data. Finish promptly when the
work is done: stopped retained disks still consume quota until an explicitly
approved supported purge, so report that separately rather than deleting them.

Run `nomad-workers templates` and `nomad-workers capacity`; choose an approved
immutable template version, architecture and resources. Capacity is advisory;
Nomad and driver policy enforce admission. Never pin a default node or interpret a
single-node VM count as a global cluster limit. Use request/request-status, then the
returned target config for create/submit/watch/collect/result/finish. A template
proposal goes to operator review; never publish a catalog entry yourself.

Use the native SessionStart hook's creator record with --creator-record. Keep the
originating Claude session/agent instance separate from the new worker session UUID.
Hook observations are not authenticated proof of liveness. No background heartbeat,
automatic cleanup or ownership takeover. Old-generation submit is rejected; use an
explicit handoff rather than impersonating an earlier owner.

Generate and persist one request UUID per turn. After disconnect, watch that same
request/cursor, never resend its prompt. Repeated submit is receipt lookup; owner
death/unknown is an explicit error, preserve files and stop dispatch.
To reread an existing response, use `request-transcript --session SESSION --request REQUEST`.
Its returned cursor is independent from live `watch`; follow `has_more` with
`request-transcript --cursor CURSOR` until false. Repeating a page does not advance
watch or send a prompt. The output contains redacted visible text and tool statuses,
not raw tool arguments or hidden thoughts. Read `history_complete`/`tail_buffered`:
a growing text block is withheld until a logical boundary, and retention gaps or
bounds are explicit. A terminal `watch` immediately returns its cached terminal
receipt and a transcript next action; never submit another prompt just to recover
an answer. Disconnecting a watch closes only that caller's observer, not the live
request/queue owner/runtime. Requires the existing guest to remain accessible;
after stop use previously verified exported results when available.
Write prompts to a file with a QUOTED heredoc (`cat > prompt.txt <<'PROMPT'`) or a
literal direct file write, then `submit --prompt-file prompt.txt`. An unquoted
heredoc expands backticks and `$()` before Python/shell sees the prompt.

A terminal
model turn does not prove the user's task is complete. Cancelled/limited/error
outcomes remain visible; do not turn them into successful task completion.

Collect explicit result paths. New catalog targets also require full redacted native
stdout/stderr and TaskEvents snapshot; history gaps, rotation loss or upload failures
block finish. Report actual receipt backend. S3 is optional, but configured failure
never silently falls back. Verify checksums/committed manifest and use finish only
on explicit intent. No disk purge or paid canary follows automatically. Retain VM
and data if export/readback is unavailable. Keep provider policy/credentials and
installation endpoints outside public source and prepared images.


Installed caller binding: `nomad-workers` selects the private operator catalog
config. After `request`/`request-status` returns `target_config`, use
`nomad-workers --config TARGET_CONFIG create/submit/watch/request-transcript/collect/result/finish`
with the original session/request UUIDs. Never invent a config location or use the
retired plugin for a new Lima session. The returned target is attempt-bound.
The native SessionStart hook supplies `--creator-record` for a new work session.
Do not restart/resume an existing owner merely to upgrade: generation continuity
must be verified first, and its prepared VM/data must remain accessible.
Native caller authentication references the existing identity in private config;
never print, copy, rotate or send its token to a guest. Missing approved S3 writer
blocks durable finish; keep VM/data, never silently substitute local receipts.


Guest commands use the same owned target, NOT a host shell:
`nomad-workers --config TARGET_CONFIG exec --session WORKER_UUID --exec-id EXEC_UUID --timeout 60 -- /usr/bin/id`
Persist one fresh execution UUID per intentional command. Repeating it only reads
its receipt; an unknown/timeout result NEVER authorizes replay. New exec uses the same durable managed-operation runner as operation-start.
On completion output includes redacted stdout/stderr and actual guest exit_code.
If still running, output carries operation_id and next_action; use operation-status,
operation-logs or operation-wait with that SAME ID. Disconnect only stops observing;
never resubmit under a fresh ID. Explicit operation-cancel targets only its owned process.
Legacy exec_unknown IDs remain lookup-only and need the documented maintenance review. Nonzero exit
is failure even when transport completed. Exec stages appear in native task logs;
command output receipts are included in diagnostic export. Never put credentials
in command argv or print environment/secret files. Root/sudo inside your own VM
may install dependencies; proposing a reusable environment is optional, not a
prerequisite for one-off guest setup. No host Docker socket or host commands.

The native SessionStart must really run for a new initiating agent. Never fabricate
an event or copy/edit another owner's record. Existing VM ownership must survive
plugin updates; do not use resume as an ownership-repair workaround. A real resume
currently changes the observed generation and is not permission to take over it: if ownership
checks reject it, stop and request explicit supported handoff. No self-minted owner.

Capacity zero means no additional capacity of that resource is presently free.
An operator limit of zero means no additional configured cap, not zero resources.
`free_guest_vcpus_by_cpu = floor(free_cpu_mhz / cpu_mhz_per_vcpu)` is node-specific;
architectures are not interchangeable. Guest RAM excludes the per-VM overhead;
Nomad memory reservation includes it. Displayed free RAM/disk are not a reservation;
retained disks still consume driver quota and physical admission can reject a job.

Public and private endpoints are not guaranteed by a template. Before a download,
run bounded DNS/TLS/connectivity checks INSIDE your VM using exec and report the
specific failure; never silently widen egress or fall back to the coordinator.
Return a patch/bundle through explicit collect paths and checksum/manifest gate.
There is no implicit GitHub push authorization. Each guest artifact is limited to
8 MiB: split a larger bundle into declared bounded files, or report the limit.

For installation, compilation and other potentially long commands, use managed
operations instead of `nohup`, `&`, or repeated short exec calls:

```
nomad-workers --config TARGET operation-start --session SESSION_UUID --operation OP_UUID --label install-tools -- /usr/bin/sudo /usr/bin/apt-get update
nomad-workers --config TARGET operation-status --session SESSION_UUID --operation OP_UUID
nomad-workers --config TARGET operation-logs --session SESSION_UUID --operation OP_UUID
nomad-workers --config TARGET operation-wait --session SESSION_UUID --operation OP_UUID --wait-seconds 30
```

One ID identifies one command forever. Start returning running/launching is ONLY a
successful launch, not completion of installation or the user's task. Reconnect
with status/logs/wait for that SAME ID; never resend after an unknown launch.
Cursor-based logs can be reread explicitly with --cursor 0. Completion requires a
terminal state and saved exit_code; only completed + exit_code 0 is command success.
A failed command is not a successful task. Keep commands foreground inside the
managed runner so that exit and output describe the actual work, not a detached shell.

Deadlines come from the installed operator operations.json: recommended default
30 minutes and maximum 2 hours; --timeout selects a deadline within that policy.
Exec uses the managed operation deadline (default 30 minutes); its foreground
observation lasts at most foreground_timeout_seconds (normally 60 seconds).
A returned running operation continues; exec-status or operation-wait reads the
same durable result. --timeout is the operation deadline, not the caller connection lifetime. A wait timeout just stops waiting; it does
not cancel the guest command. An operation deadline/cancel stops only its verified
own process group; effects may be partial. Cancellation is explicit:
`operation-cancel --session SESSION_UUID --operation OP_UUID`.

Running/unknown operations and incomplete logs block finish. Export includes saved
operation state/output with checksums; failure retains VM/data. Do not forge the
native owner record to bypass generation or machine/boot identity checks. On owner
loss or reboot, report unknown and preserve results; never recreate the command.

Nomad logs show operation label + short ID + running/progress/exit stages, elapsed
milliseconds and byte counts. Existing v3 guests support that compatibility view;
full bounded redacted output text in native task logs needs guest log schema 2 in
a separately versioned prepared image. `operation-logs` works on v3 too. Updating
the caller never hotpatches a running guest/Prime or adopts an existing unmanaged
installation process. Secret values must not appear in argv or operation labels.

To propose a reusable prepared environment, `prepare-template` creates an immutable
review packet from declared recipe/checks files already collected in a verified
result manifest. `template-draft` inspects it. Use a purpose-based template ID and
new version; do not overwrite an existing catalog version. These commands do not
run the recipe, seal/capture a disk, stop a VM, upload or publish. Show the draft SHA,
contents and unknown size/digest to the operator for explicit next-step approval.
Disk size/checksum/minimum stay unknown until clean-candidate acceptance. Do not
claim the template is ready/published merely because its preparation report passed.
Never place credentials, project repositories or owner identity in a template.

A result export is a checkpoint, not a command to finish the VM. After the owner
accepts preparation, continue work in that same VM when its resources suffice.
Use a NEW `collect --snapshot SNAPSHOT_UUID` for a later verified snapshot, even
when reusing the same already terminal model request; do not send another model
prompt just to export. `result --snapshot` reads historical checkpoints.
`finish --snapshot` accepts only a current-revision snapshot and freezes new work
before close. Stale/legacy receipts, unknown work or failed uploads never authorize
finish. Once finalizing, retry only the same candidate; do not start new work.


Managed-operation output protocol:
- Use a meaningful operation label for each dependency step, for example `install-go`, `install-webkit`, or `build-libghostty`. Print nonsecret step boundaries inside multi-step recipes. Do not log credentials or raw environment.
- `operation-wait` emits NDJSON: parse each line separately. A frame has `state` as an object, so inspect `frame.state.phase` (or `frame.phase`), never compare `state` to a string. A running frame or successful launch is not completion.
- Example wait frame: `{"phase":"running","state":{"phase":"running"}}`; terminal example: `{"phase":"completed","state":{"phase":"completed","exit_code":0}}`. Verify terminal phase AND exit code before proceeding. `unknown` means inspect/reconcile, never resend the command.
- Read output with `operation-logs --session SESSION_UUID --operation OP_UUID --cursor CURSOR`; consume `events[].text`, retain the returned cursor, and repeat while more output is available. `eof` means caught up at that moment, not necessarily command completion.
- New operations forward redacted incremental output to native Nomad logs up to the operator's total `max_log_bytes` allowance. The legacy `native_output_bytes` policy field is accepted but normalized to this total allowance for new operations. Limits on existing running operations stay unchanged. An explicit `[NATIVE_OUTPUT_LIMIT]` notice points to `operation-logs`; check `state.output_truncated` separately because retained guest logs also have a finite limit.
- Quiet commands emit a status heartbeat only after at least 30 seconds without complete output lines. This proves the runner is alive, not progress. No new output can mean buffering; use explicit flushed step messages in recipes.


Saving a prepared environment is ONE user-facing action, not a request for the user to author schemas or replay an installation recipe. Offer: “Save this VM as TEMPLATE VERSION? I will briefly stop it, copy its prepared disk, clean only the copy, verify it and publish a new immutable version. The original disk stays retained.” Include a short explicit summary of retained tools and excluded project/runtime data. One confirmation binds that source, name/version and lifecycle; do not ask again for each internal technical stage unless a genuinely new permission is required.

The existing dependency recipe/check report is internal provenance, not a promise of exact replay, and does not require rebuilding the environment. A prior metadata-only draft remains usable evidence; produce a new disk-clone save plan referencing it, never rewrite its old hash or imply that it already authorized a stop/publication. Smoke usage may remain unknown when the existing response has no usage metadata; never send another prompt merely to fill that field.

Never call finish/stop/capture/publish before confirmation of the concrete source and template. Never clean or seal the original working disk. Only the isolated copied candidate may be scrubbed. A failed candidate operation preserves the original and evidence, and an unknown outcome must be reconciled without blind replay. Publication is not automatic node/catalog activation.


Readiness is an explicit sequence: `request` → `request-status` until allocation/task Running and target_config bound → call `create --session UUID` ONCE against that target → require its `phase: ready` and `record_id`. Then perform environment preparation and the authorized smoke. Do not wait for session readiness before calling create.

The public readiness layers are VM, transport, runtime and session. `request-status` uses Nomad observations for allocation/VM state; it does not execute a transport probe, runtime probe or session create. Therefore `transport_ready`, `runtime_ready`, `session_ready` are null when not currently verified. `runtime.name` reflects the configured runtime command, not proof that another adapter is supported. Follow neutral `readiness_stage`/`next_action`: `session_initialization_required` → create once; `session_ready_receipt` reports a historical durable create receipt (`last_observed_session_ready`), use target `status` for current liveness. `create_intent/create_unknown` means inspect the existing attempt, never recreate, resend a prompt or concurrently call create. The current implementation remains the existing configured adapter; these generic readiness fields do not add another runtime implementation.


For a long installation use one managed wait: `operation-wait --session SESSION_UUID --operation OP_UUID --deadline-seconds 1800`. This is a total caller wait window, not a per-poll 60-second limit; the existing operation policy still bounds it. The plugin performs bounded read-only polls internally and emits NDJSON. `wait_status: deadline_reached`/exit 3 means the wait window ended while the guest operation continues; it does not cancel or mark the install failed. Reattach using the SAME operation ID. Ctrl-C/disconnect ends observation only. Use explicit owned `operation-cancel` to cancel; never relaunch a command after an unknown outcome. This does not extend the guest operation deadline.


Preparation recipes MUST name each meaningful step and record its result. Inline/source the packaged `examples/managed-recipe.sh` INSIDE the owned operation's `/bin/bash -c` script, then use `worker_step install-tools COMMAND ...`, `worker_download download-go APPROVED_PINNED_URL /tmp/go.tgz`, and `worker_step verify-go sha256sum --check /tmp/go.tgz.sha256`. The helper runs inside the VM, not the coordinator; keep versions/checksums from the project. It does not print argv, URL or environment. Do not use silent `curl -fsSL` for a long transfer whose progress matters: use the helper or `curl -fSL --progress-meter`, plus a checksum step.

The runner converts helper markers into named step start/end, exit code and measured duration in native Nomad text. Download bytes come from the output file's actual size; total stays unknown unless explicitly observed/reported. Byte updates are throttled, with a final byte count even for short/failed downloads; the following step result determines success. Carriage-return curl meters are forwarded incrementally. A quiet heartbeat (minimum 30s) names the current step, elapsed time and age of last complete output. Diagnostic JSON `bytes` counts retained log data, NOT download/task progress. Never turn it into a percentage.

These producer changes affect new managed operations after the caller update. The current operation's stored runner is immutable; keep waiting on its existing ID rather than cancelling/relaunching an installation to obtain different logs. Existing v5 consumers accept the text through the standard stream field; no guest image or runtime restart is needed. Consumers with schema1 remain counters-only and must be reported honestly.


Repository profiles are optional operator configuration applied automatically to
new VMs. `capacity` reports approved per-node profile IDs/digests and the default;
an explicit `request --repository-profile ID` selects one, otherwise the node's
configured default applies. Do not replace this with `npm login`, credentialed
URLs, `go env -w`, insecure TLS, or editing Ubuntu signing keys. Native task logs
show profile apply/ready/error stages. Existing sessions keep their pinned profile.

Normal npm/Go commands and managed operations inherit the configured clients.
A managed APT drop-in also covers `sudo apt`, without changing sudoers. Unmapped
APT suites/components retain their original source: never silently redirect
updates/backports if the cache does not support them. Use ordinary non-root Go/npm
commands; direct `sudo [-n] COMMAND` is wrapped after sudo, while arbitrary nested
sudo policies/shell forms are not rewritten. Explicit root wrappers should retain
the operator profile, not switch to public mirrors.

For a manually downloaded Go compiler archive, prefer the optional
`NOMAD_WORKER_GO_DIST_BASE_URL` hint when present, append the exact release and
architecture filename, and still verify the recipe's pinned official SHA-256.
This hint is not a Go-standard setting and cannot transparently redirect an
already-hardcoded curl URL. Do not lower module/checksum verification or add a
new monetary question. Current anonymous cache reads require no new credentials.

When a human asks how to connect to your VM, use `connection-info --session UUID`
with your existing target config. It only renders a configured existing SSH
profile; it does not grant access, fetch keys or execute SSH. Report missing profile
fields honestly instead of inventing an address, login user, key or LIMA_HOME.

For a new VM request, include `--task-title 'Actual task description'` and the
initiating coordinator's descriptive `--source-agent-name` / `--source-agent-type`
unless genuinely observed native fields or private defaults already provide them.
Keep the native creator record/session/generation authoritative. The model inside
the worker is not the initiating coordinator's name. Supply an actual task link if
available; otherwise report it as not provided, never invent one. Older request
lookups remain available without inventing new metadata.

New worker jobs use the operator-approved native 30-minute disconnect grace:
keep the original allocation, no replacement while unknown, no client-side stop
on disconnect. During a transient control-plane outage, inspect the SAME request
and allocation; do not request another VM or replay an uncertain command/prompt.
After the finite grace expires Nomad may mark it lost; do not promise indefinite
continuation or automatically rebind failed jobs. Preserve retained data and ask
for operator-directed recovery. No automatic disk deletion is enabled.

### Find existing work before requesting another VM

Do not ask the user to copy worker UUIDs between agents or choose a VM by its name.
Use `find --project PROJECT --creator-record "$REAL_CREATOR_RECORD"` (or `list`)
with the current native record. `--mine` is the default: exact initiating agent
instance, native session and generation. The JSON supplies `session_id`,
`allocation_id`, `target_config`, task title, initiator, current/previous IDs and
observed Nomad state. Use these returned IDs with the existing target `status`
command; discovery itself does not probe ACP or execute work. Project/title are
descriptions, never ownership. `--all-owned` is an explicit read-only broader view,
not permission to adopt another creator's VM.

If multiple current candidates exist, present their tasks/states and ask which
work to continue. Never select the newest automatically. Missing/denied observations
mean unverified, not stopped. Historical rows expose `historical_target_config` separately from
`current_target_config`; use the non-historical candidate's current ID/config pair.
Historical source IDs remain historical; no automatic
alias adoption, ownership handoff, prompt replay or new allocation is performed.
Future verified resume/recovery bindings persist the descriptive ID lineage.
For already recovered sessions an operator can run `index-lineage --session NEW_UUID`
after checking the exact source/target evidence; this writes only the local mapping,
not owner records, jobs or VM data. Installation of this release does not itself
index or migrate existing sessions.

For an intended stop of your own current model turn, use `vm cancel VM_ID`, then
`vm cancel-status VM_ID` and `vm reconnect VM_ID`/`vm history VM_ID` to observe its
actual result. Ambiguity is an error, never guess a task. `cancel_accepted` means
only the existing runtime accepted cancellation; it does not mean the turn/tool
has stopped. Unknown outcomes are lookup-only, never resend under a new ID or
clear the gate. Status/history/transcript and operation status/logs remain usable
while work/cancellation is unfinished. They still verify current ownership and
attempt. Do not turn a quiet watch/read error into an automatic cancel, prompt
replay, daemon/VM restart or unrestricted exec workaround.

## File transfer and finite shell

Use existing VM ID; default configuration and verified native initiator identity
are supplied by the normal wrapper/hook. No SSH key or Git credential is needed.

```sh
nomad-workers vm upload VM_ID ./source.tar.gz --destination input/source.tar.gz --extract-to source
nomad-workers vm upload-status VM_ID
nomad-workers vm upload-reconcile VM_ID
nomad-workers vm upload-abort VM_ID
nomad-workers vm exec VM_ID --timeout 60 -- /usr/bin/id
```

Upload uses existing S3 exchange, 8 MiB multipart chunks, full SHA-256/size checks
and atomic workspace publication. Maximum input is 256 MiB. Optional extraction
supports plain/gzip tar with bounded metadata and rejects traversal, links,
special files, duplicate paths and overwriting existing output trees. Never embed
source bytes as base64 in shell commands or transfer Git credentials.

Retry the same file/destination after an interrupted upload, or reconcile its
existing receipt; do not invent an ID or replay a model prompt. Status performs a
read-only inspection. Abort follows the durable exact transfer binding and
removes only its private unpublished file; published outputs and objects already
in exchange retention remain intact. Incomplete extraction staging requires
reconciliation. An optional transfer ID only disambiguates multiple transfers;
normal use needs the VM ID alone.

Finite exec uses existing Nomad allocation-exec and managed operation receipts,
independent of ACP availability. It is not direct SSH. The native tool-call
context keeps command retries bound; changing command input under one call
conflicts rather than dispatching a replacement. Unknown operations/transfers
retain their normal safety gates. This update does not implement resource resize,
disk destruction or change finish semantics. Shell-only cleanup still requires
the separately approved existing lifecycle/operator path; do not submit a dummy
model request just to satisfy a missing cleanup capability.
