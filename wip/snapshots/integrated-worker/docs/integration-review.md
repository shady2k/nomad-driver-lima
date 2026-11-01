# Native worker integration review (source candidate only)

This isolated branch combines repository profiles 173919d, retained resume a247803,
stopped resource change ebe6b75, and corrected durable exec 9fe7cc1. Caller version
0.1.19 is a review candidate, not an installed release. Main and production remain
unchanged. No paid model calls or VM acceptance are performed by this build.

Integration corrections:
- Preserve both resume/resource-change and repository task config/schema fields,
  fingerprint attributes and capacity metadata.
- New disabled-profile records remain schema 1 with unchanged omitted JSON fields.
  Profile-enabled records use schema 2; old binaries fail closed on reading these.
- Resume preserves the exact source schema and pinned profile, independent of a
  changed operator default. Explicit profile changes, schema downgrade and profile
  tampering fail. Successor immutable binding includes the profile snapshot.
- Python resume preserves explicit profile ID/digest. The driver remains the
  authoritative source for implicit default profiles stored in its ledger.
- Boot-specific profile application runs after identity/resource verification and
  before credentials/workload, including a resumed boot. Recovery of an already
  running allocation reads its snapshot without reapplying/relaunching.
- Durable exec retains the existing operation runner and transport contract.
  CLI 0 means completed success, 2 failure/unknown, 3 still running after observation.
  Existing legacy exec records remain lookup-only; lease renewal is explicit
  owner-checked command activity, never status polling/authenticated liveness.

Tests include schema 1/2 resume projection/reconciliation, profile tamper/downgrade,
full fake-CLI stop-to-resume with preserved profile despite disabled new default,
new boot application, plus each component's full existing suite and race fixtures.
The fake repository adapter now checks the actual per-boot fake identity rather
than a pre-resume fixture's fixed boot string. Sanitizer symlink refusal keeps the
established error category and still occurs before mutation.

The build tool emits Linux amd64 and macOS arm64 binaries, caller/source archives,
and exact checksums, baseline comparison and actual source branch. The existing
guest release declaration is historical and may not match current task-main source;
report the mismatch without claiming an image rebuild or modifying the live image.
Task-main/adapter/launcher bytes are unchanged from baseline 7bbdfed. New repository
helpers are embedded first-party code delivered through the existing driver path.

Runtime gates: coordinated deployment review; active-user VM/allocations check;
private profile file + checksum via existing Ansible; empty-node restart only when
approved; one owned no-model native acceptance of profile, stop/resume/resources,
then caller compatibility. No automatic migration, no downgrade once schema 2 exists,
no changes to Mac live workloads. Current approval is source/testing only.

Public provenance/connection-info remains a separate user-requested work item; its
uncommitted main-worktree edits are deliberately excluded from this integration.
