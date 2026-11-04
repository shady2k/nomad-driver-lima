#!/usr/bin/env bash
# images/verify.sh -- boot an image and check it.
#
# Usage: images/verify.sh <arch> <version>
#
# Takes an image -- a local build (IMAGE_PATH) or one published to the operator's
# store (BOX_STORE_URL) -- checks its digest, and boots it with a platform bundle
# (PLATFORM_ARCHIVE, or one built here with images/platform.sh) mounted read-only
# at /opt/box, the way the node mounts one into a box. It uses
# images/box-boot.yaml, the same shape of Lima configuration the box driver
# writes, and runs images/guest-checks.sh inside the guest. Every check prints
# PASS or FAIL with the value expected and the value found, and the script exits
# non-zero when any of them failed.
#
# What is checked, in the guest: it booted, cloud-init finished, boxd.service is
# active, the bundle is mounted at /opt/box and boxctl answers out of it; the
# image's tool versions are the ones its manifest records, and so are the
# bundle's (its version, its boxd and its tools); /run/box/secrets and /run/boxd
# are tmpfs; a credential written into /run/box/secrets the way the driver
# delivers it is read by boxd, is not readable by the box's default user, and is
# nowhere on the disk; a rotated credential reaches boxd through `boxctl reload`;
# guest/no-secrets.sh finds no secret in the image.
#
# Environment:
#   BOX_STORE_URL           base URL of the store (required unless IMAGE_PATH is
#                           set); the image is read from <url>/box-base/...
#   BOX_STORE_TOKEN         a read token, for a store that needs one
#   BOX_STORE_TOKEN_FILE    a file holding that token; how the operator fills it
#                           (a vault, a secret manager) is theirs
#   BOX_STORE_TOKEN_HEADER  the header it goes in (default Authorization)
#   BOX_STORE_TOKEN_SCHEME  its scheme (default Bearer; empty for a bare key)
#   IMAGE_PATH   verify a local build instead of downloading a published one
#                (its .sha256 and .json must be next to it)
#   OUT          where a local build's files are, if not next to IMAGE_PATH
#   WORK         work directory (default <repo>/.images) -- keep it short: lima
#                refuses long paths below it
#   LIMA_HOME    Lima home (default $WORK/lima-home)
#   LIMACTL      limactl binary (default: PATH, then a nix store lima 2.x)
#   QEMU_IMG     qemu-img binary (default: PATH, then a nix store qemu 11.x)
#   PLATFORM_ARCHIVE  a platform bundle to boot with (box-platform-<version>
#                -linux-<arch>.tar.gz, from images/platform.sh). Unset: one is
#                built here with images/platform.sh, from the boxd source the
#                BOXD_* settings name (images/boxd-source.sh), version $VERSION
#   PLATFORM_ADDONS   a directory to pack into the bundle built here (the same
#                input images/platform.sh takes; ignored when PLATFORM_ARCHIVE
#                is set)
#   BOXD_RELEASE, BOXD_COMMIT, BOXD_ALLOW_DIRTY, BOXD_REPO, BOXD_RELEASE_BASE_URL
#                which boxd a bundle built here carries (images/boxd-source.sh)
#   CPUS/MEMORY/DISK  VM size (default: 2 / 2GiB / 20GiB)
#   KEEP=1       keep the VM and the downloaded image
#
# Nothing here kills processes by pattern: the verify VM is stopped and deleted
# by name through limactl under the explicit $LIMA_HOME of this run.

set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

log() { printf '\n==> %s\n' "$*" >&2; }
die() { printf 'verify: %s\n' "$*" >&2; exit 1; }

ARCH=${1:-}
VERSION=${2:-}
case $ARCH in
  amd64|arm64) ;;
  "") die "usage: images/verify.sh <amd64|arm64> <version>" ;;
  *) die "unsupported arch '$ARCH' (want amd64 or arm64)" ;;
esac
[[ -n $VERSION ]] || die "usage: images/verify.sh <arch> <version>"

BOX_STORE_URL=${BOX_STORE_URL:-}
BOX_STORE_URL=${BOX_STORE_URL%/}
BOX_STORE_TOKEN=${BOX_STORE_TOKEN:-}
BOX_STORE_TOKEN_FILE=${BOX_STORE_TOKEN_FILE:-}
BOX_STORE_TOKEN_HEADER=${BOX_STORE_TOKEN_HEADER:-Authorization}
BOX_STORE_TOKEN_SCHEME=${BOX_STORE_TOKEN_SCHEME-Bearer}
OUT=${OUT:-$ROOT/out}
WORK=${WORK:-$ROOT/.images}
LIMA_HOME=${LIMA_HOME:-$WORK/lima-home}
CPUS=${CPUS:-2}
MEMORY=${MEMORY:-2GiB}
DISK=${DISK:-20GiB}
KEEP=${KEEP:-0}

case $ARCH in
  amd64) LIMA_ARCH=x86_64 ;;
  arm64) LIMA_ARCH=aarch64 ;;
esac

# --- PASS/FAIL ---------------------------------------------------------------
# Every line says what was expected and what was found, so the output is the
# evidence for a version.
CHECKS=0
FAILED=0
pass() { CHECKS=$((CHECKS + 1)); printf 'PASS  %s (expected %s, actual %s)\n' "$1" "$2" "$3"; }
fail() { CHECKS=$((CHECKS + 1)); FAILED=$((FAILED + 1)); printf 'FAIL  %s (expected %s, actual %s)\n' "$1" "$2" "$3"; }
check() { if [[ $2 == "$3" ]]; then pass "$1" "$2" "$3"; else fail "$1" "$2" "$3"; fi; }
short() { local v=$1; if [[ ${#v} -gt 80 ]]; then printf '%s...' "${v:0:77}"; else printf '%s' "$v"; fi; }

find_limactl() {
  if [[ -n ${LIMACTL:-} ]]; then printf '%s\n' "$LIMACTL"; return; fi
  if command -v limactl >/dev/null 2>&1; then command -v limactl; return; fi
  local found
  found=$(ls -d /nix/store/*-lima-2*/bin/limactl 2>/dev/null | head -1 || true)
  [[ -n $found ]] && { printf '%s\n' "$found"; return; }
  die "limactl not found: install lima 2.x or set LIMACTL"
}
LIMACTL=$(find_limactl)
[[ -x $LIMACTL ]] || die "limactl '$LIMACTL' is not executable"
find_qemu_img() {
  if [[ -n ${QEMU_IMG:-} ]]; then printf '%s\n' "$QEMU_IMG"; return; fi
  if command -v qemu-img >/dev/null 2>&1; then command -v qemu-img; return; fi
  local found
  found=$(ls -d /nix/store/*-qemu-11*/bin/qemu-img 2>/dev/null | head -1 || true)
  [[ -n $found ]] && { printf '%s\n' "$found"; return; }
  die "qemu-img not found: install qemu or set QEMU_IMG"
}
QEMU_IMG=$(find_qemu_img)
[[ -x $QEMU_IMG ]] || die "qemu-img '$QEMU_IMG' is not executable"
QEMU_BIN=$(dirname "$QEMU_IMG")
export PATH=$QEMU_BIN:$PATH
export LIMA_HOME
mkdir -p "$WORK/cache" "$WORK/scripts" "$LIMA_HOME"

NAME=box-verify-$ARCH-${VERSION//[^0-9A-Za-z]/-}
IMAGE_NAME=box-base-$VERSION-$ARCH.qcow2
MANIFEST_NAME=box-base-$VERSION-$ARCH.json
# A store URL is only needed when there is nothing local to verify.
if [[ -z ${IMAGE_PATH:-} ]]; then
  [[ -n $BOX_STORE_URL ]] || die "BOX_STORE_URL is not set: it is the base URL of the store the image was published to (or set IMAGE_PATH to verify a local build)"
fi
IMAGE_URL=$BOX_STORE_URL/box-base/$VERSION/$ARCH/$IMAGE_NAME
SHA_URL=$IMAGE_URL.sha256
MANIFEST_URL=$BOX_STORE_URL/box-base/$VERSION/$ARCH/$MANIFEST_NAME
IMAGE=$WORK/cache/$IMAGE_NAME

lima() { LIMA_HOME=$LIMA_HOME "$LIMACTL" "$@"; }

cleanup() {
  local status=$?
  set +e
  if [[ -d $LIMA_HOME/$NAME ]]; then
    if [[ $KEEP == 1 ]]; then
      log "kept the verify VM $NAME in $LIMA_HOME (KEEP=1)"
    else
      log "deleting the verify VM $NAME"
      lima stop "$NAME" >/dev/null 2>&1
      lima delete -f "$NAME" >/dev/null 2>&1
    fi
  fi
  exit $status
}
trap cleanup EXIT

if [[ -d $LIMA_HOME/$NAME ]]; then
  lima stop "$NAME" >/dev/null 2>&1
  lima delete -f "$NAME" >/dev/null 2>&1
fi

# --- the image, and the record of what it should be --------------------------
log "resolving the image and its manifest"
if [[ -n ${IMAGE_PATH:-} ]]; then
  # A local build, verified against its own .sha256 and .json before it is
  # published.
  IMAGE=$IMAGE_PATH
  [[ -s $IMAGE ]] || die "IMAGE_PATH=$IMAGE does not exist"
  for cand in "$IMAGE.sha256" "$OUT/box-base-$VERSION-$ARCH.qcow2.sha256" "$(dirname "$IMAGE")/box-base-$VERSION-$ARCH.qcow2.sha256"; do
    if [[ -s $cand ]]; then SHA_FILE=$cand; break; fi
  done
  [[ -n ${SHA_FILE:-} ]] || die "no .sha256 next to $IMAGE (set OUT to the build output directory)"
  MANIFEST=""
  for cand in "${IMAGE%.qcow2}.json" "$OUT/box-base-$VERSION-$ARCH.json"; do
    if [[ -s $cand ]]; then MANIFEST=$cand; break; fi
  done
  WANT_SHA=$(cut -d' ' -f1 "$SHA_FILE")
else
  # A read token, if the store has one, is passed the same way publish.sh
  # passes the write one: through a 0600 file that curl reads.
  CURL_AUTH=()
  if [[ -z $BOX_STORE_TOKEN && -n $BOX_STORE_TOKEN_FILE ]]; then
    [[ -r $BOX_STORE_TOKEN_FILE ]] || die "BOX_STORE_TOKEN_FILE=$BOX_STORE_TOKEN_FILE is not readable"
    BOX_STORE_TOKEN=$(< "$BOX_STORE_TOKEN_FILE")
    BOX_STORE_TOKEN=${BOX_STORE_TOKEN%$'\n'}
  fi
  if [[ -n $BOX_STORE_TOKEN ]]; then
    header_file=$WORK/cache/.read-headers
    if [[ -n $BOX_STORE_TOKEN_SCHEME ]]; then
      printf '%s: %s %s\n' "$BOX_STORE_TOKEN_HEADER" "$BOX_STORE_TOKEN_SCHEME" "$BOX_STORE_TOKEN" > "$header_file"
    else
      printf '%s: %s\n' "$BOX_STORE_TOKEN_HEADER" "$BOX_STORE_TOKEN" > "$header_file"
    fi
    chmod 0600 "$header_file"
    unset BOX_STORE_TOKEN
    CURL_AUTH=(-H @"$header_file")
  fi
  log "downloading $IMAGE_URL"
  rm -f "$IMAGE"
  curl -fL --retry 3 --proto '=https,http' --proto-redir '=https,http' -o "$IMAGE" "${CURL_AUTH[@]}" "$IMAGE_URL"
  WANT_SHA=$(curl -fsSL --retry 3 "${CURL_AUTH[@]}" "$SHA_URL" | cut -d' ' -f1)
  MANIFEST=$WORK/cache/$MANIFEST_NAME
  rm -f "$MANIFEST"
  # The manifest is the record of what the image should be; a store that lost it
  # still has the image's own copy at /etc/box/image.json, so a missing one is
  # reported and not fatal.
  if curl -fsSL --retry 3 "${CURL_AUTH[@]}" -o "$MANIFEST" "$MANIFEST_URL" 2>/dev/null; then
    log "downloaded the manifest $MANIFEST_URL"
  else
    MANIFEST=""
    log "the store has no $MANIFEST_NAME; the image's own /etc/box/image.json is the record"
  fi
fi
[[ -n $WANT_SHA ]] || die "the published .sha256 for $VERSION $ARCH is empty"
GOT_SHA=$(sha256sum "$IMAGE" | cut -d' ' -f1)
check "the image's sha256 is the digest it came with" "$WANT_SHA" "$GOT_SHA"
if [[ $FAILED -ne 0 ]]; then
  printf '\nRESULT: %d check(s), %d failed\n' "$CHECKS" "$FAILED"
  die "the image is not the one $VERSION $ARCH was published as"
fi
if [[ -n $MANIFEST ]]; then
  if python3 -c "import json,sys; json.load(open(sys.argv[1]))" "$MANIFEST" 2>/dev/null; then
    pass "the manifest is valid JSON" "valid JSON" "valid JSON"
  else
    fail "the manifest is valid JSON" "valid JSON" "invalid"
  fi
fi

# --- the platform bundle ------------------------------------------------------
# The image carries no boxd and no executor: the node downloads a platform bundle
# and mounts it read-only at /opt/box before the guest boots (docs/guest-image.md).
# This is the same thing for one boot: a bundle on this host -- the one given, or
# one built here -- unpacked into a directory of its own and mounted the way the
# driver mounts it (9p with cache fscache, read-only).
PLATFORM_ARCHIVE=${PLATFORM_ARCHIVE:-}
BUNDLE_ROOT=$WORK/bundle
BUNDLE_DIR=$BUNDLE_ROOT/tree
rm -rf "$BUNDLE_ROOT"
mkdir -p "$BUNDLE_DIR"
if [[ -n $PLATFORM_ARCHIVE ]]; then
  log "using the platform bundle $PLATFORM_ARCHIVE"
  [[ -s $PLATFORM_ARCHIVE ]] || die "PLATFORM_ARCHIVE=$PLATFORM_ARCHIVE does not exist"
  PLATFORM_ARCHIVE=$(cd "$(dirname "$PLATFORM_ARCHIVE")" && pwd)/$(basename "$PLATFORM_ARCHIVE")
else
  log "building a platform bundle for linux/$ARCH with boxd from ${BOXD_RELEASE:-${BOXD_COMMIT:-this checkout}}"
  BOXD_DIR=$BUNDLE_ROOT/boxd
  mkdir -p "$BOXD_DIR"
  "$ROOT/images/boxd-source.sh" "$ARCH" "$BOXD_DIR" || die "images/boxd-source.sh failed"
  PLATFORM_ADDONS_ARGS=()
  if [[ -n ${PLATFORM_ADDONS:-} ]]; then
    PLATFORM_ADDONS_ARGS=(--addons "$PLATFORM_ADDONS")
  fi
  "$ROOT/images/platform.sh" --version "$VERSION" --arch "$ARCH" \
    --boxd "$BOXD_DIR/boxd" --out "$BUNDLE_ROOT/out" "${PLATFORM_ADDONS_ARGS[@]}" > "$BUNDLE_ROOT/built.json" \
    || die "images/platform.sh could not build a bundle"
  cat "$BUNDLE_ROOT/built.json" >&2
  PLATFORM_ARCHIVE=$BUNDLE_ROOT/out/box-platform-$VERSION-linux-$ARCH.tar.gz
  # The boxd binary went into the archive; the tree it was built from is not
  # needed any more. The archive stays: it is what this run mounts and what the
  # report names.
  rm -rf "$BOXD_DIR"
fi
WANT_BUNDLE_SHA=$(sha256sum "$PLATFORM_ARCHIVE" | cut -d' ' -f1)
if [[ -s $PLATFORM_ARCHIVE.sha256 ]]; then
  check "the bundle's sha256 is the digest it came with" \
    "$(cut -d' ' -f1 "$PLATFORM_ARCHIVE.sha256")" "$WANT_BUNDLE_SHA"
fi
tar -xzf "$PLATFORM_ARCHIVE" -C "$BUNDLE_DIR" || die "cannot unpack $PLATFORM_ARCHIVE"
[[ -s $BUNDLE_DIR/manifest.json ]] || die "the bundle has no manifest.json"
BUNDLE_VERSION=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["version"])' "$BUNDLE_DIR/manifest.json") \
  || die "the bundle's manifest is not valid JSON"
BUNDLE_BOXD=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("boxd", ""))' "$BUNDLE_DIR/manifest.json")
BUNDLE_SCHEMA=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("schema", ""))' "$BUNDLE_DIR/manifest.json")
log "bundle $BUNDLE_VERSION (boxd ${BUNDLE_BOXD:-?}) at $BUNDLE_DIR, sha256 $WANT_BUNDLE_SHA"
check "the bundle is schema 1" "1" "$BUNDLE_SCHEMA"
check "the bundle holds the boxd it names" "present" \
  "$([[ -x $BUNDLE_DIR/bin/boxd ]] && echo present || echo missing)"

# --- boot --------------------------------------------------------------------
# box-boot.yaml is the shape of the driver's own configuration, the platform
# mount included: __MOUNTS__ is the bundle's mount. The directory is the bundle's
# own and never changes under the guest -- a new bundle is a new directory -- so
# 9p may cache it. A run always mounts one: what this verifies is the pair, an
# image that takes a bundle and a bundle that runs.
MOUNTS_YAML=$(python3 - "$BUNDLE_DIR" <<'PY'
import sys
location = sys.argv[1]
print()
print(f"- location: {location!r}")
print("  mountPoint: /opt/box")
print("  writable: false")
print("  9p:")
print("    securityModel: none")
print("    cache: fscache")
PY
)
YAML=$WORK/scripts/box-boot-$ARCH.yaml
python3 - "$ROOT/images/box-boot.yaml" "$YAML" "$LIMA_ARCH" "$IMAGE" "$MOUNTS_YAML" <<'PY'
import sys

template, out, lima_arch, image, mounts = sys.argv[1:6]
text = open(template).read()
text = text.replace("__LIMA_ARCH__", lima_arch).replace("__IMAGE_PATH__", image)
text = text.replace("__MOUNTS__", mounts)
open(out, "w").write(text)
PY
# Without a usable /dev/kvm qemu runs the guest under TCG, where lima's
# default CPU model for a guest of the host's own architecture ("host") does not
# exist; "max" is the TCG equivalent.
if [[ ! -w /dev/kvm ]]; then
  printf 'vmOpts:\n  qemu:\n    cpuType:\n      %s: max\n' "$LIMA_ARCH" >> "$YAML"
  log "no usable /dev/kvm: the guest runs under TCG with cpuType $LIMA_ARCH=max"
fi

log "booting $IMAGE_NAME (cpus $CPUS, memory $MEMORY, disk $DISK)"
if lima start --name "$NAME" --tty=false --timeout 60m "$YAML" > "$WORK/boot.log" 2>&1; then
  pass "the image boots under lima" "a running instance" "a running instance"
else
  fail "the image boots under lima" "a running instance" "limactl start failed"
  tail -20 "$WORK/boot.log" | sed 's/^/      /'
fi
if [[ -d $LIMA_HOME/$NAME ]]; then
  status=$(lima list --format '{{.Status}}' "$NAME" 2>/dev/null | head -1 || true)
  check "the instance is running" "Running" "$status"
  # cloud-init creates the box's user; the checks below are only meaningful once
  # the first boot has finished.
  lima shell "$NAME" sudo cloud-init status --wait >/dev/null 2>&1 || true
fi
if [[ $FAILED -ne 0 ]]; then
  printf '\nRESULT: %d check(s), %d failed\n' "$CHECKS" "$FAILED"
  exit 1
fi

# --- the checks inside the guest ---------------------------------------------
log "running the guest checks"
lima cp "$ROOT/images/guest-checks.sh" "$NAME:/tmp/guest-checks.sh"
lima cp "$ROOT/guest/no-secrets.sh" "$NAME:/tmp/no-secrets.sh"
GUEST_ARGS=(--platform /opt/box)
if [[ -n $MANIFEST ]]; then
  lima cp "$MANIFEST" "$NAME:/tmp/image-manifest.json"
  GUEST_ARGS+=(--manifest /tmp/image-manifest.json)
fi
set +e
lima shell "$NAME" sudo bash /tmp/guest-checks.sh "${GUEST_ARGS[@]}"
GUEST_STATUS=$?
set -e
if [[ $GUEST_STATUS -eq 0 ]]; then
  pass "every check inside the guest passed" "exit 0" "exit 0"
else
  fail "every check inside the guest passed" "exit 0" "exit $GUEST_STATUS"
fi

log "stopping the VM"
lima stop "$NAME"

printf '\n'
if [[ $FAILED -ne 0 ]]; then
  printf 'RESULT: %d check(s) in verify.sh, %d failed\n' "$CHECKS" "$FAILED"
  exit 1
fi
printf 'RESULT: verify.sh: %d checks, all passed; the guest ran its own and all passed\n' "$CHECKS"
exit 0
