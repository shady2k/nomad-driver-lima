# Cancelling one owned ACP request

Caller 0.1.43 adds:

```sh
nomad-workers --config CONFIG vm cancel VM_ID
nomad-workers --config CONFIG vm cancel-status VM_ID
```

The VM facade resolves the current owned VM/attempt and its durable task receipt.
One unfinished task is selected; ambiguous tasks require explicit `--request ID`.
It never guesses the newest request from a timestamp or a project name. The native
hook supplies existing initiator identity; no new credentials or network access
are involved. The legacy exact interface remains available:

```sh
nomad-workers --config TARGET request-cancel --session SESSION --request REQUEST
nomad-workers --config TARGET request-cancel-status --session SESSION --request REQUEST
```

Cancellation/status never scans the transcript or full session-history registry;
they use the private bound create/turn receipts and the native queue owner.
A transcript limit, malformed history or absent history registry therefore cannot
prevent an otherwise valid exact-request cancellation.

The caller validates current native owner/generation, Nomad allocation/attempt,
private session/record, prompt digest and native request mapping. A read-only guest
preflight binds machine/boot identity. The guest verifies it again immediately
before control dispatch. A supported legacy guest turn without session_id still
uses the existing explicit prompt/session/record binding; absence is not a guessed
native request ID.

Both caller and guest save a private cancellation intent before the mutating RPC.
The pinned acpx 0.21.0 `tryCancelOnRunningOwner` sends `cancel_prompt` with exact
`targetRequestId` and native owner generation to the existing queue owner. A
matching queued request is removed, or the matching active request receives the
runtime's cancellation. Another active request is not cancelled. No `ensure`,
`startTurn`, daemon recreation, session-wide CLI cancel, prompt replay, VM signal,
process-group kill, token revoke or new model request is used.

`cancel_accepted` is an ACK, **not a terminal result**. `cancel_is_terminal_proof`
is always false. Read/watch the same request to observe an actual completed,
cancelled or failed result. Tool/background-process cleanup follows that runtime's
own behavior; an ACK cannot prove every arbitrary descendant is gone.

`target_not_cancelled` means the owner did not cancel that exact target; it does
not prove completion. `owner_unavailable` does not prove a lost/cancelled request.
A lost transport/ACK yields `cancel_unknown`. Repeating cancel for the same request
is lookup-only on both sides, including concurrent native calls or a crash after
intent persistence but before dispatch. It never silently resends cancellation
under a new operation UUID. `cancel-status` may recover a guest ACK or independently
retained terminal result. If the outcome remains unknown, preserve it and observe
that existing request; do not clear receipts or submit a replacement task.

Cancel exits 0 for accepted/no-op terminal evidence and 3 for not-cancelled,
unavailable or unknown outcomes; the JSON phase describes which. Status commands
return observation data without dispatching cancellation. A terminal result that
was already retained makes cancel a no-op. `watch` records terminal state in the
ordinary existing receipt path; history remains read-only with respect to it.

Status/history/transcript and operation status/logs remain available regardless
of unfinished/unknown work or a pending cancellation. A cancel receipt lives in
its own subdirectory and does not create a global activity gate. Ownership and
attempt checks still apply. New prompts, model changes, snapshot/finish and
arbitrary exec retain their existing conflicting-work guards; this is not an
unrestricted shell bypass.

A quiet stream or reader timeout is not a reason to assume the model is stuck.
Inspect status/history first. When stopping your own task is intended, use this
explicit cancel action instead of killing watch/Prime/Bao or restarting the VM.
No current live user request is cancelled as part of installing/testing this
feature. No driver or guest-image upgrade is required: the helper is delivered
by the caller over existing native Nomad exec.
