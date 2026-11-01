# Optional repository profiles

Integrated source implementation, not deployed. No running VM, installed
plugin, Nomad client, ACL or OpenBao policy has been changed. Work is isolated
on `feature/repository-profile-integration` for coordinated merge with resume.

## Configuration and request

The driver accepts `repository_profiles_file` and `repository_profiles_sha256`:
an absolute private operator JSON path and SHA-256 of its exact bytes. The
[public example](../plugin/examples/repository-profile.example.json) contains
placeholders only and no default. Without a configured default or an explicit
request, existing behavior and legacy task-spec hashes are unchanged.

The document contains `repository_profiles` keyed by approved ID and optional
`repository_profile_default`. Each definition has schema_version 1, id, version,
and optional apt/npm/go settings. `architectures` may restrict tested variants.
The normalized definition is pinned once in the task's durable driver state;
a changed default cannot alter an existing task or recovery.

The caller supports `request --repository-profile ID` and optional
`--repository-profile-sha256 SHA`; the same options work for capacity request
fit. Only compatible nodes enter the resource envelope. Nomad constraints bind
advertised per-node profile digests; driver admission remains authoritative.
No agent-supplied endpoints are accepted. A normal request with no flag uses the
configured node default automatically. Node capacity exposes profile IDs/digests
and the default, never endpoints or credentials.

Ubuntu 24.04 Linux guests on amd64/arm64 are supported. A profile can explicitly
restrict architectures; do not advertise an untested cache architecture. Current
anonymous READ caches require no credential or new OpenBao grant.

## Client behavior

* apt rewrites only mapped suites in the original `ubuntu.sources`. Optional
  `apt.components` limits mapped components: other components keep the original
  URI, as do unmapped suites such as updates/backports. All source types,
  architectures, official Ubuntu Signed-By and other repository files are
  preserved. Unsupported formats, missing mapped suites or signature downgrades
  fail before workload launch. No signing key or trusted=yes is installed.
* npm uses the configured registry and strict TLS. Existing package-lock SRI is
  unchanged. Explicit custom tarball URLs can still reference other hosts.
* Go uses GOPROXY without direct fallback unless configured separately outside
  this schema. GOSUMDB/GONOSUMDB/GOPRIVATE/GOAUTH and checksum verification are not
  changed. This is not an offline/only-cache egress guarantee.
* `go.distribution_base_url` becomes the documented installer hint
  `NOMAD_WORKER_GO_DIST_BASE_URL`. It is not a Go-standard setting and cannot
  transparently redirect a hardcoded curl URL. Archive installation must still
  use the exact version/architecture and separately pinned official checksum.

Profile files/executor are root-owned read-only data in guest `/run` tmpfs. A
neutral symlink `/etc/apt/apt.conf.d/99nomad-worker-repositories` targets the
runtime config, so normal `sudo apt` works without env_keep or sudoers changes.
Original apt source files are not overwritten; a conflicting admin drop-in
fails rather than being replaced. The candidate sanitizer removes our exact
neutral symlink before export, even when `/etc/apt` is retained. No private URL
is written into that on-disk link or the base image.

APT lists and per-user npm/Go module caches use temporary profile-owned paths
under `/tmp`; normal HOME caches are not rewritten. Existing image sanitation
must remove temporary/client caches and project data. No authentication values
are accepted in profiles, URLs, argv, image files or logs.

## Automatic execution path

1. Fresh StartTask selects the operator default/approved ID and snapshots its
   normalized definition/digest before VM creation. Recover/lookup uses that
   snapshot instead of selecting a new default.
2. After guest machine/boot identity is known and before token delivery/workload
   launch, the existing Lima transport runs the embedded one-shot helper as
   guest root. It checks OS, identity, tmpfs and digest. It makes no network
   request or package installation. Failure retains the VM and prevents launch.
3. The receipt binds profile, environment and reviewed executor SHA-256. Driver
   native stages report apply/ready/failure with ID/version/digest and safe error
   code, not endpoints or complete payloads.
4. Driver wraps the workload and all native Exec/Stream/PTY commands with the
   verified guest executor. Prime, its subprocesses, native foreground exec and
   managed-operation supervisors inherit one profile. Foreground/managed child
   launchers also rebind the reference before Popen. No prompt is resent.
5. Direct `sudo [-n] [-H] [-E] [--] COMMAND` reapplies the environment inside sudo,
   not through a new policy grant. Unsupported direct sudo option forms fail
   explicitly. Nested arbitrary shell sudo behavior is not rewritten; apt is
   covered independently by its global runtime drop-in. Ordinary non-root
   npm/Go commands and `sudo ... bash -c ...` inherit the correct profile.

The executor is a file of our existing driver/plugin, not a daemon/bridge.
Missing/tampered tmpfs files fail; an existing task is not hot-patched. Rebooted
VMs require their normal managed new-run preparation before runtime readiness.

## Rollout and compatibility

Merge this branch with the separately developed resume changes before producing
an installation release. Shared files requiring review are driver.go/store.go,
caller request/CLI and sanitation. No live restart or profile activation is
part of this source change. The ordinary disabled path remains schema 1 with
legacy-compatible spec hashing. Profile-enabled records use schema 2, which
older binaries reject instead of silently dropping the profile on recovery.
Coordinate schema 2 with resume; do not downgrade to an older binary once such
records exist. Disabling the default affects only future requests.

Stage the private JSON using the existing Ansible installation workflow, then
set the two plugin config fields. Validate/readback and an empty-client reload
must be coordinated by the deployment owner. Preserve existing tasks, caches
and native owner records. No guest image rebuild is required: the reviewed
helper is embedded in the driver and sent over the existing guest transport.
A bounded owned no-model VM acceptance after merge remains the runtime gate;
source tests are not presented as a live guest installation.

## Verification and authentication boundary

Tests cover profile/default/request/ledger pinning, disabled legacy hashing,
apply-before-launch/no-secret-before-ready, native foreground and real managed
subprocess execution, native npm/Go clients in a relocated filesystem fixture,
sudo environment reset, selective sources, and tamper/ownership rejection.
Native apt-config also parsed the managed drop-in in an unprivileged temporary
Linux fixture. That test changed no host config and made no downloads; actual
cache routing/signatures are separately verified by the deployment owner.

Future authenticated caches would need separately approved exact OpenBao
path/field grants and standard Bao Agent process/tmpfs delivery. No such
transport or grant is added now; the current LLM-only role remains unchanged.

References: [APT config precedence](https://manpages.debian.org/bookworm/apt/apt.conf.5.en.html),
[npm configuration](https://docs.npmjs.com/cli/v11/configuring-npm/npmrc/),
[Go module authentication](https://go.dev/ref/mod#private-module-proxy-auth).
