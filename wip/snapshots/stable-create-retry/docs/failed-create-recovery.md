# Recover a proven pre-create cache permission failure

`nomad-driver-lima recover-absent-create` is an explicit operator operation,
not an automatic retry/reconciliation policy. Linux only: the absence proof uses
`/proc` and the configured unprivileged runtime identity. It does not stop, delete,
create or restart any VM, allocation or service. It never sends a Prime request.

Use the same existing state directory, Lima home and runtime user as the driver.
The operator must already have read access to the exact Nomad allocation. The
command uses the official Nomad CLI's existing authentication; it creates no
credentials and passes none to Lima. API errors and non-JSON exit-zero responses
fail closed. Do not relax ACLs to make a recovery succeed.

```sh
/path/to/immutable/nomad-driver-lima recover-absent-create \
  --state-dir "$DRIVER_STATE_DIR" \
  --lima-home "$LIMA_HOME" \
  --limactl /absolute/path/to/limactl \
  --runtime-user "$RUNTIME_USER" \
  --nomad /absolute/path/to/nomad \
  --key "$FAILED_LEDGER_KEY" \
  --allocation "$FAILED_ALLOCATION_ID" \
  --node "$EXPECTED_NODE_ID" \
  --job "$EXPECTED_JOB_ID" \
  --namespace "$EXPECTED_NAMESPACE" \
  --failure-log "$DRIVER_STATE_DIR/diagnostics/create-$TIMESTAMP.stderr"
```

The narrowly supported case is a `creating` ledger with no machine ID, boot ID,
configuration hash, start/completion time, exit or finalization evidence, and a
private existing driver diagnostic recording an exact `_catalog-cache/<sha>.yaml`
open permission failure. Other create failures remain uncertain. The diagnostic
and ledger are not deleted.

While holding both the task lock and node admission lock, the operation:

1. Reads the exact allocation using explicit `GET`. Requires matching allocation,
   node, job, namespace and task; failed/dead, never started, no restarts or next
   allocation. It rejects observed Started/restart events.
2. Uses the runtime UID's official `limactl list --json`, checks the instance
   directory is completely absent (therefore no partial disk), and inspects
   runtime UID process command lines for that instance/path. It never reads
   process environment. Any ambiguity or inability to inspect retains reservation.
3. Repeats allocation/runtime checks. A changed allocation index or proof taking
   more than 30 seconds fails closed.
4. Writes and fsyncs `<key>.failed-create.audit` (0600), containing the original
   failed intent and hashes of its diagnostic, before committing the ledger to
   `no_vm_failed` with the audit hash. This releases only that unused reservation.

The original task cannot be replayed: `StartTask` rejects its `no_vm_failed`
intent. A separately authorized new allocation is a distinct operation. A repeated
recovery returns the same audit without repeating runtime actions. A crash after
an intact audit but before the ledger commit requires fresh absence checks and
reuses that audit; conflicting/partial evidence requires operator investigation.
No code path unlinks artifacts or purges disks.

The locks serialize cooperating driver operations, not arbitrary external
`limactl` users. Do not concurrently operate on the targeted instance outside the
driver. This is an administrative repair for one proven never-created attempt,
not a general recovery mechanism for missing, previously running VMs.


## Operator credential stays on a different host

When the runtime has no allocation-read identity, use the one-shot controller
`tools/recover-failed-create.py` from the already authorized controller machine.
It opens separate existing SSH connections to the runtime and operator. It does
not assume operator-to-runtime SSH and does not provision keys or known_hosts.
Strict host-key checking, batch authentication and disabled agent forwarding are
mandatory. Pass the same existing identity/config used by deployment, if needed.

```sh
python3 tools/recover-failed-create.py \
  --runtime-host "$EXISTING_RUNTIME_SSH_TARGET" \
  --operator-host "$EXISTING_OPERATOR_SSH_TARGET" \
  --ssh-identity "$EXISTING_CONTROLLER_KEY_PATH" \
  --driver "$IMMUTABLE_REMOTE_DRIVER_BINARY" \
  --state-dir "$DRIVER_STATE_DIR" --lima-home "$LIMA_HOME" \
  --runtime-user "$RUNTIME_USER" --limactl "$EXISTING_LIMACTL" \
  --key "$FAILED_LEDGER_KEY" --allocation "$FAILED_ALLOCATION_ID" \
  --node "$EXPECTED_NODE_ID" --job "$EXPECTED_JOB_ID" \
  --namespace "$EXPECTED_NAMESPACE" \
  --failure-log "$EXACT_EXISTING_CREATE_STDERR" \
  --operator-cli "$EXISTING_AUTHENTICATED_NOMAD_CLI" \
  --operator-python "$EXISTING_OPERATOR_PYTHON"
```

`--runtime-sudo` uses existing passwordless `sudo -n` only when deployment already
uses it; it grants nothing. `--ssh-config` optionally selects an existing config.
The operator CLI may be the existing authenticated operator wrapper; its token
stays on that host. No fake Nomad CLI or saved allocation JSON is used.

The remote driver runs `recover-absent-create --operator-stdio`; this mode excludes
`--nomad`. While holding both locks it issues two independent unpredictable nonce
challenges, sequence 1/2, each bound to the exact target and an eight-second
response deadline. Each challenge triggers a new real GET on the operator host.
That host strips Job, task Env, event text and every other non-status field before
sending its response. The runtime rejects replay, wrong identity/sequence,
unknown/oversized payloads, expired observations and changed allocation index.
An authentication failure has no fallback and triggers no grant creation.

This is trust in the existing authenticated SSH controller, **not cryptographic
attestation that a nonce proves a real API read**. The reviewed controller makes
the real reads. Runtime absence checks, lock scope and audit/commit semantics are
identical to local verification. There is no daemon or background retry.

An EOF/timeout during verification leaves the original ledger intact. If the
connection is lost *after* the second valid response and commit, final delivery
can be unknown: do not roll back or create/replay a task. Inspect/repeat the same
addressed recovery; a completed audit returns idempotently without another commit.
No transport can promise that a lost completion response means no mutation.
