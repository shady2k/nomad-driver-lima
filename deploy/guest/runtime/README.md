# Existing pinned runtime inputs

These files are verbatim copies of the previously verified `nomad-driver-boxlite`
`examples/prime/guest/{provision.sh,package.json,package-lock.json,uv.lock}` and
`images/prime-base/kernel.lock`. They install unmodified official Prime 0.10.1,
Node 24.22.1, acpx 0.21.0 and uv 0.13.5. The provision script pins release archive
SHA256; npm/uv locks pin runtime dependencies. No fork, provider config or secret is
included. `prepare-worker.py` records exact hashes in its output manifest.

The system apt packages are the existing Ubuntu image's standard repositories;
their exact package versions are not snapshotted. Thus runtime inputs are pinned,
but rebuilding the whole guest is not claimed bit-for-bit reproducible. Downloads
and package availability still need the approved native provisioning acceptance.
The original package name contains BoxLite as historical metadata; no BoxLite
runtime or companion is installed by these files. Never execute provision.sh on
the host. Provisioning runs only in a newly owned Lima guest.

Official release sources:
https://github.com/PrimeIntellect-ai/prime-agent/releases/tag/v0.10.1
https://nodejs.org/dist/v24.22.1/
https://www.npmjs.com/package/acpx/v/0.21.0
