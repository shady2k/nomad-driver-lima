# Confirmed template save workflow (source checkpoint)

The caller prepares a manifest-bound plan identifying the exact source allocation, session, owner generation, current verified result snapshot, proposed template ID and new immutable version. Confirmation binds that plan and its explicit actions. It is an intent record, not cryptographic proof of a human approval; execution requires the existing trusted operator publisher role.

The plan requires an actual disk copy: official `limactl clone --tty=false --start=false SOURCE CANDIDATE` after source finish and stop. No dependency reinstall or clean-rebuild fallback is allowed. The dependency recipe remains provenance, not a replacement for the prepared disk. An explicit data-composition manifest binds retained/removed paths and a human-readable description; overlapping paths are rejected. OpenBao does not establish that arbitrary disk data is secret-free. Only the candidate may be cleaned/sealed; the source remains unchanged.

After final result durability checks, source Prime/Bao stop and its task token self-revokes. The original VM is stopped and its disk retained; it is not deleted, automatically restarted, or transparently resumed. Continue in a new work session from the accepted template and preserved results. A failed/unknown stage retains its evidence and data; no automatic replay is permitted.

The finite controller journals intent before each stage under a lock. Stage receipts bind operation ID and plan digest. Candidate checks require standalone disk/checksum, two independent boot identities, dependency checks and unchanged source artifact. Publication uses a new immutable prefix and conditional catalog append with readback. Existing versions cannot be overwritten. Published entries remain pending activation: pinned node/caller catalog configuration changes are a separate coordinated Ansible step.

Current source APIs: `template-save-plan`, `template-save`, and `template-save-status`. Existing `prepare-template` drafts remain metadata-only. Nothing executes merely by producing a draft or plan.

## Implemented clone binding

`deploy/template-clone.py LIMA_HOME LIMACTL` consumes the controller stage JSON on stdin. Run it as the existing runtime user, through the fixed trusted operator command binding. It requires a bound source-finish receipt with `source_instance` and `source_disk_identity` (SHA256 by disk filename), validates exact stopped CLI identity/directory, refuses symlinks and existing candidates, uses no start/delete/stop command, and verifies source disk hashes again after clone. Candidate identity is deterministic from operation UUID; an existing/partial candidate fails closed for explicit reconciliation. No clone was executed outside fixtures.

## Remaining operator binding gate

The controller and conditional-write code have offline fixtures; no live save or S3 write was performed. The existing image playbooks still need fixed trusted command bindings emitting the validated stage receipt protocol. In particular `base-seal.sh` intentionally refuses credentialed-worker state: it must NOT be run unchanged on a cloned working environment. Candidate-only sanitation must remove the agreed runtime/project paths, scan retained content, discard free filesystem blocks and produce bound sanitation evidence before that existing identity-seal/export/acceptance pipeline can run. Do not turn absence of known strings into a universal secret-free guarantee. This candidate sanitation binding is not yet implemented; execution must remain blocked until it is tested. The policy preflight requires source ownership, isolated builder capacity, existing scoped writer and verified backend conditional writes before source stop. Merely setting a flag does not prove S3 conditional support: test competing conditional writes in a disposable operator-approved prefix first.

Required existing-role access: read the exact source/task metadata and verified result receipt; stop only the confirmed source; run the existing isolated builder; write/read new immutable environment prefixes; conditional read/write of the one catalog object. No caller/guest S3 keys, new persistent service, or broad grants are supplied by this patch. Actual binding and specific source publication remain pending, not operationally complete.


## Concrete finite stage bindings (source, not installed)

- Caller identity: `template-source-finish.py TARGET_CONFIG` uses existing Adapter result/finish and native owner/attempt checks. It rejects a missing Vault cleanup contract before effects. Nomad owns token revocation/VM-stop confirmation as before.
- Existing node operator: `template-source-clone.py PRIVATE_CONTEXT` takes the exact driver ledger key, locks that actual key, validates source allocation/session/creator and retained terminal state, invokes official clone as the existing runtime user, and rechecks unchanged ledger before releasing the lock. Clone disables mounts, provisioning, probes, personal SSH keys and agent forwarding on the candidate only.
- `template-artifact-stage.py PRIVATE_CONTEXT` binds candidate sanitation to existing base-artifact seal/export/boot/check/catalog helpers. It creates only a private tmpfs marker, never sends provider credentials to the candidate, and generates unique per-operation acceptance names. Two acceptance VMs are sequential and stopped with disks retained. No recipe is executed or dependencies reinstalled.
- `template-publication-stage.py PRIVATE_CONTEXT` reuses existing publisher/readback and CAS append. URLs are explicit existing operator cache/read locators; no public bucket policy or signed credential URLs are introduced.
- `catalog-cas-probe.py PRIVATE_POLICY EXACT_KEY` is prepared but NOT run. It creates one small retained object under `environments/.cas-probes/<UUID>/probe.json`, checks If-None-Match, competing If-Match (one winner), stale If-Match rejection and exact readback. Needs existing writer GetObject/HeadObject/PutObject only on that exact key; no delete, list, catalog or base modification. The result binds endpoint/bucket/region by hash. Existing access must cover this prefix; otherwise report that exact permission boundary before running, do not add a grant silently.

Installation-specific context remains outside the repository. The root/node role already needs private ledger read/lock and run-as-existing-runtime-user access; the caller role keeps its existing Nomad identity; the publisher keeps its existing OpenBao-backed S3 writer. No token is moved between these roles or to a guest. Before a concrete capture, fixed contexts/command mappings, source ledger key, current catalog SHA, frozen assets and dependency probe policy must be bound by the existing operator deployment. No such live binding was applied in this source task.

The sanitizer removes only known application runtime paths/generated SSH identity and the agreed candidate-only remove list. It scans bounded preserved config/user/log data, fails closed on unreadable/oversize data or matching secret patterns, and records hashes/counts rather than content. It is finite detection, not proof against an arbitrary hidden secret. Existing identity seal performs cloud-init identity reset and filesystem trim; export keeps QEMU/VZ normal locking. Candidate execution still requires trusted runtime/autostart/mount preflight before startup.

## Existing draft compatibility and simple UX

A draft such as `example-project-dev v1-20261031` remains immutable provenance even when its old `next_step` mentions the former clean-builder plan. Generate a new save-plan hash with `official_stopped_disk_clone_then_existing_seal_export_pipeline`, referring to the original draft hash; never mutate that draft or treat its confirmation flag as approval to stop. The agent handles technical metadata internally. The user confirms source, name/version, data summary and brief stop once. An existing successful smoke response is reused without a model replay; unknown usage stays unknown.

## Release 0.1.13 deployment contract

One source release contains caller and finite operator helpers; no daemon is installed and no Nomad restart is required. Keep the active source session's owner record/generation. A caller cache update does not authorize a fabricated SessionStart or handoff: use the new executable explicitly from the still-valid current native session; do not force a resume merely to save its VM.

`template-save-execute.py --policy PRIVATE --plan PRIVATE --confirmation PRIVATE --journal PRIVATE` runs on the EXISTING Mac operator/publisher identity. It does not load or impersonate the Linux caller's local initiator state. The real caller preflight/finish runs on the caller host through the existing controller SSH. This separation does not grant an ordinary caller direct publisher/host privileges.

Required context additions:
- Operator command `preflight`: fixed invocation of `template-preflight.py PRIVATE_PREFLIGHT_CONTEXT`. That context has `source_preflight`, `node_preflight`, `publisher_preflight` argv arrays using the same existing SSH identities. Source command is `template-source-finish.py TARGET`; node is `template-preflight-node.py NODE_CONTEXT`; publisher is `template-publication-stage.py PUBLISHER_CONTEXT`.
- Node context: existing fields plus optional `acceptance_cpus` (2), `acceptance_memory_mib` (4096), `physical_reserve_gib` (5), `readiness_timeout_seconds` (600), and `model_definitions_file` (defaults to the pinned repository definitions fixture). Preflight creates only a private UUID build root, stages source-frozen manifests/seal/probe assets idempotently, checks current source/available memory/CPU/physical disk, and does not stop it. Acceptance waits for exact Running identity AND a successful native shell probe; the no-prompt catalog-control proof is generated against the NEW disk and passed to catalog generation, retaining `prime_model_catalog=1`.
- Publisher context: `artifact_fetch_command`, a fixed existing operator SSH command invoking `template-export-stream.py NODE_CONTEXT`. The stream accepts only fixed accepted-artifact filenames and binds each header to the plan/op; receiver writes private temp files with bounded size/deadline and SHA verification before immutable rename. No tar, arbitrary paths, credentials or public object access is added.
- An absent shared catalog requires `expected_catalog_sha256: null`, `initial_catalog_file` and `initial_catalog_sha256`, pinning a NONEMPTY compatible catalog with the existing accepted agent-base versions. Preflight only reads/checks this seed. Commit initializes once with `If-None-Match: *`, adding the new version while preserving seed entries. A concurrent creator cannot be overwritten. Existing catalog updates still require pinned SHA + ETag CAS. Never invent an empty initial catalog or silently replace a changed one.

CAS proof has now been supplied by deployment using the existing publisher; source tests also cover initialization races. No live shared catalog was written by this source task. Operator contexts remain private installation data, not repository fixtures. The source VM remains running until the one concrete confirmation.

### Verified public package fixtures during candidate sanitation

Some official toolchain files contain public test keys, parser strings, and
credential examples. The sanitizer still scans every retained file. A matching
file can pass only when its complete SHA-256 equals an operation-bound trusted
artifact record supplied by the operator. Modified files and unlisted files
continue to fail; user/configuration paths cannot receive this exception.

The private node context can supply `trusted_provenance_file` and
`trusted_provenance_sha256`. The file must be a regular, operator-owned file
without group/other write permissions. Its schema is version 1, with the exact
`operation_id`, `plan_sha256`, and `files` entries containing `path`, `sha256`,
and `provenance` (`kind`, `evidence_sha256`). Evidence must come from an exact
accepted base disk, a checksum-pinned official archive, installed package
checksums, or independently reproduced package metadata. A checksum of the
current file alone is not provenance. The operator must retain the supporting
reports; this is trusted operator input, not cryptographic attestation or a
permission available to the requesting agent.

The sanitation receipt records the evidence digest and number of verified
public matching files. Runtime state cleanup, SSH identity sealing, full scan
quotas, and subsequent independent boot checks remain mandatory. Pattern
coverage is finite and does not establish a universal absence of secrets.

### Retrying a source refusal before finalization effects

A verified checkpoint snapshot of the selected terminal request is sufficient to
finish that request; its immutable base turn need not be rewritten as exported.
The snapshot must bind the same session, request, ACP request and terminal
result. Other unexported turns or snapshots still prevent finish.

The operator can add `--retry-source-pre-effect` to the same
`template-save-execute.py` invocation only when the journal is unknown at
`source_finish` and has no completed stages. Under the journal lock this uses
the existing fixed source command in `source_finish_pre_effect` mode. The actual
caller verifies current ownership, ready session, running allocation/task, exact
attempt and current result snapshot, no closing candidate and no pending work.
A successful proof is retained in `pre_effect_retry_audit` before retry. No
journal edit, prompt replay, weaker export receipt, or source identity handoff is
needed. Unavailable/invalid proof preserves the unknown journal. Unknown later
stages and a source with any finalization intent cannot use this retry path.

### Retrying clone when the source has already finished

`VaultFinalizing` remains a durable no-redelivery fence after normal driver stop;
it is not proof of pending cleanup. Cloning still requires a terminal retained
ledger phase, matching owner/session/attempt, the confirmed source finish
receipt (including token revocation), and a physically stopped source.

For an unknown `clone_candidate` with only `source_finish` recorded, the operator
can use `--retry-clone-pre-effect` with the same plan, confirmation and journal.
Under the ledger/candidate locks the existing node command proves exact candidate
absence, stopped source/no live PID metadata and complete source disk hashes.
That proof is audited, and both ledger and disk hashes are checked again before
clone. Any partial candidate, running process, changed source or later unknown
stage rejects retry. The completed source finish receipt is retained unchanged;
source finish is never replayed. Resumed preflight uses the retained-source node
checks and normal publisher checks instead of requiring the source to be running.
The ledger itself is not rewritten to clear the fence.

The deployed ledger schema stores creator agent/session IDs but has no
`CreatorGeneration`. For that schema the successful source finish receipt binds
the exact immutable caller-generated plan, including caller config SHA and
generation. That verified receipt is the generation evidence; no ledger field
is invented. New finish receipts also record the explicitly compared caller
creator identity. A conflicting generation in either a newer ledger field or
an explicit finish identity is rejected, as is a receipt from a different plan.
