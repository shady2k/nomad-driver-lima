# Native agent base (recipe only)

`templates/agent-base.yaml` is the first catalog candidate, `template_id = agent-base`,
version `v1-prime0.10.1-acpx0.21.0-node24.22.1`. Each task gets its own VM cloned
on demand from a prepared stopped base. The base is not a shared worker. A future
plugin request selects `template_id`, immutable template version, architecture and
resources; Nomad places the task and the driver owns its distinct clone.

## Contents and preparation boundary

The recipe uses the same checksum-pinned Ubuntu 24.04 minimal images as
`examples/ubuntu-pinned.yaml`. It installs native Prime 0.10.1, acpx 0.21.0,
Node 24.22.1 and uv 0.13.5, plus the full tested Prime Python kernel dependencies.
It needs no Docker, containerd, project repository or BoxLite component.
The public npm/uv/hash-locked kernel metadata is embedded so the template has no
relative file dependencies when loaded from a pinned raw URL. Release downloads
are SHA256-checked; npm uses its integrity lock and Python its frozen/hash locks.
Ubuntu packages still follow the signed Ubuntu repositories, not a package snapshot.

Prepare one base per guest architecture AND VM backend (Linux QEMU vs Mac VZ).
Do not copy a QEMU base to VZ or use emulation as native architecture acceptance.
Recipe defaults (2 CPUs, 4 GiB, 20 GiB disk) are preparation defaults, not a cluster
budget. Future clones must receive their own Nomad-accounted requested resources;
disk cannot shrink below the prepared disk's filesystem requirements.

Nothing has been created or installed by adding this recipe. Future operator steps,
only after activation approval, using the same runtime user and LIMA_HOME as the driver:

```sh
limactl start --tty=false --name=agent-base-v1-amd64 templates/agent-base.yaml
limactl shell agent-base-v1-amd64 sudo /usr/local/libexec/agent-base-check
# Complete the seal/identity gate below before accepting the base.
limactl stop agent-base-v1-amd64
limactl list --json
# Example only: current driver does NOT yet issue this command.
limactl clone --tty=false agent-base-v1-amd64 task-unique-name
limactl start --tty=false task-unique-name
```

The provision script emits stages to Lima's provisioning output. It writes a recipe
fingerprint only after a successful no-model check. Subsequent starts check that
fingerprint, CLI versions, imports and ACP; they do not reinstall. A fingerprint
mismatch refuses the old base: prepare a new version instead of mutating old clones.
If initial preparation failed before the marker, rerunning can resume installation.
No provision script uses shell xtrace. Do not run provisioning over a credentialed base.

## Readiness and clone hygiene gate

`agent-base-check` tests CLI versions, full kernel imports, ACP initialize and
session/new against a temporary loopback synthetic provider. It sends no prompt,
counts unexpected model HTTP requests, closes the transport, and removes its temporary
config/session directory. It does NOT prove authenticated provider access, model
execution, full conversation resume, or production network isolation. acpx readiness
here is its version check; real submit/watch remains the plugin acceptance gate.

Before sealing, confirm there are no surviving Prime/acpx owners/daemons, prompts,
session journals, provider config, API keys, Bao tokens, SSH private keys from a user,
project files, result archives or bootstrap credentials anywhere in the base. Do not
turn a previously credentialed working VM into the base. Check root and guest-user
homes as well as `/opt/worker` and temporary/log/cache locations. Provisioning logs
must contain no credentials. Only Lima-generated instance access material is allowed.

Lima 2.2.1 clone refuses a running source, copies the guest disk, skips selected host
runtime files and resets its VZ identifier. This is NOT evidence that guest machine-id,
SSH host keys, random seed or application state is sanitized. Before production clone
acceptance, verify a distribution-supported seal/first-boot procedure regenerates
machine-id, SSH host keys and seed for EACH clone, and Lima regenerates cloud-init
hostname/access identity. No ad-hoc identity wiping is executed by this recipe.
Record/compare base and clone machine-id and SSH fingerprints; fail closed on duplicates.
This identity gate is pending; the recipe is not a production-sealed image yet.

Only AFTER cloning and identity verification may external operator config supply
provider/model/output budget and session-scoped headers (including stable
`x-opencode-session`). Keep credentials out of YAML, Git, the base and VM command
arguments/logs. Never copy a Bao operator token into a guest. New session UUIDs,
journals and result manifests belong to the allocation clone, not the base. Verify
results and export receipt before stopping a task; retain disks unless explicit purge
was granted. Do not attach host home, filesystem, agent, Docker socket or KVM device.

## Session lifecycle and existing S3 export contract

The agent chooses a template/version and resources; each session receives its own
allocation-owned clone. The stopped prepared base is retained for future clones.
Reuse the existing BoxLite plugin's S3 transfer and committed manifest/receipt
contract for results and diagnostic logs; do not introduce a second uploader or
storage protocol. This is the planned Lima integration, not deployed behavior.

Before stopping or deleting a session VM, upload its selected results/logs, verify
sizes and checksums, and obtain the durable committed manifest/receipt. Retrieval
must use the backend recorded in that receipt and remain possible after VM stop.
An upload failure is an explicit retryable export failure: keep local artifacts
and the VM, do not report exported/committed, and do not delete or release the VM
as though the export succeeded. Retry only the transfer using the existing
idempotent contract; never blindly repeat the model prompt or task execution.

S3 remains optional. Without configured S3, preserve the existing local/direct
backend contract and label the actual receipt backend; coordinator-local disk is
not object-store durability. A configured but unavailable S3 endpoint is not an
absent S3 configuration and must not silently fall back or acknowledge a commit.

No automatic VM deletion or retention deadline has been agreed or enabled.
Normal explicit finish, automatic stop, and destructive disk purge are separate
actions. Disk deletion still requires explicit authorization; neither a successful
S3 upload nor this proposed lifecycle grants automatic purge permission.

## Current implementation gap

Current driver still creates from a YAML image recipe. It does not yet resolve a
catalog ID or clone a prepared base. Minimal next change: resolve an immutable
operator-owned base record (template version/hash, arch, VM backend, stopped state,
seal evidence); persist allocation -> clone intent before cloning; reject name/owner
collisions; clone once; set requested resources before start; verify fresh guest
identity; then use existing exec/logs/stats/wait/recover/stop handling. Recover must
reattach the same owned clone and must not execute the task twice. Protect the base
from task cleanup and keep export-before-finish semantics. Plugin selection is future
work; the installed/published plugin has not been changed.

## Validation and distribution

Source-only review: YAML/schema validation and extracted shell/Python syntax can run
without creating a VM. No VM preparation, ACP probe, clone, reboot, identity seal,
model call or host changes are claimed for this new recipe until separately executed.

[GitHub templates](https://lima-vm.io/docs/templates/github/) support an experimental
`github:OWNER/REPO/templates/agent-base.yaml@COMMIT` syntax since Lima 2.0. Prefer
`https://raw.githubusercontent.com/OWNER/REPO/COMMIT/templates/agent-base.yaml` with an
immutable reviewed commit. These are documentation placeholders, not published URLs.
No GitHub publication is authorized by this change. See the pinned
[2.2.1 clone implementation](https://github.com/lima-vm/lima/blob/v2.2.1/pkg/instance/clone.go)
and [CLI](https://github.com/lima-vm/lima/blob/v2.2.1/cmd/limactl/clone.go).
