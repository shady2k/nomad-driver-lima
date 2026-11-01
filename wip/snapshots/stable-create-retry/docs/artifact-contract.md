# Prepared boot artifact contract v1

Operator pins the catalog JSON with `catalog_file` and `catalog_sha256` in driver
config. It has `schema_version: 1`, `templates: [...]`. Each entry:

```json
{
  "template_id": "agent-base", "version": "v1", "guest_os": "linux",
  "architecture": "amd64", "vm_type": "qemu",
  "disk": {"url": "file:///operator/cache/agent-base.qcow2", "sha256": "64 lowercase hex", "size_bytes": 1, "virtual_size_bytes": 21474836480, "minimum_disk_gib": 20, "format": "qcow2"},
  "runtime_yaml": {"url": "file:///operator/cache/runtime.yaml", "sha256": "64 lowercase hex", "size_bytes": 1},
  "tools": {"prime": "0.10.1", "acpx": "0.21.0"},
  "acceptance": {"sealed": true, "independent_boots": 2, "unique_identities": true}
}
```

Numbers/digests above are placeholders, not acceptance evidence. `url` is an absolute
file URL or HTTPS read URL (including already authorized temporary S3 read URL).
Credentials/URL queries are never logged. No redirect. URLs can be refreshed by a new
operator-pinned catalog; existing task spec pins artifact hashes and version, not
an expiring URL. Worker cannot edit catalog, publish templates or select arbitrary URL.
Cache is digest-addressed and rehashes before use; failed/partial downloads never
become a cache hit. No eviction is implemented, so live bases are not evicted.

Runtime YAML is a **minimal boot configuration**, not the preparation recipe.
It must omit `base`, `provision`, `mounts`, `additionalDisks`, `portForwards`,
`networks`, `copyToHost`, `hostResolver`, `env`, `param`, and `user`. Driver injects
one verified local boot image and disables host mounts/agent/key forwarding. Supply
only backend-required boot settings and no provider settings or credentials.
`templates/agent-base.yaml` is the one-time preparation recipe and cannot be passed
as this runtime YAML. Metadata is accepted only with sealed/two independent boots/
unique identities. Independent image-worker acceptance remains a rollout gate.

Task driver config adds `template_id`, `template_version`, `template_disk_sha256`,
`creator_agent_id`, `creator_session_id`, `purpose`, optional `project`, and `task_link`.
Driver validates against its own operator-approved catalog. Agent-visible discovery
is advisory; driver reservations/policy and Nomad are authoritative. Existing
`template_file` path remains supported for legacy prototype tasks when no catalog
is configured; catalog-configured nodes require a catalog selection, never fallback.

## Image/activation handoff

To use the new main-task contract, the prepared native disk includes these exact
plugin assets as read-only files (no installation after every VM boot):

- `plugin/guest/task-main.py` -> `/opt/nomad-workers/task-main.py`
- `plugin/guest/adapter.mjs` -> `/opt/nomad-workers/adapter.mjs`
- `plugin/guest/launcher.mjs` -> `/opt/nomad-workers/launcher.mjs`
- pinned base check -> `/usr/local/libexec/agent-base-check` (no provider prompt).

The plugin-owned main task invokes the check, tails allocation/session lifecycle
records to native task stdout, and waits for verified finish ACK. It is the guest
workload of the Nomad task, not an added host service. ACP session readiness is a
separate event after the plugin calls `create`. Runtime CLI readiness alone is not
ACP readiness. No active VM or prepared artifact has been modified by source work.

Activation config adds `catalog_file` and `catalog_sha256` (legacy template fields
may be omitted). All entries in a node catalog must have passed acceptance; build
candidates stay outside it. Catalog mode requires a positive `max_total_disk_gib` logical disk budget derived
from the actual storage capacity (including retained-disk/cache/floor planning).
A missing disk budget keeps driver admission unhealthy/pending, without blocking
Nomad plugin-loader startup. Other optional authoritative node budgets: `max_vms`,
`max_total_vcpus`, `max_total_memory_mib`, `max_total_disk_gib`, `max_guest_vcpus`,
`max_guest_memory_mib`, `max_guest_disk_gib`. Zero disables those other additional driver policies; it never disables native Nomad reservations. These are per-node, not a
cluster-wide substitute scheduler. Disk totals include retained/unknown disks.
The driver serializes admission across processes with a store lock, so another
StartTask cannot bypass the operator budget. Physically retained disk release is
not inferred from DestroyTask. CPU/RAM totals remain conservative until terminal
cleanup is recorded. All changes require coordinated empty-node rollout by Ansible.

Cache is `_catalog-cache` under the existing configured private Lima home. Files
contain public disk/YAML artifacts only and are readable by the existing Lima
runtime user; no credentials are stored. Full hashes are rechecked on a cache hit.
Disk preparation/export/seal and live boot acceptance remain the artifact worker's
gate. Our file-based unit fixtures are deliberately not advertised as boot tests.

Guest asset freeze is `docs/guest-assets.json`. Current release:
`fc8ff405dcd38231509fb39049ec96863f23207e5017ae7e9205060d6117754c`.
Published v1 and v2 freezes are retained in `docs/releases/guest-assets-v1.json` and
`docs/releases/guest-assets-v2.json`; their disk
and object identifiers must not be overwritten. V2 requires the matching new driver
self-revoke lifecycle and external prime_vault config. The adapter includes a shared submit/prepare-finish gate and manifest-bound close
ACK. Do not mix its version with older coordinator finish code. Builder can record
this release hash as `tools.nomad_worker_assets_sha256`; the image itself remains
pinned by its final disk digest. Changes require a new image acceptance/digest.

Metadata distinction: task config now has `worker_session_id` (canonical UUID) in
addition to `creator_agent_id` (instance UUID) and `creator_session_id` (actual native
agent session ID, bounded safe text). The latter is not inferred from worker UUID.
Native Claude hooks supply a private observed creator record; operator-declared
fallback remains explicitly unconfirmed. No metadata field proves authentication.

Aggregate `max_total_memory_mib` covers guest memory plus configured per-VM overhead.
Disk placement/free-space display is advisory; `max_total_disk_gib` is a required
node admission budget in catalog mode, not a filesystem quota. Driver physical
free-space checks and retained logical reservations are both required.
