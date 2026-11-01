# Process secret profiles — approved design

Use the existing durable managed-operation interface, Nomad workload token and official OpenBao Agent. No additional service or arbitrary Vault-path API.

## User flow

1. Operator defines a versioned profile: logical ID, fixed record/fields, environment mappings, allowed clients/work directories, eligible task scopes and delivery mode. Endpoints and record paths belong to installation configuration.
2. At VM creation the operator policy authorizes a subset of profiles. `secret-profiles --session S` lists only those profiles and their non-secret purpose.
3. Proposed interface: `operation-start --session S --operation O --label wiki-test --secret-profile llm-test --secret-client CLIENT_ID -- CLIENT ARGS`. Existing status/logs/wait/cancel apply. Source implementation and rollout limits are documented in secret-profiles-contract.md; not installed by this source task.
4. Trusted operation runner validates owner/generation/machine, exact profile digest and client. It starts a finite official Bao Agent supervisor using the existing private tmpfs task token. Agent renders the selected fields into the child environment; client receives no Bao/Nomad token. No key in argv, image, generic task environment or caller response.
5. Disconnect leaves the operation running; reconnect reads its durable result. Unknown outcome never resubmits. Stop/cancel targets only the registered process tree with PID/start-time guards.
6. Operation completion stops its child and supervisor, clears its private tmpfs. It does **not** revoke the shared task token. VM finish stops all children, then performs the existing single task-token revoke and tmpfs cleanup before VM disposal.

## Native constraints and lifecycle

OpenBao process-supervisor mode uses `env_template` and one top-level `exec`; ordinary file templates cannot be mixed into that mode. Set `restart_on_secret_changes="never"`: changes must not replay tests/model requests. Nomad remains issuer/authentication owner; Agent renewal behavior remains explicit. Token replacement and secret rotation cannot rewrite an already-running child's environment. Restart is an explicit new operation, never automatic replay.

Several operations can each run a finite supervisor; no persistent companion is needed. Their shared task token is intentionally a shared authorization boundary. Separate record scopes are not strong isolation if the same guest root can read that token. Strong per-profile isolation requires separately scoped tasks/VMs, not a generic token-minting privilege.

Initially support environment delivery only. A file-only client needs an explicitly approved file profile, per-operation tmpfs0600 and ephemeral HOME/XDG state. Official file-template mode plus a finite existing runner must gate start on successful rendering. Mixed env/file delivery needs a tested separate path; do not claim supervisor supports it automatically.

Supervisor diagnostics and client protocol must have distinct pipes. Do not merge Bao banners into ACP stdio. Interactive clients require verification of their actual transport contract; Prime's daemon socket is not a generic client protocol.

## Security boundary

Client and its descendants can read inherited secrets. Same UID, guest root and untrusted code executed inside that process may observe or exfiltrate them. A profile authorizes this trust scope; it does not make a key invisible to project code.

Redact known injected values before durable/native/S3 logs, including chunk boundaries; omit environment dumps and raw credential-bearing argv. Reject token-variable mappings, loader variables and arbitrary profile-provided executable/environment overrides. Never snapshot operation tmpfs; template sealing must remove all reserved runtime state and verify absence before boot/export. Regex masking alone is not proof that arbitrary program output contains no secrets.

If provider keys must never enter the guest, use an already-operated external credential gateway as an explicit dependency. This is a different trust model, not a new proxy bundled into this plugin. Its guest credential can still be misused by same-UID code; stronger network/UID boundaries require separate review.

## Permissions and rollout gate

The first LLM profile may reuse the currently authorized LLM-only record: no new secret, persistent grant, auth method or token-create permission is inherently needed. Operator must approve fixed profile/client mappings and runner changes. New records/scopes, file delivery, isolation or gateway access require their exact separate permissions. No live policy/credential changes are part of this proposal.

Before implementation acceptance: synthetic two-child isolation; stop-one/other-survives; wrong profile/path/env rejection; chunked exact-value redaction; replacement/expiry/VM finish; no automatic restart/replay; snapshot exclusion; client stdout protocol separation. Explicitly demonstrate same-UID visibility rather than promising isolation.

Sources: https://openbao.org/docs/agent-and-proxy/agent/process-supervisor/ and https://openbao.org/docs/agent-and-proxy/agent/ . Supervisor is upstream beta; pin/test the installed release.
