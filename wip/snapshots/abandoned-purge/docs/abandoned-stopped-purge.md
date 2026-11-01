# Addressed abandoned stopped-disk purge

This is a source-only workflow for an operator-selected stopped instance, represented here as `INSTANCE_NAME`. Real target identities and any operation-specific approval belong in private operator configuration; no incident-specific targets or approval statements are recorded here.

Prerequisites: existing authenticated controller SSH to the runtime and operator hosts, the operator's existing protected Nomad authorization reference (never copied), an existing absolute `qemu-img`, and the already approved permanent deletion. No new grants are required. A current Job with missing creator metadata, HTTP403, inaccessible backing chain, active allocation/process, dependency, transferred ownership or changed disk fails closed.

Create a private node-side observation JSON (0600) from fresh read-only evidence:

```json
{
  "purpose": "abandoned_stopped_purge",
  "allocation": "<exact-allocation>",
  "instance": "<exact-instance>",
  "session": "<exact-worker-session>",
  "vm_status": "Stopped",
  "disk_retained": true,
  "disk_size_bytes": 123,
  "disk_sha256": "<exact-sha256>",
  "ledger_unchanged": true,
  "ledger_sha256": "<exact-current-ledger-sha256>",
  "revocation": "unconfirmed",
  "approval_reference": "<specific-permanent-delete-approval>",
  "runtime_stop": {"ok": true, "runtime_stopped": true, "revocation": "not_attempted"}
}
```

`runtime_stop` records the verified physically stopped state; it does not claim that this maintenance tool stopped a process. Native Lima must independently report Stopped, both PIDs zero, exact ownership/resources/config, and no matching process. A regular non-symlink disk must match the approved SHA/size. There is no synthetic StopReceipt or invented exit code.

Invoke the **same packaged** controller and standalone binary:

```sh
python3 recover-failed-create.py \
  --operation purge-retained --confirm-purge --abandoned-stopped \
  --runtime-host '<existing-runtime-ssh-profile>' \
  --operator-host '<existing-operator-ssh-profile>' \
  --operator-python '<existing-python>' \
  --operator-address 'https://<nomad-host>' \
  --operator-token-file '<existing-private-auth-reference>' \
  --operator-token-format json --allow-gc-404 \
  --driver '<standalone-reviewed-maintenance-binary>' \
  --state-dir '<existing-state-root>' --lima-home '<dedicated-managed-home>' \
  --runtime-user '<existing-runtime-user>' --limactl '<existing-limactl>' \
  --qemu-img '<existing-qemu-img>' \
  --key '<exact-key>' --instance '<exact-instance>' \
  --allocation '<exact-allocation>' --node '<exact-node>' \
  --job '<exact-job>' --namespace '<exact-namespace>' \
  --ledger-sha256 '<exact-current-ledger-sha256>' \
  --stopped-source-receipt '<private-node-observation-json>' \
  --stopped-source-receipt-sha256 '<exact-observation-sha256>'
```

While exact task and node-admission locks are held, the tool checks other dedicated-home qcow2 backing chains and shared inodes, then requires two fresh operator GET observations of the current Job (Stop=true, Status=dead, matching ID/namespace/creator and unchanged ModifyIndex). If both current Job and allocation were GC404, the same existing management identity must additionally receive HTTP200 namespace job and allocation listings, with neither target job nor allocation referenced. No current-job fields or owner are fabricated; the approved durable ledger and native ownership bind that case. A dead current job with Stop=false still fails and must be stopped through the separately authorized ordinary Nomad operation.

The source allocation must be terminal/dead, or genuinely GC404 with existing management authorization and matching node inventory; authorization errors are never absence. It refuses ResumeNext ownership transfers. Manual/root mutations outside these cooperating locks remain an administrator boundary; pause such mutations during this operation.

Before addressed native `limactl delete`, the original ledger, unconfirmed-revocation observation, current job proof and dependency-verification result are fsynced to the purge audit. After native instance/disk/process absence is verified, completion is fsynced and disk quota released. Unknown delete results retain the reservation; retry exactly the same receipt/key to reconcile absence. Once the fsynced intent exists, retry is lookup-only: a still-present/unknown instance retains quota and requires operator investigation; delete is never redispatched, including a crash after intent fsync but before dispatch. Never issue a new broad delete. Other VMs are read-only inventory and are not stopped or altered.

**Deleting a disk does not revoke an OpenBao token or a copied model credential.** This tool makes no issuer call and preserves `revocation=unconfirmed` in audit. Issuer revocation/status remains a separate exact-accessor, existing-authority operation if needed; token/accessor enumeration or 403 alone cannot substitute for proof. In these stopped OOM guests, volatile guest tmpfs/processes are already absent. Do not export or publish their data as a sanitized template.
