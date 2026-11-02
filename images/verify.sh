#!/usr/bin/env bash
# images/verify.sh -- boot a published image and check it.
#
# Usage: images/verify.sh <arch> <version>
#
# Downloads box-base-<version>-<arch>.qcow2 and its .sha256 from the store
# images/publish.sh uploaded them to, verifies the digest, boots the image with
# images/box-boot.yaml (the same shape of Lima configuration the box driver
# writes), and runs images/guest-checks.sh inside the guest. The output is the
# evidence for a published version: the apt sources it configures are the only
# hosts apt talks to, the package managers work, and the tools are the versions
# the manifest names.
#
# Environment:
#   BOX_STORE_URL           base URL of the store (required unless IMAGE_PATH is
#                           set); the image is read from <url>/box-base/...
#   BOX_STORE_TOKEN         a read token, for a store that needs one
#   BOX_STORE_TOKEN_FILE    a file holding that token
#   BOX_STORE_TOKEN_HEADER  the header it goes in (default Authorization)
#   BOX_STORE_TOKEN_SCHEME  its scheme (default Bearer; empty for a bare key)
#   WORK         work directory (default <repo>/.images)
#   LIMA_HOME    Lima home (default $WORK/lima-home)
#   LIMACTL      limactl binary (default: PATH, then a nix store lima 2.x)
#   QEMU_IMG     qemu-img binary (default: PATH, then a nix store qemu 11.x)
#   CPUS/MEMORY/DISK  VM size (default: 2 / 2GiB / 20GiB)
#   IMAGE_PATH   verify a local build instead of downloading the published one
#                (its .sha256 file must be next to it)
#   KEEP=1       keep the VM and the downloaded image

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

find_limactl() {
  if [[ -n ${LIMACTL:-} ]]; then printf '%s\n' "$LIMACTL"; return; fi
  if command -v limactl >/dev/null 2>&1; then command -v limactl; return; fi
  local found
  found=$(ls -d /nix/store/*-lima-2*/bin/limactl 2>/dev/null | head -1 || true)
  [[ -n $found ]] && { printf '%s\n' "$found"; return; }
  die "limactl not found: install lima 2.x or set LIMACTL"
}
LIMACTL=$(find_limactl)
find_qemu_img() {
  if [[ -n ${QEMU_IMG:-} ]]; then printf '%s\n' "$QEMU_IMG"; return; fi
  if command -v qemu-img >/dev/null 2>&1; then command -v qemu-img; return; fi
  local found
  found=$(ls -d /nix/store/*-qemu-11*/bin/qemu-img 2>/dev/null | head -1 || true)
  [[ -n $found ]] && { printf '%s\n' "$found"; return; }
  die "qemu-img not found: install qemu or set QEMU_IMG"
}
QEMU_IMG=$(find_qemu_img)
QEMU_BIN=$(dirname "$QEMU_IMG")
export PATH=$QEMU_BIN:$PATH
export LIMA_HOME
mkdir -p "$WORK/cache" "$WORK/scripts" "$LIMA_HOME"

NAME=box-verify-$ARCH-${VERSION//[^0-9A-Za-z]/-}
IMAGE_NAME=box-base-$VERSION-$ARCH.qcow2
# A store URL is only needed when there is nothing local to verify.
if [[ -z ${IMAGE_PATH:-} ]]; then
  [[ -n $BOX_STORE_URL ]] || die "BOX_STORE_URL is not set: it is the base URL of the store the image was published to (or set IMAGE_PATH to verify a local build)"
fi
IMAGE_URL=$BOX_STORE_URL/box-base/$VERSION/$ARCH/$IMAGE_NAME
SHA_URL=$IMAGE_URL.sha256
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

if [[ -n ${IMAGE_PATH:-} ]]; then
  # A local build, verified against its own .sha256 before it is published.
  log "using the local image $IMAGE_PATH"
  [[ -s $IMAGE_PATH ]] || die "IMAGE_PATH=$IMAGE_PATH does not exist"
  IMAGE=$IMAGE_PATH
  sha_file=$IMAGE.sha256
  [[ -s $sha_file ]] || sha_file=$OUT/box-base-$VERSION-$ARCH.qcow2.sha256
  [[ -s $sha_file ]] || die "no .sha256 next to $IMAGE (set OUT to the build output directory)"
  WANT_SHA=$(cut -d' ' -f1 "$sha_file")
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
fi
[[ -n $WANT_SHA ]] || die "the published .sha256 for $VERSION $ARCH is empty"
GOT_SHA=$(sha256sum "$IMAGE" | cut -d' ' -f1)
[[ $GOT_SHA == "$WANT_SHA" ]] || die "downloaded image sha256 $GOT_SHA, published $WANT_SHA"
log "sha256 verified: $GOT_SHA"

YAML=$WORK/scripts/box-boot-$ARCH.yaml
sed -e "s|__LIMA_ARCH__|$LIMA_ARCH|g" -e "s|__IMAGE_PATH__|$IMAGE|g" \
  "$ROOT/images/box-boot.yaml" > "$YAML"

log "booting $IMAGE_NAME (cpus $CPUS, memory $MEMORY, disk $DISK)"
lima start --name "$NAME" --tty=false --timeout 60m "$YAML"
[[ $(lima list --format '{{.Status}}' "$NAME") =~ ^Running ]] || die "the VM is not running"

log "running the guest checks"
lima cp "$ROOT/images/guest-checks.sh" "$NAME:/tmp/guest-checks.sh"
lima cp "$ROOT/images/no-secrets.sh" "$NAME:/tmp/no-secrets.sh"
lima shell "$NAME" sudo bash /tmp/guest-checks.sh
CHECKS_STATUS=$?

log "stopping the VM"
lima stop "$NAME"
exit $CHECKS_STATUS
