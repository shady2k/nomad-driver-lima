# nomad-driver-lima

A prototype of a Nomad `lima` task driver using the **official Lima CLI**.
Lima, QEMU/VZ and guest distributions are unmodified upstream dependencies.
There is no REST server, scheduler, companion service or runtime/kernel fork.
The public release targets Linux amd64 hosts. Mac host support remains
experimental and is not included in the release binaries.

Read [the architectural decision](docs/adr/0001-upstream-lima.md).

## Implemented

- Start, wait, inspect, signal, stop, destroy-with-disk-retention and recover.
- Durable allocation/task/owner, instance config, machine and boot identity.
- Exclusive launch intent and per-task process lock; uncertain commands are not
  replayed. Recovery attaches to the same guest process rather than calling start.
- Real guest exit codes and atomic guest result; bounded stdout/stderr forwarding
  to Nomad. Complete logs remain on the guest disk. A crash can replay the last
  forwarded chunk because the Nomad log FIFO has no transactional acknowledgement.
- Official `limactl shell` non-TTY exec/streaming and `copy` transfer primitive.
- PTY exec and terminal resize through the instance-generated official SSH config;
  Lima-managed key only, no agent forwarding or personal SSH configuration.
- Native architecture Linux guests, host mounts/personal keys/agent forwarding
  disabled, operator-pinned template and exact stable Lima version.
- CPU/RAM reservation validation using Nomad agent compute, explicit guest RAM
  overhead, physical free-space guard. Group ephemeral disk belongs in the jobspec.
- Fingerprint and lifecycle events. Network isolation is explicitly **false**.

## Deliberate limitations

**Not production ready.** Stats sample the owned host VM process: RSS bytes and
CPU percent (100% = one core), with Nomad CPU ticks expressed in MHz. They do not
represent guest free memory, command-only usage, or helper process overhead.
Sampling stops on missing/changed process identity; no synthetic zero samples.
macOS guests,
workload environment/secrets injection and automatic artifact export are not yet
implemented. A transport failure stays unknown and is not successful completion.
Early ambiguous startup preserves the instance and ledger for reconciliation.
Automatic idle close, disk purge, broad file sync and automatic acceptance of edits
are not implemented. `force=true` never grants disk deletion.

Default Lima networking can reach host loopback/LAN; this prototype does not claim
the former BoxLite egress allowlist. An additional reviewed **upstream/host network
policy** is required before untrusted production workloads. No host firewall or VPN
changes are made by this driver. Linux requires an unprivileged runtime owner with
KVM access and official QEMU/OVMF. A root Nomad agent must configure an existing non-root `runtime_user`. The driver
drops UID/GID and supplementary groups only for official limactl child processes;
it never invokes sudo or creates accounts/grants. Its private Lima home must already
belong to that account and its template must be readable by it. Unprivileged drivers
can only select their own identity. This path has unit coverage; live root-agent
acceptance remains pending.

`limactl shell --sync` is upstream interactive review functionality, not a headless
remote coordinator protocol. The driver uses explicit official copy. Sync excludes
and `.limasyncignore` are documented for 2.3, not stable 2.2.1.

## Local development

The pinned Nomad SDK source revision matches the tested Nomad 2.0.7 revision
`9dcbdc5e64ecc4ec63e42b225af729c87c87cf83`; the Go module's historical pseudo-version
name must not be mistaken for the deployed Nomad version.

```sh
go test -race ./...
go build -trimpath -o bin/nomad-driver-lima ./cmd/nomad-driver-lima
```

Tests use a fake official CLI and a real Nomad SDK gRPC adapter. Real VM tests are
opt-in and require an isolated operator JSON file via `NOMAD_LIMA_LIVE_CONFIG` and a
private `NOMAD_LIMA_LIVE_ROOT`; they must never target production state. No paid model
is called. Acceptance evidence is installation-local and is not committed here.

## License

Original driver and plugin code is licensed under [Apache-2.0](LICENSE). See
[NOTICE](NOTICE) and [third-party licensing](THIRD_PARTY.md) for dependencies.
The compiled driver's dependency licenses, including Nomad BUSL/MPL scopes,
remain applicable; the project license does not replace them.


The caller plugin is in `plugin/`. It includes owned project-session discovery,
durable execution, retained-disk resume and terminal Nomad refusal reporting.
See [the caller configuration guide](docs/public-quickstart.md) and
[the retained resume contract](docs/retained-resume.md).
