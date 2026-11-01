# Next step: one Claude plugin, one Nomad driver

The usable prototype is a generic task driver, not yet a Prime/Claude integration.
No second plugin, dispatcher, gateway, or companion service should be introduced.
No existing BoxLite plugin or production configuration was changed in this work.

Extend the single plugin's existing Nomad adapter to submit `driver = "lima"` with
explicit native guest architecture, command and resource reservations. Discover
eligible nodes using Nomad attributes/resources and constraints; do not pin a
NodeID by default or treat different guest architectures as interchangeable.
Installation-specific template/image/provider/storage settings stay in external
operator configuration. Upstream Lima and distro kernels remain dependencies.

Use native Nomad allocation lifecycle, logs, status and exec streaming. A guest
agent's ACP stdio can be carried by an owned Nomad exec stream; implementing that
adapter and verifying durable ACP identity/reconnect is a separate integration
stage. Successful generic shell tests do not prove Prime session recovery.

For explicit input/result files, use standard Nomad allocation artifacts and the
driver's official Lima copy/exec primitives, with bounded paths, manifests and
checksums. Returning a completed result must not depend on keeping compute alive.
Optional object storage remains optional; no credentials or automatic worktree
upload are implied. Artifact staging/export and secret injection are not implemented
by this prototype yet. No host-home mount or personal SSH key is needed.

Before production rollout: finish the reviewed host network boundary, verify
failure/ownership/resource reconciliation and result retention in the plugin path,
and run one agreed end-to-end Prime task. Publishing or deploying this repository
and new paid model calls were not authorized by these prototype tests.

Read-only inspection of the installed plugin CLI confirmed existing `nodes`,
`resources`, `registries`, `images`, `upload`, `artifacts`, `result`,
`exchange-upload/export/fetch`, `export-ack`, `activity` and `hold` entry points.
Keep those interfaces in one plugin; change their backend adapter only after a
separate compatibility review. No installed plugin file was changed.

An OCI worker image from the existing catalog is not a bootable Lima guest disk.
Discovery must distinguish a pinned upstream Lima guest template from OCI toolchain
images. Reusing an OCI toolchain means running it inside the upstream guest's
standard Docker environment, with a reviewed command/result contract, not passing
an OCI digest to limactl as though it were a QCOW/cloud image. The current minimal
test guest does not contain Prime or a project toolchain.

For remote transfer without a new service, stream explicit bounded archives through
native Nomad exec stdin/stdout (implemented by this driver's official Lima SSH path)
and verify their manifest/size/SHA. `limactl copy` itself runs on the node; it is not
a new remotely exposed API. No automatic whole-worktree or secret upload is implied.

ACP continuity is a separate acceptance gate: generic exec does not attach to the
stdin of the already-started workload. Verify an official Prime/acpx resume contract
with durable session identity over new exec streams before promising reconnect.
If upstream cannot do this, report that contract blocker rather than silently
introducing a guest relay service. VM recovery alone is not ACP recovery.
