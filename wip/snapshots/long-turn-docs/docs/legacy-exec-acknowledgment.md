# Acknowledge a legacy read-only exec with an unknown outcome

The legacy foreground exec has no guest PID/result journal. Its result cannot be recovered or cancelled reliably. This operation preserves that fact; it neither retries the command nor invents an exit code. Use managed operations for subsequent long commands.

Run the frozen caller as the existing caller UID with its existing configuration and creator record. No Claude resume, plugin reinstall, driver restart or guest image change is required. Do not create a new owner generation.

```
python3 plugin/scripts/workerctl.py --config CONFIG exec-status --session SESSION --exec-id EXEC
python3 plugin/scripts/workerctl.py --config CONFIG exec-abandon-plan --session SESSION --exec-id EXEC
python3 plugin/scripts/workerctl.py --config CONFIG exec-abandon --session SESSION --exec-id EXEC --approval-file APPROVAL
```

Save the plan to a private mode-0600 approval file. The plan is NOT approval: its four confirmation booleans default to false. An authorized operator must explicitly confirm that the exact command was read-only, related processes were observed absent, its outcome remains unknown, and new activity is permitted. This is trusted operator evidence, not cryptographic attestation. The command digest must match the separately inspected original command. No automatic inference from missing output is permitted.

The action rechecks current creator/session/generation, running allocation/attempt, and current machine/boot identity against a fresh approval. The legacy original boot identity is unavailable and is explicitly marked unrecorded. Approval expires after five minutes. A separate expired owner lease is not renewed or replaced.

Under the existing session lock, an audit containing the original unknown receipt and approval is atomically persisted before changing the activity to `exec_abandoned`. The original outcome remains `unknown`; replay and cancellation remain false. The audit increases the activity revision and invalidates older result checkpoints. A crash before the receipt update remains blocked and can retry the same approved acknowledgment. Repeating the old exec ID remains lookup-only.

This is restricted maintenance for an explicitly reviewed read-only legacy command, not a generic unknown-command bypass. Active/ambiguous commands require investigation. No live action is included in this source release.
