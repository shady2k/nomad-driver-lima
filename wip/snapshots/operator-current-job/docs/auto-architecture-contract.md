# Optional architecture and late platform resolution — source contract

Implemented independently: capacity now reports ALL visible nodes, including nodes
without Lima, unhealthy/ineligible nodes and unavailable detail/usage reads. Every
row has placement_eligible and explicit reasons; unknown guest capacity is null,
not a zero-capacity or working-Lima claim. Resource figures stay per node, never a
sum of RAM/vCPU across architectures. Request planner excludes ineligible rows.
No installed caller, Mac runtime, image or scheduling policy changed.

Source implementation (not installed): omitted --architecture defaults to auto;
explicit amd64/arm64 keeps the existing native-platform filter. Auto requires node
fingerprints driver.lima.catalog.late_platform_selection=true and exact
catalog.sha256. Old drivers cannot receive an auto request. Mac also needs healthy
Lima AND an accepted matching ARM64 boot artifact. No live Mac enablement occurred.

Read-only request fit uses the SAME envelope as auto dispatch:
`capacity --template ID --template-version VERSION --vcpus 2 --memory-mib 4096
--disk-gib 8` (optionally --architecture). Response includes per-node fit, conservative
CPU/MiB reservation and overreservation_possible=true. It is not a reservation or
new quota. Generic capacity remains a per-node snapshot. Pinned-catalog mismatch or
unknown disk/CPU telemetry excludes the node from auto; it remains visible.

Task fields: template_catalog_sha256 plus template_variants_json (bounded, strict
pins: architecture, guest_os, vm_type, disk_sha256, yaml_sha256, minimum_disk_gib).
No paths/URLs are accepted there. Original architecture and disk SHA are unset for
auto; actual selection is stored under ledger lock and emitted in native TaskEvent
Details and stdout. Caller waits for selected/running allocation before binding;
resolved_template and placement_attempt are immutable in target config and receipts.
A changed allocation/restart/platform refuses rebind. Recover validates pinned
selection and never creates another VM. A catalog-pin change fails closed; retain
the pinned catalog configuration while its allocations must recover.

0.1.7 freeze is independent and contains no auto-resolver. Current development
manifest is 0.1.8; driver and caller must be coordinated before installing it.
Guest assets/images are unchanged. Source tests use fakeCLI only; real cross-node
acceptance and ARM preparation are not claimed.

Implemented contract:

1. Request semantics: architecture null/auto means no user architecture restriction;
   explicit amd64/arm64 is an exact filter. Template ID/version and catalog SHA stay
   exact. From that catalog derive accepted variants with arch, guest OS, vm_type,
   disk SHA/size/minimum; reject duplicate platform tuples, unaccepted or too-large
   minimum disks. Pending capacity is advisory, not permission or reservation.
2. Durable create intent hashes original requested resources, optional architecture,
   catalog SHA and the sorted immutable candidate set. Do NOT hash a volatile free
   snapshot or preselected node into the request identity. Lookup intent BEFORE
   recomputing capacity on retry. Lost submit response => same job lookup, never
   another job. This also fixes the existing planner's early volatile recomputation.
3. Single Nomad job: candidate-node constraint limited to compatible currently
   supported nodes, no architecture equality constraint for auto. NodeIDs are an
   immutable observed eligibility set for this request, not a default pinned host.
   Scheduler places one allocation, not one job per architecture. Use a conservative
   native CPU reservation across nodes: CPUs * maximum Nomad compute
   per core plus maximum per-VM memory overhead. This may underutilize a lower-MHz
   node; don't promise all apparent free vCPUs fit. No unsupported dynamic Cores
   reservation or cross-node memory pooling. Report reservation/fit explicitly.
4. Driver TaskConfig gains candidate variants + catalog pin and optional requested
   architecture; single-disk field is unset ONLY in this new mode. At StartTask on
   the assigned node, load configured pinned catalog and match actual host OS/arch/
   vm_type. Require exactly one pinned candidate and matching SHA. Validate every
   resource, physical disk and authoritative quota BEFORE create. Old explicit
   task config remains supported; no trusting caller-provided paths or URLs.
5. Under existing ledger lock persist resolved platform, actual disk SHA, catalog
   SHA, allocation/task/request identity and reservation BEFORE side effects.
   Recover uses this exact selection; catalog drift or changed host identity fails
   closed. Never switch architecture/image on the same started allocation. Nomad
   restarts/reschedules remain disabled; no duplicate exec or prompt replay.
6. Driver native TaskEvent/readiness exposes resolved selection. Caller binds
   target config only after verifying allocation owner + resolved metadata against
   the original candidate set. Job meta records requested architecture/candidates,
   not a fabricated single disk before placement. Result receipts bind actual
   platform/disk/attempt. Recovery and exports preserve that binding.

Targeted gates before activation: unset/explicit filters; busy amd64 + ready accepted
arm64 candidate; no Lima/no ARM image stays excluded; different per-core units and
memory overhead; min disk; changed catalog/checksum; lost create response without
replan/re-submit; concurrent start/recover same selected SHA; changed placement or
stale attempt rejected. Fake CLI/scheduler and driver unit tests first. Live Mac
installation/restart, ARM image build and real cross-node acceptance remain separate
approved deployment steps. This document does not authorize them.

Rollout must install the driver capability before the auto caller; an old driver
or node catalog mismatch stays excluded. For independent 0.1.7 visibility rollout,
use installer/verification files from the frozen 0.1.7 archive, not this newer
working tree (which verifies placement.py for 0.1.8). No running session reload is
implicit. The conservative envelope can reject an otherwise physically possible
placement when calibration differs greatly; report per-node fit rather than
claiming physical totals are all usable by this request. No new hard quota exists.
