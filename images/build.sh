#!/usr/bin/env bash
# images/build.sh -- build the box base image for one architecture.
#
# Usage: images/build.sh <arch> <version>
#   arch     amd64 | arm64
#   version  image version, e.g. 1.0.0 (becomes the file name)
#
# Writes into $OUT:
#   box-base-<version>-<arch>.qcow2            the image (qcow2, compressed,
#                                              8 GiB virtual disk by default)
#   box-base-<version>-<arch>.sha256           its sha256, in sha256sum format
#   box-base-<version>-<arch>.json             build manifest: the tool versions,
#                                              every source the build used, the
#                                              platform bundle the image expects
#   box-base-<version>-<arch>.report.txt       the provisioning report
#   box-base-<version>-<arch>.guest-checks.txt the in-guest checks and their output
#
# The image carries the same record of its own build at /etc/box/image.json, so
# a booted box answers "what am I" without the manifest at hand.
#
# The image holds no boxd and no executor: boxd and the executors are a versioned
# platform bundle that the node mounts read-only at /opt/box before the guest
# boots (images/platform.sh builds one, docs/guest-image.md is the contract).
# The image provides the stub boxd.service that runs /opt/box/bin/boxd, whose
# contents are the boxd of the source below printed with "-print-unit".
#
# This repository publishes no image. This script builds one for the operator's
# own infrastructure; images/publish.sh puts it into their store, and
# images/verify.sh proves a built or published one boots and passes its checks.
# docs/guest-image.md is the contract an image has to meet, and what follows it
# instead of this recipe.
#
# The build is a Lima instance of images/box-build.yaml whose whole recipe is
# images/provision.sh; the disk is then flattened into a single qcow2. The stub
# boxd.service comes from the boxd of the source below (images/boxd-source.sh: a
# release, one commit, or this checkout), which is copied in only to print it,
# and guest/no-secrets.sh has to pass before the disk is captured. There is no
# interactive step: every input is an argument or an environment variable, so
# this script also runs unattended in CI.
#
# Environment:
#   OUT               output directory (default <repo>/out)
#   WORK              work directory (default <repo>/.images) -- holds the base
#                     image cache, the resolved boxd and the Lima home
#   LIMA_HOME         Lima home of the build VM (default $WORK/lima-home)
#   LIMACTL           limactl binary (default: PATH, then a nix store lima 2.x).
#                     An arm64 build on an x86_64 host needs a lima that carries
#                     the aarch64 guest agent (nixpkgs `lima-additional-guestagents`
#                     has it; the plain nix lima only ships x86_64 and limactl
#                     fails with "guest agent binary could not be found for
#                     Linux-aarch64"). Point LIMACTL at a copy of that tree with
#                     the extra agents in share/lima/.
#   QEMU_IMG          qemu-img binary (default: PATH, then a nix store qemu 11.x)
#   BOXD_RELEASE      vX.Y.Z: take boxd_linux_<arch> from that release of
#                     BOXD_REPO (default shady2k/nomad-driver-lima) and check it
#                     against the release's SHA256SUMS
#   BOXD_COMMIT       <sha or ref>: build ./cmd/boxd from exactly that commit
#   BOXD_ALLOW_DIRTY  1: build this checkout's working tree when it has
#                     uncommitted changes under cmd/ or internal/ (recorded as
#                     dirty); without either BOXD_RELEASE or BOXD_COMMIT the
#                     checkout's HEAD commit is the source, and a dirty tree is
#                     refused. See images/boxd-source.sh.
#   BOXD_REPO         repository whose releases carry boxd
#   BOXD_RELEASE_BASE_URL  where release assets are served (for a mirror)
# An image carries no add-ons either: extra files for the guest software are an
# input to the platform bundle (images/platform.sh --addons DIR), which is where
# the guest's /opt/box comes from.
#
#   BASE_IMAGE_URL    base cloud image (default: the public Ubuntu release image
#                     at cloud-images.ubuntu.com)
#   BASE_IMAGE_SHA256 its sha256 (default: the pinned noble release value; set
#                     both together when BOX_MIRROR_CLOUDIMAGE serves a copy)
#   BOX_MIRROR_CLOUDIMAGE  where the cloud images live
#                     (default https://cloud-images.ubuntu.com/releases/noble/release)
#   BOX_MIRROR_*      any of the recipe's other sources (apt, npm, pypi, go,
#                     go dist, github releases, node, a Docker Hub mirror); each
#                     is exported to images/provision.sh, which is where the
#                     public defaults are written down. These are deployment
#                     settings: this repository names no private host.
#   CPUS/MEMORY/DISK  build VM size (default: 4 / 4GiB / 8GiB). DISK is not only
#                     the build VM's disk: it is the virtual size of the published
#                     image, and Lima refuses to shrink a disk, so a box can grow it
#                     (disk_gib >= 8) but never start it smaller.
#   TIMEOUT           how long limactl may take to start the VM (default 120m)
#   PROVISION_TIMEOUT how long the recipe inside the VM may take, in seconds
#                     (default 7200)
#   KEEP=1            keep the build VM and the work directory for debugging
#
# Nothing here kills processes by pattern: the build VM is stopped and deleted
# by name through limactl under the explicit $LIMA_HOME of this build.

set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

log() { printf '\n==> %s\n' "$*" >&2; }
die() { printf 'build: %s\n' "$*" >&2; exit 1; }

ARCH=${1:-}
VERSION=${2:-}
case $ARCH in
  amd64|arm64) ;;
  "") die "usage: images/build.sh <amd64|arm64> <version>" ;;
  *) die "unsupported arch '$ARCH' (want amd64 or arm64)" ;;
esac
[[ -n $VERSION ]] || die "usage: images/build.sh <arch> <version>"
[[ $VERSION =~ ^[0-9A-Za-z][0-9A-Za-z._-]*$ ]] || die "version '$VERSION' is not a safe path component"

OUT=${OUT:-$ROOT/out}
WORK=${WORK:-$ROOT/.images}
LIMA_HOME=${LIMA_HOME:-$WORK/lima-home}
BOX_MIRROR_CLOUDIMAGE=${BOX_MIRROR_CLOUDIMAGE:-https://cloud-images.ubuntu.com/releases/noble/release}
CPUS=${CPUS:-4}
MEMORY=${MEMORY:-4GiB}
DISK=${DISK:-8GiB}
TIMEOUT=${TIMEOUT:-120m}
PROVISION_TIMEOUT=${PROVISION_TIMEOUT:-7200}
KEEP=${KEEP:-0}

# The digests are the ones in the release's own SHA256SUMS, so a mirror that
# serves a byte copy of the image passes the check unchanged.
case $ARCH in
  amd64)
    LIMA_ARCH=x86_64
    DEFAULT_BASE_URL=$BOX_MIRROR_CLOUDIMAGE/ubuntu-24.04-server-cloudimg-amd64.img
    DEFAULT_BASE_SHA256=04e8f2461a16a44e78a370284471f29616cdd9baf1893df25807b71f985b6c94
    ;;
  arm64)
    LIMA_ARCH=aarch64
    DEFAULT_BASE_URL=$BOX_MIRROR_CLOUDIMAGE/ubuntu-24.04-server-cloudimg-arm64.img
    DEFAULT_BASE_SHA256=50e24e1fb79e89116e939d90aa7738f7af48c442fa1bc2510131e0b39c50348e
    ;;
esac
BASE_IMAGE_URL=${BASE_IMAGE_URL:-$DEFAULT_BASE_URL}
BASE_IMAGE_SHA256=${BASE_IMAGE_SHA256:-$DEFAULT_BASE_SHA256}

# lima 2.x and qemu are not always in PATH on a build host; a nix store copy is
# the same version the box driver's integration harness uses.
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

# limactl finds the emulator as 'qemu-system-<arch>' in PATH, so the directory
# that holds qemu-img goes in front of PATH for every limactl call.
QEMU_BIN=$(dirname "$QEMU_IMG")
# Go is resolved first: the qemu directory may be /usr/bin, whose distribution
# go is older than the one go.mod asks for and would shadow it.
GO=${GO:-$(command -v go || true)}
export GO
export PATH=$QEMU_BIN:$PATH
export LIMA_HOME
mkdir -p "$OUT" "$WORK" "$LIMA_HOME" "$WORK/cache" "$WORK/scripts"

NAME=box-build-$ARCH-${VERSION//[^0-9A-Za-z]/-}
BASE_IMAGE=$WORK/cache/$(basename "$BASE_IMAGE_URL")
IMAGE_OUT=$OUT/box-base-$VERSION-$ARCH.qcow2
SHA_OUT=$IMAGE_OUT.sha256
JSON_OUT=$OUT/box-base-$VERSION-$ARCH.json
REPORT_OUT=$OUT/box-base-$VERSION-$ARCH.report.txt
CHECKS_OUT=$OUT/box-base-$VERSION-$ARCH.guest-checks.txt

lima() { LIMA_HOME=$LIMA_HOME "$LIMACTL" "$@"; }

cleanup() {
  local status=$?
  set +e
  if [[ -d $LIMA_HOME/$NAME ]]; then
    if [[ $KEEP == 1 ]]; then
      log "kept the build VM $NAME in $LIMA_HOME (KEEP=1)"
    else
      log "deleting the build VM $NAME"
      lima stop "$NAME" >/dev/null 2>&1
      lima delete -f "$NAME" >/dev/null 2>&1
    fi
  fi
  if [[ $status -ne 0 ]]; then
    log "build failed; the instance log is $LIMA_HOME/$NAME/ha.log and $LIMA_HOME/$NAME/serial*.log"
  fi
  exit $status
}
trap cleanup EXIT

# A stale instance with this name would be adopted instead of built, so it is
# removed first. Deleting by exact name through limactl stops its hostagent and
# qemu process; no process is matched by pattern anywhere in this script.
if [[ -d $LIMA_HOME/$NAME ]]; then
  log "removing the stale build VM $NAME"
  lima stop "$NAME" >/dev/null 2>&1
  lima delete -f "$NAME" >/dev/null 2>&1
fi
rm -f "$IMAGE_OUT" "$SHA_OUT" "$JSON_OUT" "$REPORT_OUT" "$CHECKS_OUT" "$CHECKS_OUT.no-secrets" "$IMAGE_OUT.tmp"

log "limactl $("$LIMACTL" --version)"
log "qemu-img $("$QEMU_IMG" --version | head -1)"
log "arch $ARCH, version $VERSION, lima home $LIMA_HOME"

# --- base image --------------------------------------------------------------
if [[ -s $BASE_IMAGE ]]; then
  log "base image: cached $BASE_IMAGE"
else
  log "downloading the base image $BASE_IMAGE_URL"
  curl -fL --retry 3 --retry-delay 2 --proto '=https' --proto-redir '=https' \
       -o "$BASE_IMAGE.part" "$BASE_IMAGE_URL"
  mv "$BASE_IMAGE.part" "$BASE_IMAGE"
fi
printf '%s  %s\n' "$BASE_IMAGE_SHA256" "$BASE_IMAGE" | sha256sum -c --status - \
  || die "the base image $BASE_IMAGE does not match $BASE_IMAGE_SHA256"
log "base image sha256 verified"

# The image has no add-ons to pack: an add-on belongs to the platform bundle
# (images/platform.sh --addons DIR), which is what mounts at /opt/box.
if [[ -n ${IMAGE_ADDONS:-} ]]; then
  die "IMAGE_ADDONS is no longer an image input: an image carries no /opt/box/addons. Pack the files into the platform bundle instead (images/platform.sh --addons DIR)"
fi

# --- provisioning input ------------------------------------------------------
# images/provision.sh is the whole recipe; build.sh only prepends the parameters,
# so the script itself stays runnable by hand inside a guest. The mirrors are
# prepended only when they are set: an unset one keeps the recipe's own public
# default, which is written down in one place, provision.sh.
PROVISION=$WORK/scripts/provision-$ARCH-$VERSION.sh
{
  printf '#!/bin/bash\n'
  printf '# Generated by images/build.sh from images/provision.sh -- do not edit.\n'
  printf 'export BOX_ARCH=%q\n' "$ARCH"
  printf 'export BOX_VERSION=%q\n' "$VERSION"
  for mirror in APT APT_SECURITY NPM PYPI GO GO_DIST GITHUB NODE DOCKER; do
    var=BOX_MIRROR_$mirror
    if [ -n "${!var:-}" ]; then
      printf 'export %s=%q\n' "$var" "${!var}"
    fi
  done
  tail -n +2 "$ROOT/images/provision.sh"
} > "$PROVISION"
chmod 0755 "$PROVISION"

YAML=$WORK/scripts/box-build-$ARCH.yaml
sed -e "s|__LIMA_ARCH__|$LIMA_ARCH|g" \
    -e "s|__IMAGE_PATH__|$BASE_IMAGE|g" \
    -e "s|__CPUS__|$CPUS|g" \
    -e "s|__MEMORY__|$MEMORY|g" \
    -e "s|__DISK__|$DISK|g" \
    -e "s|__PROVISION__|$PROVISION|g" \
    "$ROOT/images/box-build.yaml" > "$YAML"
# Without a usable /dev/kvm qemu runs the guest under TCG, where lima's
# default CPU model for a guest of the host's own architecture ("host") does not
# exist; "max" is the TCG equivalent.
if [[ ! -w /dev/kvm ]]; then
  printf 'vmOpts:\n  qemu:\n    cpuType:\n      %s: max\n' "$LIMA_ARCH" >> "$YAML"
  log "no usable /dev/kvm: the guest runs under TCG with cpuType $LIMA_ARCH=max"
fi

# --- boxd --------------------------------------------------------------------
# boxd comes from exactly one recorded source (images/boxd-source.sh): a
# release asset, one commit, or this checkout's HEAD. Nothing here builds "the
# binary at hand" without recording where it came from, and nothing falls back
# from one source to another.
log "resolving the boxd source"
BOXD_DIR=$WORK/boxd
rm -rf "$BOXD_DIR"
mkdir -p "$BOXD_DIR"
"$ROOT/images/boxd-source.sh" "$ARCH" "$BOXD_DIR"
BOXD=$BOXD_DIR/boxd
BOXD_JSON=$BOXD_DIR/boxd.json
[[ -x $BOXD ]] || die "images/boxd-source.sh wrote no binary at $BOXD"
[[ -s $BOXD_JSON ]] || die "images/boxd-source.sh wrote no record at $BOXD_JSON"

# --- build -------------------------------------------------------------------
STARTED_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)
log "starting the build VM $NAME (this runs the whole recipe; it takes a while)"
if ! lima start --name "$NAME" --tty=false --timeout "$TIMEOUT" "$YAML"; then
  # qemu's own complaint is in the hostagent's stderr, which the instance
  # takes with it when it is deleted: show it now.
  if [[ -s $LIMA_HOME/$NAME/ha.stderr.log ]]; then
    log "the last lines of $LIMA_HOME/$NAME/ha.stderr.log"
    tail -n 30 "$LIMA_HOME/$NAME/ha.stderr.log" | sed 's/^/    /' >&2
  fi
  # Lima gives its "boot scripts must have finished" readiness probe a deadline
  # of its own, which a slow guest (an emulated aarch64 VM) can miss while the
  # provision script is still running. The VM is up and reachable in that case;
  # the recipe's own state file below is what the build really waits for.
  lima shell "$NAME" sudo true >/dev/null 2>&1 \
    || die "limactl start failed and the guest does not answer"
  log "limactl start did not report the instance ready; the guest answers, so the build waits for the recipe instead"
fi

log "waiting for the provision script to finish"
# 'limactl start' returns when the instance is running, which is before the
# provision script has finished and says nothing about whether it succeeded.
# The recipe writes its own verdict to /var/lib/box-image-build/state, so the
# build waits for that file instead of for the VM.
STATE_FILE=/var/lib/box-image-build/state
guest_tail_report() {
  lima shell "$NAME" sudo tail -n 60 /var/lib/box-image-build/report.txt 2>/dev/null | sed 's/^/    /' >&2 || true
}
DEADLINE=$(( $(date +%s) + PROVISION_TIMEOUT ))
while :; do
  STATE=$(lima shell "$NAME" sudo cat "$STATE_FILE" 2>/dev/null | head -1 || true)
  case $STATE in
    DONE*) log "provisioning finished: $STATE"; break ;;
    FAILED*)
      guest_tail_report
      die "the provision script failed: $STATE"
      ;;
  esac
  if (( $(date +%s) > DEADLINE )); then
    guest_tail_report
    die "the provision script did not finish within ${PROVISION_TIMEOUT}s (state: ${STATE:-none})"
  fi
  sleep 5
done

# --- the stub unit ------------------------------------------------------------
# The image starts the daemon the platform bundle brings, so /usr/local/bin/boxd
# stays empty: systemd's unit is boxd's own "-print-unit" (the bundle layout:
# ExecStart=/opt/box/bin/boxd, RequiresMountsFor=/opt/box ...), written here and
# enabled for the first boot. The binary is copied in only to print it, and
# removed again: an image that carried boxd would have to be rebuilt for every
# boxd release, which is the whole reason the bundle exists. Nothing is started
# in the build VM -- the build VM is not a box, and images/verify.sh is where
# boxd really runs, out of a bundle.
log "installing the stub boxd.service"
lima cp "$BOXD" "$NAME:/tmp/boxd-for-unit"
lima shell "$NAME" sudo bash -c '
  set -e
  install -d -m 0755 /etc/systemd/system
  /tmp/boxd-for-unit -print-unit > /etc/systemd/system/boxd.service
  chmod 0644 /etc/systemd/system/boxd.service
  rm -f /tmp/boxd-for-unit
  grep -q "^ExecStart=/opt/box/bin/boxd" /etc/systemd/system/boxd.service
' || die "the boxd unit in the build VM is not the platform one (ExecStart=/opt/box/bin/boxd)"
lima shell "$NAME" sudo systemctl enable boxd.service >/dev/null 2>&1 \
  || die "cannot enable boxd.service in the build VM"
lima shell "$NAME" sudo systemctl -q is-enabled boxd.service >/dev/null 2>&1 \
  || die "boxd.service is not enabled in the build VM"

log "reading the build outputs out of the guest"
lima shell "$NAME" sudo cat /var/lib/box-image-build/report.txt > "$REPORT_OUT"
lima shell "$NAME" sudo cat /var/lib/box-image-build/guest-checks.txt > "$CHECKS_OUT"
lima shell "$NAME" sudo cat /var/lib/box-image-build/manifest.json > "$WORK/manifest.json"
[[ -s $REPORT_OUT ]] || die "the guest report is empty: the provision script did not run"
[[ -s $WORK/manifest.json ]] || die "the guest manifest is empty"
python3 -c "import json,sys; json.load(open(sys.argv[1]))" "$WORK/manifest.json" \
  || die "the guest manifest is not valid JSON"

# --- the image records its own build ------------------------------------------
# /etc/box/image.json goes into the image before it is captured: the version and
# arch, the base image it was grown from, the tool versions the recipe installed,
# the platform field that says a bundle completes this image, and the boxd whose
# unit the stub was printed from -- the same things the manifest names. A booted
# box can then answer "what am I" without the manifest at hand, and
# images/verify.sh checks the tools against it.
log "writing /etc/box/image.json into the guest"
python3 - "$WORK/manifest.json" "$BOXD_JSON" "$WORK/image.json" <<'PY' \
  || die "cannot build the image record"
import json, sys

guest = json.load(open(sys.argv[1]))
boxd = json.load(open(sys.argv[2]))
doc = {
    "schema": 1,
    "version": guest["version"],
    "arch": guest["arch"],
    "built_at": guest["built_at"],
    "base_image": {"name": guest["base_image"]},
    "kernel": guest["kernel"],
    # "mount" says the guest expects the node to mount a platform bundle at
    # /opt/box; the image itself carries neither boxd nor an executor.
    "platform": guest["platform"],
    "boxd": None,
    # Which boxd's "-print-unit" wrote the stub unit, so that a support question
    # about the unit has an answer.
    "unit_from": {
        "kind": boxd["kind"],
        "tag": boxd.get("tag", ""),
        "resolved_commit": boxd.get("resolved_commit", ""),
        "version": boxd["version"],
        "sha256": boxd["sha256"],
    },
    "tools": guest["tools"],
}
json.dump(doc, open(sys.argv[3], "w"), indent=2, sort_keys=False)
open(sys.argv[3], "a").write("\n")
PY
lima cp "$WORK/image.json" "$NAME:/tmp/image.json"
lima shell "$NAME" sudo install -d -m 0755 /etc/box
lima shell "$NAME" sudo install -m 0644 -o root -g root /tmp/image.json /etc/box/image.json
lima shell "$NAME" sudo rm -f /tmp/image.json
lima shell "$NAME" sudo cat /etc/box/image.json > "$WORK/image-in-guest.json"
cmp -s "$WORK/image.json" "$WORK/image-in-guest.json" \
  || die "the image record in the guest is not the one that was written"

# The rule the image exists under: no secret may ever be in an image. It is
# checked here, in the build VM, before the disk is captured -- an image that
# fails this is not written to $OUT at all.
log "checking that the image carries no credential"
lima cp "$ROOT/guest/no-secrets.sh" "$NAME:/tmp/no-secrets.sh"
if ! lima shell "$NAME" sudo bash /tmp/no-secrets.sh "$VERSION" | tee "$CHECKS_OUT.no-secrets"; then
  die "the build VM carries a credential (see $CHECKS_OUT.no-secrets); refusing to build an image from it"
fi
lima shell "$NAME" sudo rm -f /tmp/no-secrets.sh

# The report and the manifest are build artifacts, not part of the box: remove
# them from the guest before the disk is captured. The build account goes too:
# cloud-init must be able to create its own account with the host user's uid on
# the box's first boot (with a build account holding that uid it fails with
# "Failed to create user <name>", and Lima then never gets an SSH session).
# 'userdel -f' is what removes an account whose own SSH session is running the
# command; the session survives it long enough to print the remaining accounts.
log "removing the build account and the build artifacts from the guest"
lima shell "$NAME" sudo bash -c '
  rm -rf /var/lib/box-image-build
  userdel -f -r builder >/dev/null 2>&1 || true
  # Lima lingers the user it logs in as. Left behind, the entry makes logind fail
  # its user enumeration at every boot of the image.
  rm -f /var/lib/systemd/linger/builder
  getent passwd | awk -F: "\$3 >= 1000 && \$3 < 60000 {printf \"%s \", \$1}"
' > "$WORK/guest-users.txt" 2>/dev/null || true
if grep -q . "$WORK/guest-users.txt"; then
  die "the image still has accounts with uid 1000-59999: $(tr '\n' ' ' < "$WORK/guest-users.txt")"
fi
log "the image has no account with uid 1000-59999: cloud-init creates the box's user"

log "stopping the build VM $NAME"
lima stop "$NAME"

# --- capture -----------------------------------------------------------------
# lima 2.x keeps the instance disk in the instance directory: 'disk' (a raw
# file, the base image copied and grown) for qemu, 'diffdisk' for an overlay.
DISK_FILE=$LIMA_HOME/$NAME/disk
[[ -f $DISK_FILE ]] || DISK_FILE=$LIMA_HOME/$NAME/diffdisk
if [[ ! -f $DISK_FILE ]]; then
  DISK_FILE=$(find "$LIMA_HOME/$NAME" -maxdepth 1 -type f \( -name 'diffdisk' -o -name 'disk' -o -name '*.qcow2' -o -name '*.img' \) \
         -printf '%s %p\n' | sort -rn | head -1 | cut -d' ' -f2-)
fi
[[ -n ${DISK_FILE:-} && -f $DISK_FILE ]] || die "cannot find the disk of $NAME under $LIMA_HOME/$NAME"

log "flattening $(basename "$DISK_FILE") into $(basename "$IMAGE_OUT")"
"$QEMU_IMG" info --output json "$DISK_FILE" > "$WORK/disk-info.json"
# zlib compression, qcow2's default. zstd made the image about 40 MiB smaller,
# but Lima cannot read a zstd-compressed qcow2 when it converts the image into
# the raw disk Apple Virtualization (vz) boots: an arm64 image in zstd did not
# start on a Mac.
"$QEMU_IMG" convert -c -O qcow2 -o compat=1.1 "$DISK_FILE" "$IMAGE_OUT.tmp"
mv "$IMAGE_OUT.tmp" "$IMAGE_OUT"
( cd "$OUT" && sha256sum "$(basename "$IMAGE_OUT")" > "$(basename "$SHA_OUT")" )
SHA256=$(cut -d' ' -f1 "$SHA_OUT")
SIZE=$(stat -c %s "$IMAGE_OUT")
GUEST_VIRTUAL=$(python3 -c "import json,sys;print(json.load(open(sys.argv[1]))['virtual-size'])" "$WORK/disk-info.json")

# --- manifest ----------------------------------------------------------------
python3 - "$WORK/manifest.json" "$JSON_OUT" "$BOXD_JSON" <<PY
import json, sys
guest = json.load(open(sys.argv[1]))
boxd = json.load(open(sys.argv[3]))
manifest = {
    "version": guest["version"],
    "arch": guest["arch"],
    "file": "$(basename "$IMAGE_OUT")",
    "sha256": "$SHA256",
    "size": $SIZE,
    "built_at": guest["built_at"],
    "build_started_at": "$STARTED_AT",
    "build_host": {
        "uname": "$(uname -sr)",
        "limactl": "$("$LIMACTL" --version)",
        "qemu_img": "$("$QEMU_IMG" --version | head -1)",
    },
    "base_image": {
        "url": "$BASE_IMAGE_URL",
        "sha256": "$BASE_IMAGE_SHA256",
        "name": guest["base_image"],
    },
    "disk": {"format": "qcow2", "compressed": True, "virtual_size": $GUEST_VIRTUAL},
    "kernel": guest["kernel"],
    # "mount": the guest expects a platform bundle mounted at /opt/box, and the
    # image carries neither boxd nor an executor. images/publish.sh copies this
    # into the image's index entry, and the driver mounts a bundle only into an
    # image that says it wants one.
    "platform": guest["platform"],
    "boxd": None,
    # The boxd whose "-print-unit" wrote the image's stub unit; kind is "release",
    # "commit" or "head". The boxd that runs in a box comes from the bundle.
    "unit_from": {
        "kind": boxd["kind"],
        "tag": boxd.get("tag", ""),
        "resolved_commit": boxd.get("resolved_commit", ""),
        "version": boxd["version"],
        "sha256": boxd["sha256"],
    },
    "image_manifest": "/etc/box/image.json",
    "tools": guest["tools"],
    "sources": guest["sources"],
    "gaps": [g for g in guest.get("gaps", []) if g],
}
json.dump(manifest, open(sys.argv[2], "w"), indent=2, sort_keys=False)
open(sys.argv[2], "a").write("\n")
PY

log "built $IMAGE_OUT"
log "sha256 $SHA256"
log "size   $SIZE bytes"
printf '%s\n' "$("$QEMU_IMG" info "$IMAGE_OUT" | sed -n '1,8p')" >&2
