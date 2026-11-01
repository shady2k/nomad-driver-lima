# Linux canary deployment

`deploy/install.yml` stages only the Lima driver binary, pinned guest template and
plugin fragment. The default action is read-only preflight. There are no Nomad restart handlers, account/group creation, network/ACL/capacity
changes or VM launches. Explicit BoxLite retirement can stop its dedicated REST unit.
Inventory, operator vars, artifacts, host evidence and credentials stay outside Git.

## Inputs and preflight

Copy `deploy/vars.example.yml` outside the repository and replace all placeholders.
Select exactly one existing host in an external `[lima_canary]` inventory. Pin the
native Linux artifact SHA256 and reviewed template SHA256; record source commit,
architecture and embedded driver version externally. Build on the development host,
not a memory-constrained workload host. The current embedded version is
`0.1.0-prototype`; release ID is metadata, content SHA256 is the installation pin.

Directories must already exist: private 0700 runtime-owned Lima home, private driver
state writable by the Nomad identity, plugin/config/template directories. This role
assumes root-owned Nomad installation files. It never provisions runtime identities
or grants KVM. Runtime must already be nonroot and have read/write `/dev/kvm`, exact
Lima 2.2.1, QEMU/OVMF and helper binaries in its effective PATH. The Linux template
must match host architecture and be readable by the runtime. Keep paths short for
Unix sockets; verify free disk at Lima home and RAM/CPU against Nomad reservations.

Use `deploy/preflight.py` through approved SSH, with existing host-key verification:

```sh
ssh APPROVED_TARGET python3 - --runtime-user EXISTING_USER \
  --limactl /EXISTING/limactl --nomad /EXISTING/nomad \
  --lima-home /EXISTING/private-home --service EXISTING_SERVICE \
  < deploy/preflight.py > /PRIVATE/EXTERNAL/preflight.json
```

The probe does not read credentials, elevate privileges or declare a node empty.
Unavailable API access is a blocker. Inspect complete node allocations, actual
service identity and ordered `-config` paths separately. Preserve `plugin_dir` and unrelated configuration. The final Linux target uses
`client.options["driver.allowlist"] = "lima"`, removes the old BoxLite stanza and
moves its executable outside discovery, as described below.
The executable stanza is `plugin "nomad-driver-lima"`; task driver name is `lima`.

Set `lima_nomad_config_paths` to the **same complete ordered config set** the service
will use, including the new fragment. If service currently loads a single JSON file,
staging a nearby HCL file does not load it: an explicit approved additional `-config`
argument or reviewed merged configuration is required. The role changes neither
service arguments nor existing client configuration. Nomad plugin blocks replace
rather than merge for the same executable name; never duplicate BoxLite stanzas.

## Stage without activation

```sh
ANSIBLE_LOCAL_TEMP=/tmp/lima-ansible-tmp ansible-playbook \
  -i /PRIVATE/inventory deploy/install.yml --syntax-check
ansible-playbook -i /PRIVATE/inventory deploy/install.yml \
  -e @/PRIVATE/vars.yml -e lima_action=preflight
ansible-playbook -i /PRIVATE/inventory deploy/install.yml \
  -e @/PRIVATE/vars.yml -e lima_action=install --check --diff
# Only after explicit install approval:
ansible-playbook -i /PRIVATE/inventory deploy/install.yml \
  -e @/PRIVATE/vars.yml -e lima_action=install
```

Syntax check is offline; normal preflight and check mode contact the selected host.
Read-only version/KVM commands execute even in check mode. Real staging creates
atomic copies with backups and runs `nomad config validate` against the full provided
config set. Keep the exact backup paths and previously absent-file list externally.
A validation failure leaves staged files in place; restore/fix them before restart.
Repeated unchanged staging is idempotent. No automatic restart follows staging.

[Official configuration and reload limits](https://developer.hashicorp.com/nomad/docs/configuration)
require an agent restart for new task-plugin configuration; SIGHUP is insufficient.
[Config validation](https://developer.hashicorp.com/nomad/commands/config/validate)
checks the complete config without starting an agent and needs no ACL token.

## Approval and one-VM canary

Approve exact node, artifact digests, paths, existing runtime identity, any new
private directories, necessary driver allowlist/config inclusion edit, and a single
maintenance restart of the identified client service. Verify client-only role and
complete live allocation list immediately before maintenance. The canary procedure
requires an empty node; existing workloads need separate explicit handling approval.
Do not drain/restart a busy node or stop another VM. Keep out-of-band access.

Lima user networking does not enforce the previous BoxLite LAN allowlist. A trusted
canary requires explicit acceptance of that boundary. Untrusted production tasks
remain blocked until separately approved positive/negative host network policy
checks. Runtime `debian` is a shared existing account, not a tenant isolation boundary.
No account/firewall/ACL change is authorized by installing this package.

Use the single concrete sequence below for the prepared worker allocation. The
separately approved basic driver rollout canary may use the existing pinned minimal
template without guest model dependencies; that verifies installation only. It must
not be reported as a prepared agent runtime or a model acceptance test.

## Claude guest adapter prerequisites

The parallel adapter provides `plugin/guest/adapter.mjs`, `launcher.mjs` and
`plugin/scripts/workerctl.py`. Proposed guest assets are read-only under
`/opt/nomad-workers/`; no daemon/bridge is installed. Guest needs official Node 24
with `process.execve`, acpx exactly 0.21.0, official Prime, workspace, private
state_dir and external provider/model/runtime contract. Guest installation is not
performed by this host role. The adapter binds allocation/job/task/namespace/owner,
requires task driver `lima` and `Job.Meta.nomad_workers_owner=<owner UUID>`, and uses
one plugin session per allocation. Follow the adapter's own verified configuration
schema. Export/manifest validation must succeed before native job stop; no disk purge.

## Rollback

Keep the node ineligible and stop only the owned canary. Resolve unknown outcomes
before retrying tasks. Restore the exact Ansible backups for the three staged files;
on first install remove only paths proven previously absent. Restore the separately
reviewed allowlist/config inclusion edits if activated. Never remove VM homes,
ledgers, allocation directories, BoxLite files or shared plugin directories. Validate
the original complete config, then perform a separately approved client restart on
the empty node. Verify BoxLite health and restore reviewed eligibility. Rollback is
an explicit operator action; staging grants no maintenance or VM-creation permission.


## Exact path from host install to first agent task

Prepare a **new** private external output directory with the generator below. All
values are external operator parameters. Use the exact approved node UUID and native
Nomad MHz for one vCPU; `memory_mib + 128` matches this role's default overhead.
Provider/model names and a declared output budget are required; no key is requested.

```sh
python3 deploy/prepare-worker.py --output /PRIVATE/new-worker-bundle \
  --node-id NODE_UUID --owner-id OWNER_UUID \
  --namespace APPROVED_NAMESPACE --datacenter ACTUAL_DATACENTER \
  --cpu-mhz NATIVE_MHZ_FOR_ONE_VCPU --memory-mib 4096 --disk-gib 20 \
  --provider EXTERNAL_PROVIDER --model EXTERNAL_MODEL \
  --output-token-budget DECLARED_BUDGET --receipt-root /PRIVATE/worker-results
```

The output includes `worker-template.yaml` (JSON, valid YAML), exact-node batch
`worker.nomad.hcl`, `worker.vars.hcl`, `coordinator.json` and SHA256 manifest. It
embeds only an explicit asset set and existing runtime lockfiles; nothing from the
operator's environment/home is read. It refuses to overwrite an existing output.
The owner UUID is independent from the allocation UUID. Job ID defaults to
`lima-worker-canary`; supply `--job-id` for a reviewed unused ID. No task is submitted.

The native first-boot provision installs the existing pinned Ubuntu x86_64 official
runtime and read-only plugin assets. Partial installation fails for reconciliation;
a completed runtime is version-checked on later boot. Private workspace/state and
provider directories belong to the Lima guest user. Root-owned runtime assets remain
readable/executable. No systemd unit or custom daemon is added. Main task waits for
external provider configuration, then `exec`s **official Prime daemon**. It is the
Nomad task process, stopped by the driver lifecycle. Only acpx's official queue owner
and Prime are long-lived runtime components. The wait itself sends no model request.
[Upstream plain mode still runs provisioning](https://lima-vm.io/docs/config/plain/).

Follow this single sequence; do not skip the approval boundaries:

1. Generate the bundle, inspect its manifest/template/job/coordinator configuration,
   and run the offline checks below. Select an unused job ID and exact approved
   namespace/node. Verify operator access to node/allocations, namespace job submit,
   logs, alloc exec and job stop using the existing scoped credentials. The generator
   creates no credentials. Recheck active allocations and native VM/logmon state
   immediately before maintenance; an earlier empty snapshot is not continuing
   authorization.
2. Obtain the installation/restart/canary approval described above. In the external
   host-role vars use generated `worker-template.yaml` as `lima_template_src`, its
   manifest SHA256 as `lima_template_sha256`, and the approved host template destination.
   Run role preflight/check/staging; prepare the final Lima-only client config using the transition planner below.
   Retain BoxLite data but remove its stanza/discovery executable from the active
   driver set. The final allowlist is exactly `lima`. Once
   the node is confirmed empty and admission controlled, validate full config and
   restart its existing client once. Never apply merely because generation passed.
3. After health checks and controlled canary admission, review the native plan, then
   submit exactly once under the separate VM-creation grant:

   ```sh
   nomad job plan -var-file=/PRIVATE/new-worker-bundle/worker.vars.hcl /PRIVATE/new-worker-bundle/worker.nomad.hcl
   nomad job run -var-file=/PRIVATE/new-worker-bundle/worker.vars.hcl /PRIVATE/new-worker-bundle/worker.nomad.hcl
   nomad job allocs -namespace=APPROVED_NAMESPACE -json REVIEWED_JOB_ID
   ```

   If submission outcome is unknown, query that exact job; do not retry blindly.
   Wait for the single allocation/task running and runtime-ready message. First boot
   downloads pinned runtime distributions/dependencies; network availability, apt
   package resolution and boot timeout still require native acceptance. No real VM
   provisioning is claimed by the offline checks. The role installs the template on
   the host; the driver invokes its provisioning only inside the new guest.
4. Verify allocation ownership/node/namespace/task against the generated job. Through
   the existing approved secret-delivery workflow place the existing native Prime
   `models.json` into guest `/var/lib/nomad-workers/provider/models.json` with mode
   0600, then atomically create `provider.ready`. This is a separate credential-use
   boundary; this repository neither retrieves nor supplies credentials. An explicit
   operator-only stdin transfer example (never run by template generation) is:

   ```sh
   nomad alloc exec -namespace=APPROVED_NAMESPACE -task=worker -i=true -t=false ALLOCATION_UUID \
     /bin/sh -c 'umask 077; cat > /var/lib/nomad-workers/provider/models.json.tmp && python3 -m json.tool /var/lib/nomad-workers/provider/models.json.tmp >/dev/null && mv /var/lib/nomad-workers/provider/models.json.tmp /var/lib/nomad-workers/provider/models.json && touch /var/lib/nomad-workers/provider/provider.ready' \
     < /PRIVATE/EXISTING/prime-models.json
   ```

   This example is only for a newly owned trusted allocation; do not replace config
   after a plugin session is bound. It does not print the input. Do not put its content
   in a jobspec, template, Nomad variables, command arguments, Git or reports. A provider
   using another native auth mechanism needs its existing approved delivery workflow;
   do not infer new access from the example. Verify the chosen provider/model settings
   manually. The declared output budget is **not enforced or verified** by the adapter.
5. Confirm the daemon socket exists via native alloc exec without a model request.
   In generated `coordinator.json` replace only allocation placeholder with the exact
   allocation UUID **before first create**; confirm local `nomad_bin` and private
   receipt path. From then on configuration is immutable for that session. Set UUIDs
   for session/request once, then run:

   ```sh
   python3 plugin/scripts/workerctl.py --config /PRIVATE/new-worker-bundle/coordinator.json create --session SESSION_UUID
   # Only after explicit permission for the selected provider/model call:
   python3 plugin/scripts/workerctl.py --config /PRIVATE/new-worker-bundle/coordinator.json submit --session SESSION_UUID --request REQUEST_UUID --prompt-file /PRIVATE/prompt.txt
   python3 plugin/scripts/workerctl.py --config /PRIVATE/new-worker-bundle/coordinator.json watch --session SESSION_UUID --request REQUEST_UUID
   python3 plugin/scripts/workerctl.py --config /PRIVATE/new-worker-bundle/coordinator.json collect --session SESSION_UUID --request REQUEST_UUID result.txt
   python3 plugin/scripts/workerctl.py --config /PRIVATE/new-worker-bundle/coordinator.json finish --session SESSION_UUID --request REQUEST_UUID
   python3 plugin/scripts/workerctl.py --config /PRIVATE/new-worker-bundle/coordinator.json result --session SESSION_UUID --request REQUEST_UUID
   ```

   Choose a bounded prompt that explicitly writes `result.txt` in the workspace.
   `watch` reconnects to the same request, never resubmits it. Unknown outcome/owner
   death requires reconciliation, not a second request. `finish` requires successful
   SHA/manifest checks and stops only the owned job, retaining VM disks. Successful
   offline transport tests do not prove effective provider configuration or a real
   Prime model turn. That final canary remains explicitly unperformed here.

Offline checks, confined to the development machine and temporary output:

```sh
python3 -m unittest discover -s deploy/tests -p 'test_*.py'
ANSIBLE_LOCAL_TEMP=/tmp/lima-ansible-tmp ansible-playbook -i localhost, deploy/install.yml --syntax-check
# Optionally parse the generated real parameter set without submitting:
nomad job run -output -var-file=/PRIVATE/new-worker-bundle/worker.vars.hcl /PRIVATE/new-worker-bundle/worker.nomad.hcl > /PRIVATE/parsed-job.json
```

The tests parse the job with the installed native Nomad CLI, check plugin ownership
bindings/no retry policy, decode embedded assets/lock versions, check shell syntax,
manifest hashes and overwrite refusal. They do not install packages or invoke a
runtime daemon/model. Whole-guest bit-for-bit reproduction is not claimed: upstream
apt repositories remain external; verified runtime archives and npm/uv locks are
pinned. Full generated-template validation with Lima 2.2.1 and first-boot acceptance
remain necessary before claiming a native usable worker.


## Final Linux target: Lima only (preparation, not activation)

The final target is one Linux Nomad client using Lima as its only allowed task
driver. Temporary restoration of the previous client after the failed activation
was recovery, not a decision to keep BoxLite in use. No new apply/restart is granted
by this preparation. The initial `SetConfig` topology regression must pass before
reviewing the new binary and agreeing activation again.

Use `deploy/prepare-lima-only.py` against a **private external** copy of the complete
current JSON and the reviewed nine-field Lima config. The planner writes only a new
private output directory; it never contacts a host, modifies the source config,
moves a binary, invokes a service or submits a VM. The output can contain existing
sensitive client configuration: keep it outside Git and do not paste full diffs in
logs. The example config has no credentials.

```sh
python3 deploy/prepare-lima-only.py \
  --source-config /PRIVATE/current-client.json \
  --lima-config /PRIVATE/reviewed-lima-config.json \
  --disabled-plugin-dir /ABSOLUTE/REMOTE/private-disabled-plugins \
  --output /PRIVATE/new-lima-only-review
# Full syntax validation only; no agent is started:
nomad config validate /PRIVATE/new-lima-only-review/client.lima-only.json
```

The candidate changes exactly the driver allowlist to `lima`, removes
`nomad-driver-boxlite` plugin configuration and replaces/adds only the reviewed
`nomad-driver-lima` stanza. It preserves client capacity, reservations, service
paths, unrelated plugins and all other configuration. The separate plan identifies
only the old `nomad-driver-boxlite` executable to move from the existing plugin dir
to an operator-selected private directory **outside** discovery. Keeping a renamed
executable inside `plugin_dir` is insufficient: Nomad may still discover it. Check
real paths/symlinks on the Linux host before applying the move. Preserve original
hash/mode/owner and refuse to overwrite an existing destination. Existing sources,
releases, ledgers, allocations and every VM disk stay in place. No purge occurs.

After the separate renewed approval: disable admission temporarily, recheck every
namespace and native VM/logmon state, save the exact original config/binary metadata,
install the regression-tested Lima binary, move only the old discovery executable,
validate the **complete** final config, then activate that one Linux client. The
existing service unit and `-config` argument need no change when the generated JSON
replaces its current JSON. Require Ready and healthy Lima, with BoxLite absent from
the active driver set; then run exactly one trusted no-secret/no-model Linux VM.
Retain its result/disk and stop it normally. Restore reviewed admission afterward.
No Mac client or coordinator configuration/service is involved; operator API reads or
small submission coordination do not deploy code there. The review planner itself does not stop the old BoxLite REST service; the separately
approved Ansible retirement path below handles that dedicated unit.

The new final configuration, executable move and second activation remain pending
review after the core regression. Network/capacity/credentials/ACL changes are not
part of the transition. On activation failure, use the saved baseline only to restore
service availability; do not silently treat it as successful Lima migration.

Nomad's driver architecture field/fingerprint uses Go `amd64`; the upstream Lima
image architecture uses `x86_64`. The prepared Linux jobspec uses `amd64` for both
task config and driver-attribute constraint. Do not substitute image architecture
names in the driver configuration.


## Ansible is the final configuration source

The role now imports the same pure transition projection as
`prepare-lima-only.py`: it reads the existing client JSON privately, writes and
validates a private full candidate, reconciles the actual configured JSON with a
rollback backup, and moves the exact old driver outside discovery. There is no
manual-only config delta and no restart handler. Supply `lima_client_config_path`,
`lima_candidate_config_path`, `lima_disabled_plugin_dir` and the reviewed old driver
SHA256 in the external vars. `lima_empty_node_confirmed=true` is required for an
install and means an immediately preceding operator all-namespace plus native
VM/logmon check has passed; it is not an instruction to interrupt work.

For separately authorized Linux BoxLite retirement, set `lima_retire_boxlite=true`
and provide an exact `lima_retire_paths` list of `{src, name}` pairs and a private
`lima_retire_archive` outside plugin discovery. The role stops/disables only
`boxlite-rest.service`, then moves those bounded installed binary/service paths to
the recoverable archive. It never removes VM disks, source/data trees, Nomad state,
OpenBao auth/agent, envconsul, provider/model settings or shared QEMU/Lima packages.
The OpenBao agent's historical BoxLite name does not make it disposable: retain it
while it supplies the existing host identity used by Prime. No automatic service
restart or canary submission occurs. A second unchanged reconciliation should show
zero changes; verify that on the actual private inventory before declaring the
manual activation captured. Private JSON contents are hidden from Ansible output.

## Vault integration and independently versioned guest assets

`openbao-nomad.yml` reconciles the reviewed JWT trust, narrow role and policy from
an external private spec. `llm-record.yml` copies only the approved LLM field
inside the same OpenBao instance using create-only CAS; it never exports the value.
`nomad-vault-server.yml` validates the candidate with the running server binary,
backs up the old config, and activates the approved server change. A single-server
restart temporarily interrupts API/scheduling and needs an agreed window.

`nomad-vault-client.yml` uses the existing operator identity on its existing host
for fresh allocation checks. It refuses native VM/logmon activity before restart,
retains the expressly selected test VM disk, and records activation by pinned
binary/config-release identity so unchanged reconciliation does not restart again.
Inventory/specs, token files and backups remain outside the repository. No operator
or host OpenBao credential is copied to a worker. The official Nomad CLI wrapper
`nomad-operator-cli.py` reads the preexisting identity only on that operator host.

The driver role accepts the explicit HTTPS-origin `lima_vault_address` together
with a pinned catalog and an externally reviewed retained-disk budget. Retained
and uncertain attempts consume that budget. Do not clear a ledger or increase
capacity to conceal an uncertain operation; inspect the exact attempt first.

`base-refresh.yml` creates a separate builder from the previously accepted
secret-free immutable disk and verifies a distinct frozen guest release before
any VM creation. It does not rerun dependency installation. Refresh, sealing,
export, two independent boots and acceptance produce a new disk/catalog version;
the previous published version stays unchanged. Image boot acceptance is separate
from live Nomad/OpenBao login, process-only injection and cleanup acceptance.

`cache-traversal.yml` records the specifically reviewed existing-cache adjustment:
only the exact directory is root-owned, runtime-group traversable, without group
listing/write or recursive changes. It requires the exact accepted secret-free
artifact set and verifies runtime traversal/read plus denied list/write. Normal
cache creation and validation belong to the driver and must pass its source tests.

## Native no-model acceptance

Leave `guest.model` null for the Prime Vault path: the launcher already selects
`guest.prime_vault.provider.name/model`. acpx's optional override is a separately
advertised ACP model identifier; a bare provider model name may be rejected after
session initialization. Do not mutate an existing session's hashed configuration.

`vault-no-model.py`, staged/executed by `vault-canary-operator.yml` and
`vault-no-model-run.yml`, initializes once and never calls submit or invents a
model turn. It verifies native workload login, exact narrow token policy, tmpfs,
own-token renewal, Prime credential-name presence and an empty Prime auth store.
A one-shot `sessions new` may exit its ACP process normally; environment inspection
is explicitly marked unobserved if no such process remains. This is not a failed
runtime-readiness check and does not justify replaying create.

A confirmed-ready operation may resume only its health/export/finalize steps with
`--resume-ready`; config/attempt/session must still match, and no create is issued.
The zero-turn health file and redacted native diagnostics have an independently
verified coordinator-local manifest. Supported prepare_finish/close/ack then native
job stop must produce BOTH task_token_revoked and vm_stopped. A receipt on local
operator storage is not an S3 backup or a model-response acceptance claim.
Automatic token replacement and real model requests remain separate live gates.
