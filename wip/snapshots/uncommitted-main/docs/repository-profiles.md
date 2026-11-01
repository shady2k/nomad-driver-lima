# Optional repository profiles

Status: source-only hooks and tests. No running VM, host configuration, ACL,
OpenBao policy, installed plugin or guest image has been changed. Driver/caller
wiring is a separate coordinated step; the profile is not available in an
installed task schema yet.

The operator may define `repository_profiles` in private installation config.
An agent chooses an approved ID; it cannot supply repository URLs. No default
profile is enabled. The public [example](../plugin/examples/repository-profile.example.json)
uses placeholder addresses and deliberately omits `repository_profile_default`.
Setting that external default to an approved ID opts in for new VMs only.

Version 1 supports anonymous HTTPS repositories in Ubuntu 24.04 Linux guests on
amd64 and arm64. Other guests fail before writes when enabled; disabled profiles
leave their old behavior unchanged. Credentials, custom archive keys, proxy
interception, insecure TLS and arbitrary client settings are not supported.
Current anonymous cache access requires no new credential or OpenBao transport.

## Behavior

* apt maps only configured suites in the existing `ubuntu.sources`. A combined
  `noble noble-updates noble-backports` stanza is split, preserving the original
  URI for every unmapped suite. `noble-security` has its own mapping. Components,
  architectures and the official Ubuntu `Signed-By` keyring are retained.
  Other source files and the main sources.list are copied unchanged into a
  temporary shadow configuration. Unsupported source formats, missing mapped
  suites or disabled signature checks reject the profile. System source files
  are never overwritten. TLS and signature verification remain enabled.
* npm receives a temporary user config, registry selection and strict TLS.
  Existing package-lock integrity data and normal per-user cache are unchanged.
  Locks with explicit custom tarball URLs can still reference their original
  hosts: a registry profile is not a transparent network redirect.
* Go receives `GOPROXY` only. `GOSUMDB`, `GONOSUMDB`, `GOPRIVATE`, `GOAUTH` and
  existing module checks are not changed. Checksum-database access may still be
  needed; this does not promise cache-only egress or offline builds.
* An optional `go.distribution_base_url` is an installer URL helper, not a made-up
  Go environment setting. `distribution_url(profile, platform, "1.26.10")`
  returns the exact architecture-specific archive URL. The installer must still
  verify its separately pinned official SHA-256. No download happens here.

Configuration lives only under `/run/nomad-worker-repository-profiles/<digest>`
on guest tmpfs. APT lists use `/tmp/nomad-workers-repository-cache/<digest>`;
APT manages its own sandbox partial directory. The helper changes no existing
user home, root-owned runtime-token directory or directory permissions. Before
publishing a prepared image, the existing approved sanitation composition must
remove temporary/client caches and project data; `/run` itself is not exported.
No profile endpoints are added to the base image or public repository.

## Integration contract

1. `plugin/scripts/repository_profiles.py:select(config, requested_id, platform)`
   resolves the private definition and returns its normalized version and SHA.
   Pin those values in the create intent/task binding, so a changed operator
   definition cannot silently alter an existing request.
2. After the driver's existing guest identity check, pass
   `apply_payload(selected, expected_guest_identity)` to the reviewed guest
   helper through the existing Lima guest transport, as guest root. This is a
   one-shot operation, not another service. It performs no package installation
   or network request. It verifies guest identity, supported OS, tmpfs and profile
   digest before writes. An enabled profile failure must prevent runtime-ready;
   it must not silently fall back to public registries.
3. The receipt contains ID/version/profile digest, environment-file path and
   environment digest. It contains no endpoint values. Store the two-digest
   result of `execution_reference(selected, receipt)` with the task/session.
4. Immediately before the existing guest workload/managed-operation `Popen`,
   call `repository_profile.execution_environment(reference, inherited_env)`.
   It verifies the fixed root-owned read-only environment file and digest,
   then overlays only the approved client settings. `None` preserves ordinary
   subprocess inheritance. No general environment snapshot is persisted.
   Wire this to the native workload and native exec/managed operations together;
   preparing files alone does not configure their processes. For root installs,
   apply the overlay inside the existing root guest wrapper: `sudo` may discard
   environment supplied before it. Do not change host sudo policy.
5. Emit native task stages for profile apply/start/ready/failure using only
   ID/version/digest and safe error codes. On a reboot, `/run` must be prepared
   again before launching a new runtime. A missing or changed environment file
   is an error, not permission to resend an old command or prompt.

The shared driver, bootstrap, managed-operation and caller files are intentionally
untouched while the separate resume change is in progress. They must consume the
above contract before operator config is activated. Existing sessions are not
modified by selecting a profile for a new VM.

## Authentication boundary if needed later

Authentication is not implemented or required for the current anonymous READ
profile. A future private cache would require a separately approved, exact
OpenBao path/field read grant. Reuse the task's Nomad-managed Vault lifecycle;
never send an operator token or the LLM record to package clients. Official Bao
Agent supervision can supply credentials only to the relevant client process or
its private tmpfs files. npm can use a registry-scoped environment token; apt can
use a private auth.conf; Go supports `NETRC` (host scope, not URL-path scope).
No credential-bearing URLs, image files, argv or logs. Do not extend the current
LLM-only role silently.

References: [APT config precedence](https://manpages.debian.org/bookworm/apt/apt.conf.5.en.html),
[APT auth.conf](https://manpages.debian.org/testing/apt/apt_auth.conf.5.en.html),
[npm configuration](https://docs.npmjs.com/cli/v11/configuring-npm/npmrc/),
[Go module authentication](https://go.dev/ref/mod#private-module-proxy-auth).
