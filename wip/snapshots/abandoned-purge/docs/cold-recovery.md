# Cold task recovery and exact-pair maintenance

Nomad persists a TaskHandle through its local state database. The SDK's private
TaskConfig raw driver configuration is not preserved by that round trip. Handle
version 2 therefore persists only attachment inputs: allocation/task/node/session,
resource identity and whether Vault is required. It excludes commands, environment
and credential values. Recovery attaches to the owned VM and never launches main.
Legacy handles use existing durable identity and exposure history; missing policy
is an error, not permission to assume that a task is secret-free.

## Recovery of an already duplicated invocation

This is a narrow operator operation, not a caller ownership bypass. Both exact
VMs and disks are retained. A pinned private plan identifies original and duplicate
by task ID, ledger key, instance, machine, boot and canonical ledger SHA256. The
pair must have the same allocation/task/session/creator. All duplicate exposure
hashes must be covered by the original's recorded exposure history.

The operator must first arrange a coordinated management maintenance window.
Both initiating agents must pause; this does not establish whether a queued model
turn completed. Do not resubmit an unknown turn. The client/plugin must release
both task locks. Stopping the Nomad client is permitted only after verifying its
service stop policy preserves guest processes and proving that every other live
task can cold-recover. It temporarily interrupts management of other tasks; guest
continuity must be checked before and after, not promised from unit tests.

Prepare mode-0600 operator files, without credentials:

```json
{"state_dir":"/operator/state","lima_home":"/operator/lima","limactl":"/operator/bin/limactl","runtime_user":"worker","vault_address":"https://secrets.example.invalid","expected_version":"2.2.1"}
```

The plan has `version: 1` and `original`/`duplicate` objects, each containing
`task_id`, `key`, `instance`, `machine_id`, `boot_id`, `ledger_sha256`. The ledger
digest is the SHA256 of its canonical stored JSON bytes, not an invented new row.
The command verifies owner, configuration, guest identity and the exact state.

```sh
nomad-driver-lima recover-owned-pair \
  --runtime-config /operator/private/runtime.config \
  --plan /operator/private/pair.json --plan-sha256 <approved-sha256>
```

This is read-only preflight. After the one explicit confirmation covering both
stops, shared token revoke and retention, repeat with `--confirm-stop-both`.
Before effects the operation takes admission and both task locks, rechecks the
plan and fsyncs an intent audit. It then:

1. Confirms stop-only of both owned Prime/Bao runtime trees before any revoke.
2. Uses the original's normal driver stop/revoke path to obtain a real retained
   receipt and stop that VM.
3. Stops the duplicate VM, retaining its disk. Its tokens were covered by the
   original's confirmed revoke, so it does not repeat revoke-self and misinterpret
   a resulting 403. It never creates a guest revoke receipt or a resumable receipt
   for the duplicate.
4. Fsyncs completion evidence. A failure reports the last phase; inspect retained
   state/audits. Never edit the ledger or blindly replay an old plan.

Afterward restart management with the reviewed driver, verify unrelated tasks,
and use the normal retained-disk resume operation for the original with a new
allocation, worker session and task token. The original's existing disk stays
owned; no recipe rebuild, allocation-handle forgery, prompt retry or disk deletion
is involved. Old ACP journals are historical evidence, not a queue to replay.

## Resource safety

Admission uses fresh Lima inventory as well as uncertain ledger reservations.
A surviving runtime counts even when its ledger phase is stopped/destroyed.
An observed stopped runtime with both native PIDs zero consumes no vCPU/VM slot or RAM; its disk
still consumes quota. Unknown inventory or configured RAM denies new admission.
Configured guest RAM plus per-VM overhead is used, never current RSS.

The physical host ceiling is enforced even when operator caps are zero. Set
`max_total_memory_mib` to an authoritative host allocatable ceiling after OS/host
reserve; zero means no additional operator cap, not unlimited physical RAM.
Fingerprint and admission use the same runtime CPU/VM/RAM inventory. Caller fit calculations expose the same RAM commitment and
finite effective memory ceiling. The snapshot is advisory, never a reservation.
Current physical probing does not claim to discover every possible nested cgroup
constraint; any additional service/cgroup limit must be reflected in the explicit
operator cap before admission is enabled.

## OOM isolation proposal — not applied

Admission cannot isolate an individual guest OOM. A separate installation change
should place each guest process tree in its own systemd/cgroup scope, with an
explicit memory limit and `memory.oom.group`, outside the client service's failure
domain. Verify actual QEMU/host-agent cgroup inheritance on the supported Lima
version first. Reconsider the client's `OOMPolicy=stop` only with that tested scope
layout: changing it alone does not protect the host. Do not switch to a service
KillMode that kills unrelated surviving VMs. No systemd/cgroup settings are changed
by this source patch or maintenance command.

### Resume when Nomad no longer has the original handle

Maintenance stdout contains a bounded `resume_source` binding and SHA256 of the
real original receipt. Save this complete successful result as a private file.
It is operator evidence, not a fabricated Nomad TaskEvent. After the old affected
allocation is terminal/stopped through Nomad, the operator can use:

```sh
python3 tools/resume-maintained-source.py \
  --config /operator/private/caller.config --session <fresh-worker-session-uuid> \
  --source-allocation <original-allocation-uuid> \
  --creator-record /operator/private/actual-native-creator-record.json \
  --maintenance-receipt /operator/private/pair-completion.json \
  --maintenance-receipt-sha256 <sha256-of-completion-file> --execute
```

This uses the existing caller configuration/identity and an actual native creator
record. It submits an ordinary resume job through Nomad, preserving standard
model configuration, owner binding, target persistence and fresh task-token
issuance. It never supplies a token or submits a prompt. A lost submit response
is lookup-only; rerunning must not create another job. A changed source or receipt
for the same new session is rejected. This operator tool does not install or
replace the caller plugin; keep the current installed plugin and its stable state.
