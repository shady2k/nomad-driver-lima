# Checkpoint export and finalization

Export is an intermediate checkpoint. It never grants task completion or closes a
VM. After user acceptance, the same prepared VM may continue with new model turns
or guest operations. Each NEW submit/exec/managed-operation intent reserves a
monotonic work revision under the existing session lock, before dispatch. Duplicate
IDs are readback only. The durable intent receipt is the revision journal; recovery
can derive the revision even if the later session-pointer write was interrupted.

A completed export is immutable and remains readable using `result`. New work
invalidates its usefulness as a closing candidate, not its historical checksums or
receipt. Upload-failed/frozen snapshots must finish their transfer before new work;
unknown commands/turns stay unknown. Known running work blocks a quiescent export.

To export again WITHOUT another model request, choose a new snapshot UUID bound to
an already terminal request. `--snapshot` defaults to the request UUID for the first
legacy-style export. Never reuse a snapshot UUID with changed files or revision.

```
# Preparation checkpoint, followed by explicit user acceptance and later work:
nomad-workers --config TARGET collect --session SESSION --request REQUEST \
  --snapshot PREP_SNAPSHOT recipe.json checks.json
# After later work, capture fresh results without resending the model prompt:
nomad-workers --config TARGET collect --session SESSION --request REQUEST \
  --snapshot FINAL_SNAPSHOT result.patch report.json
nomad-workers --config TARGET result --session SESSION --request REQUEST --snapshot PREP_SNAPSHOT
nomad-workers --config TARGET finish --session SESSION --request REQUEST --snapshot FINAL_SNAPSHOT
```

The S3 receipt's request-ID path is the snapshot UUID; the bound checkpoint metadata
retains the actual model request UUID, activity revision and allocation attempt.
Both use the existing authorized request-prefix/commit protocol, not a new store or
grant. Diagnostic indexes carry the same revision. Historical object restore uses
the receipt snapshot prefix, not the model request prefix.

`finish` verifies that all turns/operations and exports are settled, checks receipt
readback and current revision, then durably records ONE closing candidate and moves
to `finalizing` BEFORE remote close actions. That state rejects new activity. An
explicit retry may resume only the same snapshot/hash/revision using idempotent
prepare/close/ack steps; it never repeats a prompt or selects a different receipt.
Unknown stop/cleanup remains pending rather than being called success. Retained
files/results are not removed. Legacy receipts lacking revision metadata stay
readable but need a fresh snapshot before they can close a VM.

This is a source-only correction until a coordinated plugin release. Do not
silently update the active caller or mutate its current VM. Template drafts may
select their preparation receipt with `prepare-template --snapshot UUID`.
