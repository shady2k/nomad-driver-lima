#!/usr/bin/env bash
# images/addons-pack.sh -- pack a directory of add-ons into one deterministic tar.
#
# An operator sometimes needs extra files in every box: a package of code or data
# that the software in the guest loads from a local path. That is not a
# credential, so it does not belong in the box-executors Nomad variable (64 KiB,
# meant for credentials); it is an input to the image instead. This script turns
# a directory on the build host into the one tar that images/build.sh copies into
# the build VM and extracts at /opt/box/addons.
#
# Nothing here knows what the files are. Whatever is below SRC_DIR -- one
# program's package, data files, several unrelated trees -- is packed as it is:
# the recipe, boxd and the driver never look inside, and nothing here is named
# after a particular piece of guest software. Each program that uses the files is
# pointed at the installed path by its own configuration, which the operator
# supplies with the box's executor configuration (docs/guest-image.md).
#
# Usage: images/addons-pack.sh SRC_DIR OUT_TAR
#   SRC_DIR   a directory on this host. Its contents become /opt/box/addons/ in
#             the image, not the directory itself: SRC_DIR/foo/index.js is
#             /opt/box/addons/foo/index.js.
#   OUT_TAR   the tar to write; extract it with -C /opt/box/addons.
#
# The archive is deterministic: entries sorted by name, owner and group 0, mtime
# 0, directories 0755, files 0644, and 0755 for a file that had any execute bit.
# One tree gives the same archive bytes on every host, so the digest the manifest
# records means the same thing everywhere.
#
# Only regular files and directories are packed. A symlink, socket, fifo or
# device is refused by name -- inside the image it would point at something the
# operator never chose -- and so is an empty SRC_DIR.
#
# stdout carries one JSON object and nothing else (the log goes to stderr):
#   {"files": N, "bytes": TOTAL, "sha256": "<sha256 of the listing>"}
# The listing is one line per regular file, "<sha256 of the file>  <path>", the
# path relative to SRC_DIR and the lines sorted: that covers the tree's contents
# and its shape without the tar's own headers, which are what a reader would have
# to unpack to rebuild. images/build.sh records the object in the build manifest
# and in /etc/box/image.json.
#
# GNU tar is required. macOS ships BSD tar, so install gnu-tar (it provides gtar)
# and it is used instead. For the hashes sha256sum is used, or shasum -a 256
# where there is no sha256sum, the same fallback as deploy/build.sh.
set -euo pipefail

log() { printf '==> %s\n' "$*" >&2; }
die() { printf 'addons-pack: %s\n' "$*" >&2; exit 1; }

case ${1:-} in
  -h | --help)
    sed -n '2,43p' "$0" | sed 's/^# \{0,1\}//'
    exit 0
    ;;
esac
[[ $# -eq 2 ]] || die "usage: images/addons-pack.sh SRC_DIR OUT_TAR"

SRC_ARG=${1:-}
OUT_TAR=${2:-}
[[ -n $SRC_ARG ]] || die "SRC_DIR is empty"
[[ -n $OUT_TAR ]] || die "OUT_TAR is empty"
[[ -d $SRC_ARG ]] || die "SRC_DIR '$SRC_ARG' is not a directory"
# pwd resolves a relative path and a symlinked SRC_DIR: the paths below are
# stripped of this prefix, and tar is given a real directory to read.
SRC_DIR=$(cd "$SRC_ARG" && pwd -P) || die "cannot enter SRC_DIR '$SRC_ARG'"

# The two tools are resolved before anything is written: a host without GNU tar
# cannot make the archive this script promises.
find_gnu_tar() {
  local candidate
  for candidate in tar gtar; do
    command -v "$candidate" >/dev/null 2>&1 || continue
    if "$candidate" --version 2>/dev/null | grep -q 'GNU tar'; then
      printf '%s\n' "$candidate"
      return 0
    fi
  done
  return 1
}
TAR=$(find_gnu_tar) \
  || die "GNU tar is required to write this archive (macOS: install gnu-tar, which provides gtar): BSD tar cannot sort the entries or force the owner"

if command -v sha256sum >/dev/null 2>&1; then
  SHA=(sha256sum)
elif command -v shasum >/dev/null 2>&1; then
  SHA=(shasum -a 256)
else
  die "neither sha256sum nor shasum is installed"
fi

# An empty directory would install nothing while announcing that it had.
shopt -s nullglob dotglob
entries=("$SRC_DIR"/*)
shopt -u nullglob dotglob
((${#entries[@]} > 0)) || die "SRC_DIR '$SRC_DIR' is empty: put the files to install at /opt/box/addons in it"

# Anything that is not a regular file or a directory is refused by name: a
# symlink resolves inside the image to something this host chose by accident, and
# a socket, fifo or device has no meaning in an image that gets copied around.
if ! ODD=$(find "$SRC_DIR" ! -type f ! -type d -print); then
  die "cannot read SRC_DIR '$SRC_DIR'"
fi
if [[ -n $ODD ]]; then
  die "SRC_DIR '$SRC_DIR' holds entries that are not a regular file or a directory: $(printf '%s' "$ODD" | tr '\n' ' ')"
fi

# --- the listing the digest covers --------------------------------------------
LISTING=$(mktemp "${TMPDIR:-/tmp}/addons-listing-XXXXXX")
trap 'rm -f "$LISTING"' EXIT

FILES=0
BYTES=0
while IFS= read -r -d '' path; do
  FILES=$((FILES + 1))
  BYTES=$((BYTES + $(wc -c <"$path" | tr -d ' ')))
done < <(find "$SRC_DIR" -type f -print0)

# One line per regular file, "./" stripped: sha256sum's own line with the path
# made relative to SRC_DIR, sorted in the C collation so a host's locale cannot
# change the digest.
while IFS= read -r -d '' path; do
  rel=${path#"$SRC_DIR"}
  rel=${rel#/}
  line=$("${SHA[@]}" -- "$path") || die "cannot hash '$path'"
  printf '%s  %s\n' "${line%%  *}" "$rel"
done < <(find "$SRC_DIR" -type f -print0) | LC_ALL=C sort >"$LISTING"

line=$("${SHA[@]}" -- "$LISTING") || die "cannot hash the listing of '$SRC_DIR'"
LISTING_SHA=${line%%  *}
[[ $LISTING_SHA =~ ^[0-9a-f]{64}$ ]] || die "cannot hash the listing of '$SRC_DIR'"

# --- the archive --------------------------------------------------------------
log "packing $SRC_DIR ($FILES file(s), $BYTES bytes) into $OUT_TAR"
"$TAR" -C "$SRC_DIR" \
  --sort=name --owner=0 --group=0 --numeric-owner --mtime=@0 \
  --mode='u+rwX,go+rX,go-w' \
  -cf "$OUT_TAR" . \
  || die "tar could not write $OUT_TAR"
[[ -s $OUT_TAR ]] || die "tar wrote no archive at $OUT_TAR"

printf '{"files": %d, "bytes": %d, "sha256": "%s"}\n' "$FILES" "$BYTES" "$LISTING_SHA"
