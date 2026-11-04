#!/bin/bash
# images/pins.sh -- the versions and digests of the executors a platform bundle
# carries. This file is the one place they are written down: images/platform.sh
# downloads the executors a bundle holds, checks them against these pins, and
# records the versions in the bundle's manifest. Nothing else pins them.
#
# The bundle is the operator's recipe, like the image's. Which programs go into
# it is not this repository's decision -- platform.sh takes the boxd it is given,
# and the executors below are the ones this project's own bundles carry. An
# operator who wants another set builds the archive themselves; docs/guest-image.md
# says what a bundle has to look like.
#
# shellcheck disable=SC2034  # every variable here is read by the script that sources the file
# Sourced with BOX_ARCH set (amd64 or arm64). It sets:
#   PRIME_AGENT_VERSION   the release tag of the executor's GitHub release
#   PRIME_AGENT_ARCH      its asset's architecture name (x64 or arm64)
#   PRIME_AGENT_SHA256    the sha256 of that release's linux tarball
#   OPENCODE_VERSION      the npm version
#   OPENCODE_NPM_PKG      the npm package that carries the binary for the arch
#                         (the launcher package installs it from npm's postinstall
#                         script, which needs the target CPU; the per-arch package
#                         is the same binary without that step)
#   OPENCODE_INTEGRITY    the npm integrity (sha512, base64) of that package
#
# The pins were re-checked against their public upstreams when this recipe came
# back (2026-11-04): prime-agent 0.10.2 is the tarball both of its linux releases
# describe, and opencode 2.2.5 is the integrity the registry publishes for both
# of its linux packages. Changing a version means changing its pin: a download
# that does not match stops the build.
set -euo pipefail

PRIME_AGENT_VERSION=0.10.2
OPENCODE_VERSION=2.2.5

case ${BOX_ARCH:-} in
  amd64)
    PRIME_AGENT_ARCH=x64
    PRIME_AGENT_SHA256=09659961dfde201cc39f1e69b607385d708da3e591f267a4fea04970afb8b005
    OPENCODE_NPM_PKG=@opencode/cli-linux-x64
    OPENCODE_INTEGRITY=sha512-BkHAfTqNsg/qphyHNrnH+DGkOxZsGjamNpM5DvwoEvz7ZAPNw8mEbMXGTCDOAH+/+40Eqeie6KNZBZZQwPCjaA==
    ;;
  arm64)
    PRIME_AGENT_ARCH=arm64
    PRIME_AGENT_SHA256=cd31de3a52dac3bdee44b723f10341cc5255f135acbcde8894575e324fa852b7
    OPENCODE_NPM_PKG=@opencode/cli-linux-arm64
    OPENCODE_INTEGRITY=sha512-O4qOy/WRYQmhClMB/5qBaoYfzaDK1+PJRQOfU80ixyj5ruQbCxraH+VW2UyHb6RWOlUv41fec+isd62LvCuwuA==
    ;;
  *)
    printf 'pins.sh: BOX_ARCH must be amd64 or arm64 (got "%s")\n' "${BOX_ARCH:-}" >&2
    exit 1
    ;;
esac
