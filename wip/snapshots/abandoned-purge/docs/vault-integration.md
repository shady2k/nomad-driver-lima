# Native Nomad/OpenBao integration — v3 source acceptance

This is tested source, not a claim of deployed Nomad/OpenBao/model acceptance.
Published v1/v2 disk artifacts stay immutable; `docs/releases/guest-assets-v1.json`
and `docs/releases/guest-assets-v2.json` retain their freezes.
`docs/guest-assets.json` is the new v3 guest release.
The release SHA is SHA256 of canonical JSON `files` (sorted keys, compact encoding).

## External contract

Nomad owns workload JWT login and task-token issuance. Configure its standard
Vault integration for the operator OpenBao endpoint/auth mount/default identity.
The approved role is `agent-worker-prime`, namespace `agent-workers`, jobs
`worker-*`, task `worker`, audience `vault.io`. Token TTL30m, explicit maxTTL1h,
renewable service token, unlimited uses, no generic default policy.
Only the dedicated LLM record plus lookup-self/renew-self/revoke-self are needed.
No AppRole bootstrap or shared host/operator token is delivered to a VM.

Driver operator config needs `vault_address = "https://<approved-origin>"`.
Catalog plugin config has `vault: {"role":"agent-worker-prime"}`. This adds native
job `Vault={Role:"agent-worker-prime",Env:false,DisableFile:false,ChangeMode:"noop"}`
and `vault_required=true` in task driver config. The plugin guest config requires:

```
prime_vault:
  address: same approved OpenBao HTTPS origin as driver
  secret_path: exact KVv2 API path of the dedicated LLM record
  bao_bin: absolute installed official Bao binary
  kernel_python: pinned existing Prime kernel Python
  provider:
    name: external provider alias
    base_url: existing provider HTTPS endpoint
    model: existing approved model ID
    max_tokens: external output ceiling
    context_window: verified model context bound
```

`agent_argv[0]` is the absolute official Prime binary. The launcher builds its ACP
mode arguments from this external config. No secret values belong anywhere in
these objects. `plugin/config.catalog.example.json` uses neutral placeholders.
Provider model metadata references `LLM_API_KEY` by environment NAME only and sets
`x-opencode-session` to the durable worker session UUID.

## Delivery and process lifecycle

The driver reads only Nomad TaskDir.SecretsDir/vault_token, bounds regular-file
reads, refuses symlinks and secret task environment variables. It checks the
owned guest machine/boot identity before sending tokens over the existing Lima
SSH stdin channel. No token in argv, task logs, ledger, handle or artifact cache.
A protected tmpfs directory stores current/exposed old task tokens solely for
lifecycle cleanup. Host mounts, JWT auth code and transport daemons are absent.
Actual Store.Owner is32hex; the earlier helper's incorrect64hex expectation is fixed.

Adapter create writes a non-secret runtime request. Allocation task-main owns
upstream Bao Agent -> official `prime-agent --mode daemon`. Bao env_template
injects only LLM_API_KEY into that child. Its banners and diagnostics go to native
Nomad task stdout/stderr; the ACP stream is a SEPARATE official Prime CLI connected
to the private daemon socket. The ACP process has no LLM_API_KEY/BAO_TOKEN/VAULT_TOKEN.

PID/start-time, controller identity and socket dev/inode guard every new launch
and submit. Missing/dead/replaced owner is an explicit error, never prompt replay.
A private flock serializes runtime start and stop, including the intent/PID window.
Start intent with unknown PID outcome is retained, not guessed or replayed.

Prime0.10.1 itself has an unconditional daemon ensure/autostart path. Guards reject
observed owner death before dispatch and the fixture proves no prompt/autostart in
that case. A daemon dying BETWEEN a guard check and upstream ensure can still race
inside Prime; no unsupported no-autostart switch, fork or custom bridge is claimed.
Task-main detects the changed/dead owner/socket and fails instead of adopting it.
This residual upstream TOCTOU is not proof of transparent crash recovery.

## Finish, replacement and revocation

Verified artifact/log receipt -> close ACP -> finish marker -> stop only owned
Prime/Bao process identities -> self-revoke every exposed task token -> clear
volatile tokens -> confirmed Lima Stop. VM disk/data are retained; never purge.
Explicit cancel uses the same owned-runtime/token cleanup without claiming upload.
A one-shot cleanup imports the same plugin task-main functions if its controller
has died; it is not an added service. Reused PIDs are never signalled.

The driver journals finalization BEFORE signalling, then suppresses delivery under
its secret mutex across recovery. Token replacements after that gate remain host
only. Previously exposed old tokens stay in private tmpfs until cleanup. Successful
revoke acknowledgements are retained separately so retries skip acknowledged tokens.
HTTP error, redirect or lost acknowledgement is UNKNOWN, not assumed success;
tokens/VM/data remain for diagnosis. An already expired/revoked token returning403
is also not falsely labelled a newly acknowledged revoke. This conservative case
may require operator resolution rather than an automatic green finish.

Native TaskEvents `lima task_token_revoked` AND `lima vm_stopped` are required by the
Vault-enabled coordinator before reporting successful finish, bound to the same
allocation attempt. Terminal allocation alone is insufficient. Unknown outcomes
are queryable without resending a model prompt or recreating a VM.

Bao2.5.4 token_file mode also renews the same token; Nomad remains sole issuer/auth
owner, not literally the only process issuing renewal requests. Replacement is
read at reauthentication after old-token renewal failure, not immediately by a
file notification. restart_on_secret_changes=never avoids restarting Prime.
Stopping Nomad's renewal loop alone does not revoke a token kept alive by Bao.
Finite maxTTL1h and owned-process stop/self-revoke are therefore required.
Revoke does not erase a static provider key already in Prime memory. Guest root
can read it; copying that provider key requires its separate rotation.
Tmpfs does not promise immunity to guest/host swap persistence.

## Evidence and activation gate

- 25 Python unit tests; driver/Lima Go race suite and vet pass.
- Official Bao2.5.4 + Prime0.10.1 socket fixture: clean ACP, real synthetic provider
  authentication, end_turn, no persisted runtime key; zero paid requests.
- New v2 plugin fixture: real Prime/acpx, MOCK Bao/provider/TLS revoke endpoint.
  create/submit/watch/finish, stable session header, two-token cleanup, partial
  revoke failure then retry of only unacknowledged work, controller death rejects
  prompt, one-shot owned cleanup, native logs. No business secret or new VM.
- Upstream token-file fixture confirms renewal/replacement/no child restart.

Refresh existing dependency-complete disk with ONLY these three accepted assets;
publish a distinct v2 disk/catalog version. Revalidate sealing/boot identity and
actual native task logs/cleanup events with the sole deployment executor. Actual
Nomad2.0.7/OpenBao2.5.4 login/renewal/revoke and real gateway acceptance remain live
gates; this source checkpoint does not claim those tests have run.


## V3 readiness correction

Official Bao2.5.4/Prime0.10.1 synthetic Linux regression reproduced v2's 30-second
readiness timeout despite a live daemon/listening socket and successful synthetic
lookup/secret fetch. The main Bao TID had no children; another TID owned Prime.
V3 enumerates all TIDs, checks each child parent TGID plus PID/start ticks, and
requires the expected listening socket kernel inode to be an FD of that exact
Prime daemon. No process-name-only acceptance or weakened owner guard is used.
`/proc/stat` comm is treated as bytes, so a non-UTF8 process name cannot break
identity parsing; malformed identity remains unowned.

On readiness failure, `runtime-readiness-diagnostics.json` (0600) and native
`prime_daemon_readiness_failed` stdout record only booleans/counters, safe reason
code and supervisor exit code: token file metadata, socket metadata and daemon
ownership. No argv, environment, token/key value or raw supervisor output is
copied into that record. Cleanup uses the same all-thread ownership proof. A still-listening socket without
a confirmed owned process prevents the stopped acknowledgement; no unknown PID
is signalled.

The official fixture uses only loopback synthetic auth/secret responses and sends
no model request; it checks readiness, socket rejection for wrong PID/supervisor,
owned-process cleanup and safe diagnostics for a failing child. A separate
existing mocked-provider lifecycle regression covers submit/watch/finish/revoke.
A refreshed, sealed image and no-model production acceptance remain deployment
gates; source tests do not establish production/model readiness.
