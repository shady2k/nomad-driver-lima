# Exec and managed operations use one runner

New `exec --exec-id ID --timeout SECONDS -- argv…` uses `operations.start/wait/logs` and the existing finite guest supervisor, journal and process guards. There is no second guest protocol or new daemon. ID is also the operation ID. The command deadline defaults to operations.json `default_timeout_seconds`; foreground observation is bounded by `foreground_timeout_seconds`. The latter never kills the workload.

CLI exit 0 means confirmed successful completion; exit 2 means failed/unknown; exit 3 means the foreground observation ended while the command continues (use its ID, never resubmit). A completed response contains actual exit_code and redacted stdout/stderr. A running response contains operation_id, operation_continues and next_action. Use `exec-status --exec-id ID`, or `operation-status/logs/wait --operation ID`. A caller disconnect or wait interruption does not cancel work. Reconnect reads the same durable state; changing command content under the same ID is rejected. Explicit operation-cancel uses the existing machine/boot/process guards. Terminal cancelled/timed_out commands may have partial effects.

If the start response itself is lost, status reconciles the existing guest journal. If no durable guest intent can be established, outcome remains unknown and the command is never resent automatically. Read status before new work. Only running or terminal observations can clear that uncertainty; genuine unknown work still fences destructive finish.

Historical exec-*.json IDs retain their original fingerprint and lookup-only semantics. The explicit reviewed legacy acknowledgment remains available; it is not applied automatically.

New explicit owner-checked command invocations update the existing report-only lease using the command ID as replay-resistant event ID. Status/log/wait/reconnect do not extend it. This is activity evidence, not authenticated originating-agent liveness: owner_state remains owner_unconfirmed, and automatic cleanup remains disabled. No generation handoff or expired lease replacement is performed.

Caller-only source change. Existing guest v5/native operation protocol is reused; no driver or image update is needed. Do not reinstall or resume an active user session without coordinated approval. Repository-profile integration remains a separate branch and is not part of this release.
