---
name: nomad-workers
description: Create owned native Lima work sessions through Nomad using approved prepared templates.
---
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
death/unknown is an explicit error, preserve files and stop dispatch. A terminal
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
`nomad-workers --config TARGET_CONFIG create/submit/watch/collect/result/finish`
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
