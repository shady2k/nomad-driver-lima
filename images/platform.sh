#!/usr/bin/env bash
# images/platform.sh -- build one platform bundle for one architecture.
#
# A box does not get boxd and the executors from its image any more: the node
# downloads a versioned "platform bundle" and mounts it read-only at /opt/box
# before the guest boots. This script builds that archive on a Linux build host,
# with no VM:
#
#   images/platform.sh --version 1.0.0 --arch amd64 --boxd dist/boxd_linux_amd64
#
# Usage:
#   images/platform.sh --version V --arch amd64|arm64 --boxd PATH
#                      [--addons DIR] [--out DIR]
#                      [--boxd-version V] [--boxd-meta FILE]
#                      [--built-at RFC3339]
#
# Writes into --out DIR (default <repo>/out):
#   box-platform-<version>-linux-<arch>.tar.gz         the bundle
#   box-platform-<version>-linux-<arch>.tar.gz.sha256  its sha256, sha256sum format
# and prints one JSON line on stdout, the fields of the index entry:
#   {"version","arch","file","sha256","size","built_at","boxd","tools"}
#
# The archive's root is what the guest sees at /opt/box (docs/guest-image.md):
#
#   manifest.json        schema, version, arch, boxd, tools, addons, built_at
#   bin/boxd             the binary given with --boxd
#   bin/boxctl           symlink -> boxd
#   bin/<tool>           symlink into ../tools/<tool>/...
#   tools/prime-agent/   the release tarball's contents, package.json beside the binary
#   tools/opencode/      the npm package's contents (an npm tree)
#   addons/              --addons DIR, packed by images/addons-pack.sh (absent
#                        when it is not given)
#
# Everything a tool needs is inside its own directory, and every symlink in the
# archive is relative and stays inside it: the guest mounts the tree read-only
# and anything pointing outside would point at a path the operator never chose.
#
# The archive is deterministic: entries sorted by name, owner and group 0, mtime
# 0, and gzip without a name or a timestamp. The same inputs give the same bytes
# -- including built_at, which comes from --built-at or SOURCE_DATE_EPOCH when
# they are set and is the current time otherwise.
#
# An arm64 bundle builds on an amd64 host: nothing here runs an artifact of the
# target architecture. The executors are fetched as their own tarballs rather
# than installed, and the boxd version is read from --boxd-version, from the
# boxd.json images/boxd-source.sh writes beside the binary, or from the binary
# itself when it can run on this host.
#
# Environment (the same names images/provision.sh used for the same sources):
#   BOX_MIRROR_GITHUB   GitHub release assets (default https://github.com)
#   BOX_MIRROR_NPM      npm registry (default https://registry.npmjs.org/)
#   OUT                 output directory (default <repo>/out; --out wins)
#   SOURCE_DATE_EPOCH   seconds since the epoch to use as built_at
#   BOX_PINS_FILE       the pins to use (default images/pins.sh). It exists so
#                       that images/platform_test.sh can serve fixtures instead
#                       of the real executors; a bundle for a box uses the file
#                       in this repository.
#
# Nothing here publishes anything, and nothing kills a process.
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

log() { printf '\n==> %s\n' "$*" >&2; }
die() { printf 'platform: %s\n' "$*" >&2; exit 1; }

usage() {
  sed -n '2,59p' "$0" | sed 's/^# \{0,1\}//'
}

VERSION=
ARCH=
BOXD=
ADDONS=
OUT_ARG=
BOXD_VERSION=
BOXD_META=
BUILT_AT=

need_value() { [[ $# -ge 2 && -n $2 ]] || die "$1 needs a value"; }

while [[ $# -gt 0 ]]; do
  case $1 in
    --version) need_value "$@"; VERSION=$2; shift 2 ;;
    --arch) need_value "$@"; ARCH=$2; shift 2 ;;
    --boxd) need_value "$@"; BOXD=$2; shift 2 ;;
    --addons) need_value "$@"; ADDONS=$2; shift 2 ;;
    --out) need_value "$@"; OUT_ARG=$2; shift 2 ;;
    --boxd-version) need_value "$@"; BOXD_VERSION=$2; shift 2 ;;
    --boxd-meta) need_value "$@"; BOXD_META=$2; shift 2 ;;
    --built-at) need_value "$@"; BUILT_AT=$2; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown argument '$1' (see --help)" ;;
  esac
done

[[ -n $VERSION ]] || die "usage: images/platform.sh --version V --arch amd64|arm64 --boxd PATH"
[[ $VERSION =~ ^[0-9A-Za-z][0-9A-Za-z._-]*$ ]] || die "version '$VERSION' is not a safe path component"
case $ARCH in
  amd64|arm64) ;;
  "") die "--arch is required (amd64 or arm64)" ;;
  *) die "unsupported arch '$ARCH' (want amd64 or arm64)" ;;
esac
[[ -n $BOXD ]] || die "--boxd is required: the boxd binary for linux/$ARCH (for example from images/boxd-source.sh or deploy/build.sh)"
[[ -f $BOXD ]] || die "--boxd '$BOXD' is not a file"
[[ -r $BOXD ]] || die "--boxd '$BOXD' is not readable"

OUT=${OUT_ARG:-${OUT:-$ROOT/out}}
PINS=${BOX_PINS_FILE:-$ROOT/images/pins.sh}
[[ -r $PINS ]] || die "the pin file '$PINS' is not readable"

GITHUB_URL=${BOX_MIRROR_GITHUB:-https://github.com}
GITHUB_URL=${GITHUB_URL%/}
NPM_REGISTRY=${BOX_MIRROR_NPM:-https://registry.npmjs.org/}
NPM_REGISTRY=${NPM_REGISTRY%/}/

# The pins: versions and digests of the executors this bundle carries. The file
# is per architecture, so it is read with BOX_ARCH set.
BOX_ARCH=$ARCH
# shellcheck source=images/pins.sh
. "$PINS"

# built_at: an input like any other, so that a rebuild gives the same bytes.
if [[ -z $BUILT_AT ]]; then
  if [[ -n ${SOURCE_DATE_EPOCH:-} ]]; then
    BUILT_AT=$(date -u -d "@$SOURCE_DATE_EPOCH" +%Y-%m-%dT%H:%M:%SZ) \
      || die "SOURCE_DATE_EPOCH=$SOURCE_DATE_EPOCH is not a number of seconds"
  else
    BUILT_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  fi
fi
[[ $BUILT_AT =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]] \
  || die "built_at '$BUILT_AT' is not an RFC3339 UTC time (2026-01-01T00:00:00Z)"

WORK=$(mktemp -d "${TMPDIR:-/tmp}/box-platform-XXXXXX")
STAGE=$WORK/bundle
mkdir -p "$STAGE/bin" "$STAGE/tools" "$WORK/download"
cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT INT TERM

mkdir -p "$OUT"
[[ -d $OUT ]] || die "cannot create the output directory $OUT"

log "platform bundle $VERSION linux/$ARCH (built_at $BUILT_AT)"
log "executors pinned in $PINS: prime-agent $PRIME_AGENT_VERSION ($PRIME_AGENT_ARCH), opencode $OPENCODE_VERSION ($OPENCODE_NPM_PKG)"

# --- fetching ----------------------------------------------------------------
download() { # download URL DEST
  local url=$1 dest=$2
  # http is allowed for a mirror on a private network; the pin below is what
  # decides whether the bytes are the ones this recipe expects.
  curl --fail --silent --show-error --location --retry 3 --retry-delay 2 \
       --proto '=https,http' --proto-redir '=https,http' --tlsv1.2 \
       -o "$dest.part" "$url" \
    || die "cannot download $url"
  mv "$dest.part" "$dest"
}

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d' ' -f1
  else
    shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

verify_sha256() { # verify_sha256 FILE WANT LABEL
  local got
  got=$(sha256_of "$1")
  [[ $got == "$2" ]] || die "$3 has sha256 $got, the pin says $2"
  log "sha256 verified: $3 $got"
}

# verify_npm_integrity FILE INTEGRITY -- npm's own digest form, sha512-<base64>.
verify_npm_integrity() {
  python3 - "$1" "$2" <<'PY' || exit 1
import base64, hashlib, sys

path, want = sys.argv[1], sys.argv[2]
algorithm, _, expected = want.partition("-")
if algorithm not in ("sha512", "sha256", "sha1") or not expected:
    sys.exit(f"the npm integrity '{want}' is not <algorithm>-<base64>")
digest = hashlib.new(algorithm)
with open(path, "rb") as f:
    for chunk in iter(lambda: f.read(1 << 20), b""):
        digest.update(chunk)
got = base64.b64encode(digest.digest()).decode()
if got != expected:
    sys.exit(f"{path} has {algorithm} {got}, the registry says {expected}")
print(f"{algorithm} verified: {path}", file=sys.stderr)
PY
}

# --- the executors -------------------------------------------------------------
# prime-agent: one release tarball per architecture, a sha256 pin each. Its
# package.json sits beside the binary and the executor reads it from there, so
# the whole directory is carried, not just the binary.
log "prime-agent $PRIME_AGENT_VERSION"
PRIME_ASSET=PrimeIntellect-ai/prime-agent/releases/download/v$PRIME_AGENT_VERSION/prime-agent-$PRIME_AGENT_VERSION-linux-$PRIME_AGENT_ARCH.tar.gz
download "$GITHUB_URL/$PRIME_ASSET" "$WORK/download/prime-agent.tar.gz"
verify_sha256 "$WORK/download/prime-agent.tar.gz" "$PRIME_AGENT_SHA256" "prime-agent $PRIME_AGENT_VERSION linux/$PRIME_AGENT_ARCH"
install -d -m 0755 "$STAGE/tools/prime-agent"
tar -xzf "$WORK/download/prime-agent.tar.gz" -C "$STAGE/tools/prime-agent" \
  || die "cannot unpack the prime-agent tarball"
PRIME_BIN=$(find "$STAGE/tools/prime-agent" -maxdepth 2 -type f -name prime-agent -print -quit)
[[ -n $PRIME_BIN ]] || die "the prime-agent tarball has no prime-agent binary"
chmod 0755 "$PRIME_BIN"
[[ -f $(dirname "$PRIME_BIN")/package.json ]] \
  || log "note: the prime-agent tarball has no package.json beside its binary"

# opencode: the npm package that carries the binary of this architecture, fetched
# from the registry as a tarball. The launcher package (@opencode/cli) installs it
# through a postinstall script, which needs the target CPU; the per-arch package
# is the same bytes without that step, and one platform instead of four.
log "opencode $OPENCODE_VERSION ($OPENCODE_NPM_PKG)"
META_URL=$NPM_REGISTRY/${OPENCODE_NPM_PKG}/$OPENCODE_VERSION
download "$META_URL" "$WORK/download/opencode-meta.json"
NPM_TARBALL_URL=$(python3 - "$WORK/download/opencode-meta.json" "$OPENCODE_VERSION" "$OPENCODE_NPM_PKG" <<'PY'
import json, sys

doc = json.load(open(sys.argv[1]))
want_version, want_name = sys.argv[2], sys.argv[3]
if doc.get("name") != want_name:
    sys.exit(f"the registry answered for {doc.get('name')!r}, want {want_name!r}")
if doc.get("version") != want_version:
    sys.exit(f"the registry has version {doc.get('version')!r}, the pin says {want_version!r}")
dist = doc.get("dist") or {}
tarball = dist.get("tarball")
if not tarball:
    sys.exit("the registry metadata carries no dist.tarball")
print(tarball)
PY
) || die "cannot read the npm metadata at $META_URL"
download "$NPM_TARBALL_URL" "$WORK/download/opencode.tgz"
# The pin is the registry's own integrity for this version: the same check npm
# makes, made here so that an arm64 bundle can be built on an amd64 host.
if [[ -n ${OPENCODE_INTEGRITY:-} ]]; then
  verify_npm_integrity "$WORK/download/opencode.tgz" "$OPENCODE_INTEGRITY"
else
  log "no pin for the opencode package; trusting the registry's metadata only"
fi
install -d -m 0755 "$STAGE/tools/opencode"
# npm tarballs carry everything under package/.
tar -xzf "$WORK/download/opencode.tgz" -C "$STAGE/tools/opencode" --strip-components=1 \
  || die "cannot unpack the opencode npm package"
OPENCODE_BIN=$(find "$STAGE/tools/opencode" -maxdepth 3 -type f -name opencode -print -quit)
[[ -n $OPENCODE_BIN ]] || die "the opencode package has no opencode binary"
chmod 0755 "$OPENCODE_BIN"

# --- the links the guest runs --------------------------------------------------
# Every link is relative and points into the bundle: bin/<name> -> ../tools/<name>/...
link_tool() { # link_tool NAME BINARY
  local name=$1 binary=$2 target
  target=$(python3 - "$STAGE/bin" "$binary" <<'PY'
import os, sys
bindir, binary = sys.argv[1], sys.argv[2]
print(os.path.relpath(binary, bindir))
PY
)
  ln -sfn "$target" "$STAGE/bin/$name"
  log "bin/$name -> $target"
}
link_tool prime-agent "$PRIME_BIN"
link_tool opencode "$OPENCODE_BIN"

# --- boxd ----------------------------------------------------------------------
# The binary is the one --boxd names; its architecture is checked against --arch,
# because a bundle of the wrong boxd is a guest that cannot start its daemon.
log "boxd $BOXD"
python3 - "$BOXD" "$ARCH" <<'PY' || die "$BOXD is not a linux/$ARCH ELF binary"
import struct, sys

path, want = sys.argv[1], sys.argv[2]
with open(path, "rb") as f:
    head = f.read(20)
if len(head) < 20 or head[:4] != b"\x7fELF":
    sys.exit(f"{path} is not an ELF binary")
if head[4] != 2:
    sys.exit(f"{path} is not a 64-bit ELF binary (only a 64-bit guest is built for)")
if head[5] != 1:
    sys.exit(f"{path} is not a little-endian ELF binary")
machine = struct.unpack("<H", head[18:20])[0]
machines = {"amd64": 0x3E, "arm64": 0xB7}
if machine != machines[want]:
    names = {v: k for k, v in machines.items()}
    sys.exit(f"{path} is an ELF for {names.get(machine, hex(machine))}, want {want}")
print(f"ELF machine verified: {want}", file=sys.stderr)
PY
install -m 0755 "$BOXD" "$STAGE/bin/boxd"
ln -sfn boxd "$STAGE/bin/boxctl"

# The version the manifest records. The binary of another architecture cannot be
# asked, so the answer comes from the flag, from the record images/boxd-source.sh
# wrote beside the binary, or from the binary itself when it runs here.
boxd_version_from_meta() { # boxd_version_from_meta FILE
  python3 - "$1" <<'PY'
import json, sys
try:
    doc = json.load(open(sys.argv[1]))
except Exception as exc:
    sys.exit(f"cannot read {sys.argv[1]}: {exc}")
version = doc.get("version")
if not isinstance(version, str) or not version:
    sys.exit(f"{sys.argv[1]} records no version")
print(version)
PY
}
if [[ -n $BOXD_VERSION ]]; then
  log "boxd version $BOXD_VERSION (--boxd-version)"
elif [[ -n $BOXD_META ]]; then
  BOXD_VERSION=$(boxd_version_from_meta "$BOXD_META") || die "cannot read the boxd record $BOXD_META"
  log "boxd version $BOXD_VERSION (from $BOXD_META)"
elif [[ -f $(dirname "$BOXD")/boxd.json ]]; then
  BOXD_META=$(dirname "$BOXD")/boxd.json
  BOXD_VERSION=$(boxd_version_from_meta "$BOXD_META") || die "cannot read the boxd record $BOXD_META"
  log "boxd version $BOXD_VERSION (from $BOXD_META)"
else
  BOXD_VERSION=$("$BOXD" --version 2>/dev/null | head -1 | tr -d '\r') || true
  [[ -n $BOXD_VERSION ]] \
    || die "the bundle must record the boxd version: pass --boxd-version V, or give the binary a boxd.json beside it (images/boxd-source.sh writes one), or a boxd of this host's architecture"
  BOXD_VERSION=$(printf '%s' "$BOXD_VERSION" | tr -c 'A-Za-z0-9._+-' '_' | sed -e 's/^_*//' -e 's/_*$//')
  log "boxd version $BOXD_VERSION (from $BOXD --version)"
fi

# --- addons ---------------------------------------------------------------------
# The operator's extra files, packed by images/addons-pack.sh so that the rules
# are the ones the image used: regular files and directories only, sorted,
# owner 0 and mtime 0, modes normalised.
ADDONS_JSON=null
ADDONS_SUMMARY=
if [[ -n $ADDONS ]]; then
  log "addons from $ADDONS"
  ADDONS_PACKED=$("$ROOT/images/addons-pack.sh" "$ADDONS" "$WORK/download/addons.tar") \
    || die "images/addons-pack.sh could not pack --addons $ADDONS"
  install -d -m 0755 "$STAGE/addons"
  tar -xpf "$WORK/download/addons.tar" -C "$STAGE/addons" \
    || die "cannot unpack the packed addons"
  ADDONS_JSON=$(python3 - "$ADDONS_PACKED" <<'PY'
import json, sys
packed = json.loads(sys.argv[1])
doc = {"files": packed["files"], "bytes": packed["bytes"], "sha256": packed["sha256"]}
print(json.dumps(doc))
PY
) || die "images/addons-pack.sh did not print the JSON it documents"
  ADDONS_SUMMARY=$ADDONS_PACKED
  log "addons: $ADDONS_SUMMARY"
else
  log "no addons: --addons is not set, the bundle has no addons/ directory"
fi

# --- manifest -------------------------------------------------------------------
log "manifest"
python3 - "$STAGE/manifest.json" "$VERSION" "$ARCH" "$BOXD_VERSION" "$BUILT_AT" \
  "$PRIME_AGENT_VERSION" "$OPENCODE_VERSION" "$ADDONS_JSON" <<'PY' || die "cannot write the manifest"
import json, sys

path, version, arch, boxd, built_at, prime, opencode, addons = sys.argv[1:9]
doc = {
    "schema": 1,
    "version": version,
    "arch": arch,
    "boxd": boxd,
    "tools": {"prime-agent": prime, "opencode": opencode},
    "addons": json.loads(addons),
    "built_at": built_at,
}
with open(path, "w") as f:
    json.dump(doc, f, indent=2, sort_keys=False)
    f.write("\n")
PY
cat "$STAGE/manifest.json" >&2

# --- what the archive promises --------------------------------------------------
# Every symlink is relative and lands inside the tree: the guest mounts /opt/box
# read-only, so a link that escapes would point at something the operator never
# chose -- or at nothing.
python3 - "$STAGE" <<'PY' || die "the bundle holds a symlink that leaves it"
import os, sys

root = os.path.abspath(sys.argv[1])
bad = []
for dirpath, dirnames, filenames in os.walk(root):
    for name in dirnames + filenames:
        path = os.path.join(dirpath, name)
        if not os.path.islink(path):
            continue
        target = os.readlink(path)
        rel = os.path.relpath(path, root)
        if os.path.isabs(target):
            bad.append(f"{rel} -> {target} (absolute)")
            continue
        resolved = os.path.normpath(os.path.join(os.path.dirname(rel), target))
        if resolved == ".." or resolved.startswith("../"):
            bad.append(f"{rel} -> {target} (outside the bundle)")
if bad:
    sys.exit("symlinks that leave the bundle:\n  " + "\n  ".join(bad))
PY
# The tree holds no socket, fifo or device: a bundle is files, and a mount of it
# is read-only.
ODD=$(find "$STAGE" ! -type f ! -type d ! -type l -printf '%y %p\n')
[[ -z $ODD ]] || die "the bundle holds entries that are not a file, a directory or a symlink: $ODD"

# --- the archive ----------------------------------------------------------------
ARCHIVE=$OUT/box-platform-$VERSION-linux-$ARCH.tar.gz
rm -f "$ARCHIVE.part" "$ARCHIVE"
log "packing $ARCHIVE"
# Sorted entries, owner and group 0, mtime 0, normalised modes, and gzip without
# a name or a timestamp: the same inputs give the same bytes.
( cd "$STAGE" && tar --sort=name --owner=0 --group=0 --numeric-owner --mtime=@0 \
      --mode='u+rwX,go+rX,go-w' --format=gnu -cf - . | gzip -n -9 ) > "$ARCHIVE.part" \
  || die "cannot write $ARCHIVE"
[[ -s $ARCHIVE.part ]] || die "the archive is empty: $ARCHIVE.part"
mv "$ARCHIVE.part" "$ARCHIVE"
( cd "$OUT" && sha256sum "$(basename "$ARCHIVE")" > "$(basename "$ARCHIVE").sha256" ) \
  || die "cannot write the .sha256 of $ARCHIVE"
SHA=$(cut -d' ' -f1 "$ARCHIVE.sha256")
SIZE=$(stat -c %s "$ARCHIVE")
[[ $SHA =~ ^[0-9a-f]{64}$ ]] || die "cannot hash $ARCHIVE"

# The index entry, as one line on stdout: images/publish.sh --platform reads it,
# and a person reading a build log sees what was built.
python3 - "$VERSION" "$ARCH" "$(basename "$ARCHIVE")" "$SHA" "$SIZE" "$BUILT_AT" "$BOXD_VERSION" \
  "$PRIME_AGENT_VERSION" "$OPENCODE_VERSION" <<'PY'
import json, sys

version, arch, name, sha, size, built_at, boxd, prime, opencode = sys.argv[1:10]
print(json.dumps({
    "version": version,
    "arch": arch,
    "file": name,
    "sha256": sha,
    "size": int(size),
    "built_at": built_at,
    "boxd": boxd,
    "tools": {"prime-agent": prime, "opencode": opencode},
}))
PY

log "built $ARCHIVE"
log "sha256 $SHA"
log "size   $SIZE bytes"
