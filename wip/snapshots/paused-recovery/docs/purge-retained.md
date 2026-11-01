# Addressed retained-disk purge

This is a one-shot administrative command in the same Nomad driver binary. It
never creates, stops, starts, force-kills or unprotects a VM. The operator must
explicitly approve one exact instance/key; no wildcard or directory-wide delete.
Deployment is the sole live executor. It must use the approved target manifest,
not infer targets from a directory scan. Source tests do not authorize live purge.

For the already approved stopped test disks, use the existing Mac controller's
separate SSH channels. No operator-to-runtime SSH, new key or new service:

```sh
python3 tools/recover-failed-create.py \
  --operation purge-retained --confirm-purge \
  --instance "$APPROVED_INSTANCE" --key "$APPROVED_KEY" \
  --ledger-sha256 "$APPROVED_ORIGINAL_LEDGER_SHA256" --allow-gc-404 \
  --allocation "$ORIGINAL_ALLOCATION_ID" --node "$VERIFIED_NODE_ID" \
  --namespace "$ORIGINAL_NAMESPACE" \
  --runtime-host "$EXISTING_RUNTIME_SSH_TARGET" \
  --operator-host "$EXISTING_OPERATOR_SSH_TARGET" \
  --ssh-identity "$EXISTING_CONTROLLER_SSH_KEY" \
  --driver "$IMMUTABLE_REMOTE_MAINTENANCE_BINARY" \
  --state-dir "$EXISTING_DRIVER_STATE_DIR" --lima-home "$EXISTING_LIMA_HOME" \
  --runtime-user "$EXISTING_RUNTIME_USER" --limactl "$EXISTING_LIMACTL" \
  --operator-python "$EXISTING_OPERATOR_PYTHON" \
  --operator-address "$EXISTING_NOMAD_HTTPS_ORIGIN" \
  --operator-token-file "$EXISTING_OPERATOR_AUTH_REFERENCE" \
  --operator-token-format json
```

Add `--job` only when the original job is known; do not fabricate a missing job.
For a still-retained allocation, supply its exact job and use normal terminal
verification. `--runtime-sudo` only uses already existing `sudo -n` permission.
The maintenance binary need not replace the running Nomad plugin or restart any
service. It issues exactly `limactl delete --tty=false INSTANCE`, without `--force`.
Official Lima2.2.1 refuses a non-stopped or protected instance in its delete path:
https://github.com/lima-vm/lima/blob/v2.2.1/pkg/instance/delete.go

## Fresh evidence and GC

Both task and node-admission locks remain held. Every observation is nonce,
sequence, target and deadline bound through the existing authenticated controller;
this is not cryptographic attestation. Two real observation rounds are required.
The normal path requires the exact terminal allocation/task. A plain 404 alone
never proves termination. The explicit GC path additionally requires:

- Exact approved original ledger SHA, matching destroyed retained intent, exact
  instance and allocation, and existing private state owner.
- A real HTTP404 from the allocation endpoint, HTTPS verified, not parsed error
  text. HTTP401/403, redirects, non-JSON errors and unreadable evidence fail closed.
- Fresh token-self metadata confirming the existing management identity (so the
  node list cannot be namespace-filtered), HTTP200 matching node and HTTP200
  node-allocation reads, no active allocations,
  and no contradictory retained row for that supposedly GC'd allocation.
- Locally stopped QEMU metadata, no driver/host-agent PID, matching driver-owner,
  task-key, CPU/RAM/disk and config hash; matching directory inode and runtime UID,
  no symlink layout, no matching runtime process, not protected.

The status-aware reader uses the existing management auth reference only **inside
the operator host**. It emits status fields/counts only. Token value, job spec,
task env, cookies and raw errors never leave that host. No new grant is created.

## Audit, unknown outcomes and quota

A fsynced `<key>.purge-intent.audit` preserves original ledger/resources, directory
identity, configuration hash and proof binding before `purge_pending` is written
and delete dispatched. Base/cache/config paths outside that instance are not
passed to delete. Completion requires official list absence, missing instance
directory/disk and no matching runtime process, then a fsynced completion audit.

Only then does the ledger become `destroyed_disk_purged`: actual retained
`DiskGiB=0`, original size remains in the intent audit and `PurgedDiskGiB`. Existing
running driver versions therefore release the old retained-disk quota without a
client restart. New code also verifies the audit before treating that state as
purged. No configuration or quota limit is raised.

If delete fails or its outcome is unknown, the reservation stays `purge_pending`.
A repeat revalidates the same original approval. Confirmed absence completes
without another delete. An unchanged, fully owned/stopped instance can be retried
only after fresh proofs; partial/replaced/unreadable layout remains blocked.
Repeated completed purge returns the same receipt. Losing the final response
means unknown outcome, not permission to delete a different target.

Only cooperating driver/maintenance operations share these locks. Do not operate
on the target through another external limactl process concurrently. This command
is not an automatic retention policy and does not purge catalog/base images.
