# Same-VM ACP rebind after confirmed cleanup

This finite operation initializes a fresh ACP session against the **existing**
owned runtime. It never sends a prompt, starts/restarts a daemon, kills a process,
changes credentials, recreates a VM, or replays a previous turn. The previous
native records and journals remain immutable. Read/watch select the record from
an explicit per-request binding; new requests use the new current record.

Operator command (private config supplies all infrastructure values):

```sh
python3 plugin/scripts/workerctl.py --config "$TARGET_CONFIG" runtime-rebind \
  --session "$VM_ID" --operation "$RECOVERY_ID" \
  --cleanup-receipt "$CONFIRMED_CLEANUP_RECEIPT_BASENAME"
```

The cleanup receipt must already confirm the supported owned session close, exact
old record and ACP session. The owner must have exited, all existing turns and
managed operations must be terminal, and the current native initiator, Nomad
allocation/attempt, runtime processes, socket inode and model catalog must match.
A caller lock and exclusive guest recovery lock fence concurrent transitions.
The existing one-shot launcher permits only a new ACP transport connected to the
verified daemon; initialize/session-new is not a model request.

An exclusive operation intent is fsynced before session-new. Reusing the same ID
is lookup-only. A lost native result remains `rebind_unknown`; it is never treated
as permission to create another session. A confirmed native result saved before
a pointer-write interruption is reconciled without another session-new.
`binding_pending`/`rebind_unknown` block new work but never read-only history.
Success requires the newly persisted native session and runtime identity checks,
then both guest and caller current pointers are committed. CLI exits 3 while not
ready. Old history IDs are retained explicitly, not inferred from project names.

This command is the post-cleanup portion of the recovery API. It does not grant
an agent permission to close active work, nor does it infer cleanup from a cancel
ACK. The combined cleanup/rebind facade can invoke this same operation after a
completion-bearing close. If native teardown times out again, report the failed
step/unknown result and propose an explicit runtime-only restart; do not kill or
restart implicitly. A runtime-only restart loses ephemeral kernel state while
preserving files/history, and requires separate authorization.

Prime 0.10.1 specifics stay inside the adapter: `stop_failure` is in-memory
HostedSession state in the ACP bridge. Admission checks it before prompts.
Cancel stop executes input-pause acquisition, abort/clear queue, owned-admission
cancellation, idle wait, child cancellation and pause release. Failure stores its
error; successful stop clears it. Supported session-close repeats cleanup and
returns success only after removing the hosted session. acpx 0.21.0 idle cancel
returns false without forwarding, so its ACK is not a recovery proof. Its generic
CLI sessions-close has process-termination fallbacks and is not used here.

Every unconfirmed recovery response is normalized to `rebind_unknown` in the
caller; additionally, activity admission rejects any recovery record unless both
`phase=ready` and `session_ready=true` are present. Unknown future phase names do
not open admission. The guest persists a per-old-record fence before dispatch
and changes its own session phase to `recovering`. A different operation ID (or
a missing caller receipt) cannot create another native session from that cleanup.
The new pointer remains `recovering` until the verified completion record is
fsynced; only then is guest submission reopened. Historical transcript reads do
not depend on this phase and remain available throughout.

The deployed managed-operation root also contains the standard `creation.lock`
file. It is validated as a private, owned, non-symlink regular file, not treated
as an operation directory. All other entries must remain valid operation UUIDs.
`--retry-pre-effect` is an explicit same-ID continuation for a prior rejection:
only an absent guest intent AND absent per-record fence with the unchanged ready
old binding can reach session-new. Existing guest intents are always lookup-only,
even with this flag. No caller/guest journal or lock is removed to enable retry.
