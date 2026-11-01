# Native disconnect grace for future worker jobs (review caller 0.1.23)

This separate patch adds the approved initial 30-minute grace to newly generated
worker jobs. It does not modify existing jobs, allocations, bindings or failed
requests; it does not restart a client or VM. The frozen profile release 0.1.22
is unchanged. Its driver can be used with this caller; there is no new driver ABI.

```hcl
disconnect {
  lost_after = "30m"
  replace = false
  reconcile = "keep_original"
}
```

The native API uses LostAfter=1800000000000 nanoseconds, Replace=false and
Reconcile=keep_original. StopOnClientAfter is intentionally absent, not zero.
The independent RestartPolicy Attempts=0/Mode=fail and ReschedulePolicy
Attempts=0/Unlimited=false remain unchanged. No background reaper or own service.

During this finite grace, the server can mark an allocation unknown on a disconnected
node and does not create a replacement for that unknown allocation. The client is
not configured to stop it just because the server is unreachable. On a timely
reconnect, reconciliation prefers the original allocation; healthy execution can
continue instead of starting a new disk/session. This does not repair a crashed
process, expired external credential or a permanently lost node.

**After 30 minutes the guarantee ends:** the allocation can become lost. Nomad's
lost/reconnect handling may stop the old task or place a replacement. replace=false
is not a perpetual lost-allocation lock. This policy therefore does not promise
indefinite no-stop/no-replacement. Our driver still retains disks on ordinary task
stop/destroy; no disk purge is added. Any deliberate purge remains a separately
approved maintenance action. A strict indefinite execution contract would require
a different, explicitly reviewed lifecycle rather than an arbitrarily huge duration.

A private operator may provide `disconnect_policy` with the exact keys
lost_after_seconds, replace=false, reconcile=keep_original. Omission uses the approved
1800 seconds. Zero, negative, bool/float durations, unsafe reconcile/replace, null,
unknown fields and a client-stop timeout are rejected. No configuration is installed
by this patch. Existing intent lookup remains lookup-only; no automatic rebind or
prompt/command replay is introduced for the incident's failed jobs.

Verification includes native Nomad 2.0.7+local-resync.1 `job validate` using its local
structural-validator fallback against refused loopback port 1 with an empty token and
temporary HOME, plus `job run -output -json` (no submission) API roundtrip. Invalid
lost_after and conflicting StopOnClientAfter are rejected by that real binary.
Driver configuration cannot be validated without an agent in this fixture; the
output explicitly reports this. No live WAN fault or scheduler test is claimed.

Official pinned sources: [v2.0.7 group validation](https://github.com/hashicorp/nomad/blob/v2.0.7/nomad/structs/group.go),
[v2.0.7 disconnect rescheduling guard](https://github.com/hashicorp/nomad/blob/v2.0.7/scheduler/reconciler/filters.go),
[v2.0.7 heartbeat handling](https://github.com/hashicorp/nomad/blob/v2.0.7/nomad/heartbeat.go),
[v2.0.7 allocation timeout](https://github.com/hashicorp/nomad/blob/v2.0.7/nomad/structs/alloc.go),
and [job-spec semantics](https://developer.hashicorp.com/nomad/docs/job-specification/disconnect).
