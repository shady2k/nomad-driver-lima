# Task-bound secret processes

Source implementation for review, disabled by default. Version0.1.33 is rejected due to crash-cleanup ambiguity; use only the reviewed successor with secret-process-v2. Existing managed operations run approved clients through official Bao Agent 2.5.4; no listener, bridge, auth method or token minting API is added. Environment delivery only; interactive stdin and secret-file profiles are not implemented.

## Caller interface

```
nomad-workers secret-profiles --session SESSION
nomad-workers operation-start --session SESSION --operation OPERATION_UUID \
  --label wiki-test --secret-profile model-test --secret-client opencode \
  --timeout 900 -- /operator/approved/client run 'short synthetic task'
nomad-workers operation-status --session SESSION --operation OPERATION_UUID
nomad-workers operation-logs --session SESSION --operation OPERATION_UUID
nomad-workers operation-wait --session SESSION --operation OPERATION_UUID --deadline-seconds 900
nomad-workers operation-cancel --session SESSION --operation OPERATION_UUID
```

Use the operator-installed entrypoint/config as usual. Listing contains approved IDs, versions, clients and a catalogue digest, not record paths or values. No profiles delivered means an empty list. A process receives the selected secret values; its descendants, same UID and guest root can read them. Never ask the agent to copy a key or choose a Vault path.

## Operator catalogue and delivery

Private JSON shape (nonsecret mappings, never credential values):

```json
{"schema_version":1,"profiles":[{"id":"model-test","version":"v1","description":"Approved synthetic client access","scope":{"namespaces":["workers"],"tasks":["worker"]},"clients":{"opencode":{"executable":"/operator/approved/client","work_root":"/workspace"}},"bindings":[{"record":"models/data/client","field":"API_KEY","env":"OPENAI_API_KEY"}]}]}
```

Actual record, task names, clients, work roots and endpoints come from private installation configuration. Profiles cannot override Bao/Nomad tokens or loader environment. Exact profile/client/scope/workspace/digest checks are repeated in the guest. Existing token ACLs are the real authorization ceiling; a mapping does not grant access to another record or isolate profiles sharing a token.

Driver config keys are `secret_profiles_file`, `secret_profiles_sha256`, `secret_profiles_bao_bin`, plus existing `vault_address`. File is absolute, operator-owned, non-writable by group/other, checksum-pinned, bounded and outside the ledger JSON glob (use `.config`). The Ansible role accepts optional `lima_secret_profiles_src`, `_sha256`, `_bao_bin`, `_dest`; all default disabled. It validates with the shipped compiler, backs up/copies mode0600 and emits the exact native driver fields. Inventory, endpoints, mappings and credentials remain external.

New tasks pin the catalogue and executable/origin in schema5 state. Driver delivers only namespace/task-matching profiles via stdin to task token tmpfs after checking guest identity. Envelope binds initiating agent/session/generation, worker session, allocation/task and machine/boot. Caller additionally verifies the full current Nomad attempt before every operation. Recovery re-delivery is immutable; resume preserves the pinned catalogue and delivers a new session/boot-bound envelope using the new task token. Older tasks are not silently opted in. Once schema5 exists, older binaries must not be used: their reader rejects it.

## Process and lifecycle

A finite trusted managed runner validates the selection. It enables Linux child-subreaper behavior for itself, registers guarded PID/start-time identities in task tmpfs, and starts one Bao Agent per operation with a minimal environment. Agent gets the existing token file and fixed `env_template` mappings. It may renew that token; Nomad remains authentication/issuance owner. No auto restart on secret changes: `restart_on_secret_changes="never"`.

A first-party exec shim receives mapped values in its process environment, passes them over a private FIFO to the runner's in-memory exact-value redactor, then waits for an ACK before executing the client. Client stdout/stderr have separate FIFOs; Agent diagnostics are discarded rather than mixed into protocol/log output. No secret values in HCL, argv, general task environment, caller receipts or diagnostic state. The shim strips Bao/Nomad token variables. Client HOME/XDG directories and all process-specific configuration reside in task tmpfs.

Existing status/logs/wait/cancel and durable operation IDs apply. A lost caller response reads the same operation; it does not restart it. A confirmed failure before ACK is `failed` with no client execution; ambiguous post-ACK outcomes remain unknown. Total operation deadline includes secret rendering. Cancellation targets only this operation and does not self-revoke the shared token. Active registration never means cleanup completed. The live subreaper must prove its child tree empty, fsync a separate process-tree completion receipt bound to registry nonce/operation/owner/boot, then atomically retire the active directory before removing private files. Finalization requests cooperative cancellation and waits for that receipt. A vanished runner without it is unconfirmed even if all recorded PID roots disappeared; no replay or inferred cleanup. Stale receipts cannot close another active registration. Uncertain cleanup retains registry/data. Tmpfs-file removal and task-token revocation remain separate from process-tree completion.

Token replacement/renewal does not change an already-running client's environment. Losing access to Bao does not erase a previously injected provider key. There is no automatic client restart or prompt replay. Explicit new operations pick up current credentials. VM finalization fences new starts, stops all registered secret-operation trees before the existing runtime stop/task-token revoke, then clears token/profile/per-operation tmpfs. Failed or unconfirmed outcomes retain data under the existing lifecycle rules.

Known values (including multiline fragments and common encodings) are redacted before managed/native/S3 logs. This is not prevention of deliberate exfiltration or arbitrary encoded output. Client code can persist a key to project files; same-UID/root code is not isolated. Only approved client trust scopes should receive secrets. Template/collect gates still require terminal operations, exclude reserved tmpfs, and retain their existing mutable-file credential checks; they cannot certify malicious project code never copied a key.

## Verification and rollout boundary

Tests include two independent Prime/OpenCode mock clients; cancel-one/other-survives; lost response/no replay; token replacement/no restart; pre-exec render failure; deadline; wrong binding/scope/symlink; collect exclusion; finalizer ownership/fence; schema5 finish/purge and resume pin preservation. An opt-in fixture uses installed official Bao against an ephemeral TLS loopback synthetic API, observes renew-self and verifies no per-operation revoke. It never calls a model or accesses business secrets. Linux subreaper orphan adoption and runner-death-with-orphan finalization have separate no-root kernel tests. They can run in a cached offline nonroot container with0.25CPU/128MiB/32PID; neither creates a production VM or uses credentials.

Review-ready source is not a live rollout. Required install scope: reviewed driver binary + caller package + private nonsecret mappings through Ansible, with an idle-gated coordinated driver restart and bounded no-model Linux acceptance. No new image is needed for this secret feature on the existing task token/runtime-owner contract; ACP logging image freeze is independent. No profile file, ACL, token, secret record or live process is installed/changed by source tests. New record scopes require separate exact approval; reusing the already allowed record needs no new read grant inherently.
