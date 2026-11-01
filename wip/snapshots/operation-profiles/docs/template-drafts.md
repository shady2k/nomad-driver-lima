# Template preparation: review packet, not publication

This metadata-only command does not capture, seal or stop a VM, run a command/model,
build a disk, upload to S3 or write the catalog. Installation is coordinated
separately; an active caller is not silently upgraded.

After preparation and its authorized synthetic model smoke, collect the explicit
`recipe.json` and `checks.json` paths through the existing result/manifest contract.
The draft reuses verified artifact SHA/size and configured receipt readback. A
coordinator-local receipt remains local evidence; it is not presented as an S3
backup or independent image acceptance.

```
nomad-workers --config TARGET prepare-template --session SESSION_UUID \
  --draft DRAFT_UUID --template tools --template-version v1 \
  --request SMOKE_REQUEST_UUID --snapshot PREP_SNAPSHOT_UUID --recipe-path recipe.json --checks-path checks.json
nomad-workers --config TARGET template-draft --session SESSION_UUID --draft DRAFT_UUID
```

Recipe schema 1 has exactly `purpose`, `packages`, `steps`, `schema_version`.
Packages contain manager/name/pinned version. Steps contain a purpose label and
argv; they are recorded, never executed. Credential references, private keys and
bearer/signed URLs are rejected. The agent must supply dependency-only steps, not
project checkout/data or installation-specific credentials. Automated screening
and an agent report do not replace operator review.

Checks schema 1 has `environment_ready`, checks (name/status/version), and
`model_smoke` (status/request_id/provider/model/usage). Its request ID must match the
verified result receipt. A claimed passing smoke must have terminal `end_turn`;
that is not a claim that the user's project task finished. Blocked/failed checks
produce a non-ready review packet. Reported versions/usage are explicitly reported
evidence, not provider attestation.

The packet is private, immutable and bound to original owner/generation, allocation
attempt, config hash, source result manifest and both artifact hashes. Reusing its
ID with different inputs is rejected. Existing catalog versions cannot be
replaced. Disk output checksum, exported size and validated minimum size stay
unknown until the operator build/export acceptance proves them.

The returned SHA identifies the exact review packet for confirmation. There is no
agent `approve`, `seal`, `stop` or `publish` switch. Later operator work reuses
`deploy/base-build.yml` / `base-refresh.yml`, `base-seal.sh` and `base-artifact.yml`:
review recipe → build a clean candidate → seal only the new approved disk → two
independent boots with unique identity → verify actual size/digest/architecture/
minimum disk → explicit confirmation for the exact immutable catalog version.
Stopping an existing source VM, if chosen, needs its own exact approval before
that action. Never seal the live user disk. Keys/tokens, repositories/data, owner
identity and private result/operation journals must not be baked into the image.

Full native operation output still requires the separately versioned schema-2
image. Current v3 guests have compatible named progress stages and
`operation-logs`. Do not bake or replace an image while a user workflow is active
without coordination; published artifacts remain immutable.

## Phase 1 integration

The initial preparation plan must include evaluating reuse and naming a potential
dependency-only project-purpose template, e.g. `example-project-dev`. At successful completion,
the readiness report explicitly offers preparing that versioned draft with the
commands above (or identifies an already accepted exact version to reuse). It must
not end at versions + "waiting for acceptance" without the template option.
Previously requested draft preparation can proceed without asking for the same
non-destructive step again. A failed/blocked check is disclosed as incomplete
evidence; no template readiness claim follows from it.

This fixes permissive wording in0.1.9: "Proposing ... is allowed" did not require
that offer, whereas reporting and STOP were mandatory. The installed0.1.9 skill
matched57126c3 byte-for-byte when inspected. No runtime/template command was run
as part of this instruction correction. Stop/capture/seal/publication permissions
are unchanged; recipes remain dependency-only and exclude project checkout/data,
credentials and originating identity.
