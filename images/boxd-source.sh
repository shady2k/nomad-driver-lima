#!/usr/bin/env bash
# images/boxd-source.sh -- resolve the boxd an image carries, from exactly one
# recorded source.
#
# Usage: images/boxd-source.sh <amd64|arm64> <output-dir>
#
# Writes into <output-dir>:
#   boxd        the binary for linux/<arch>, static (CGO_ENABLED=0)
#   boxd.json   where it came from: kind, ref or tag, resolved commit, sha256,
#               version, and whether the tree it was built from was dirty
#
# The source is chosen by the environment, and only one of them may be set:
#
#   BOXD_RELEASE=vX.Y.Z   a published release: boxd_linux_<arch> and SHA256SUMS
#                         from <release base>/<repo>/releases/download/<tag>/,
#                         the digest checked against SHA256SUMS, nothing built
#   BOXD_COMMIT=<sha|ref> build ./cmd/boxd from exactly that commit of this
#                         repository (git archive into a temporary directory)
#   neither               build ./cmd/boxd from this checkout's HEAD commit; the
#                         tree must have no uncommitted change under cmd/ or
#                         internal/, and BOXD_ALLOW_DIRTY=1 overrides that -- the
#                         image is then recorded as built from a dirty tree
#
# Neither BOXD_RELEASE nor BOXD_COMMIT may be combined: an image names one
# source, so that "which boxd is in this image" has one answer.
#
# Environment:
#   BOXD_REPO              repository whose releases carry boxd
#                          (default shady2k/nomad-driver-lima)
#   BOXD_RELEASE_BASE_URL  where release assets are served, for a mirror
#                          (default https://github.com)
#   BOXD_ALLOW_DIRTY=1     build the HEAD source from a dirty tree and record it
#   OUT                    log to stderr only; nothing else is written
#
# The binary is built the way deploy/build.sh builds the release: CGO_ENABLED=0,
# -trimpath, and the version stamped into internal/version.Version (a release
# asset already carries the version release.yml stamped, which is the tag).
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

log() { printf '\n==> %s\n' "$*" >&2; }
die() { printf 'boxd-source: %s\n' "$*" >&2; exit 1; }

ARCH=${1:-}
OUTDIR=${2:-}
case $ARCH in
  amd64|arm64) ;;
  "") die "usage: images/boxd-source.sh <amd64|arm64> <output-dir>" ;;
  *) die "unsupported arch '$ARCH' (want amd64 or arm64)" ;;
esac
[[ -n $OUTDIR ]] || die "usage: images/boxd-source.sh <arch> <output-dir>"

BOXD_REPO=${BOXD_REPO:-shady2k/nomad-driver-lima}
BOXD_RELEASE=${BOXD_RELEASE:-}
BOXD_COMMIT=${BOXD_COMMIT:-}
BOXD_ALLOW_DIRTY=${BOXD_ALLOW_DIRTY:-0}
BOXD_RELEASE_BASE_URL=${BOXD_RELEASE_BASE_URL:-https://github.com}
BOXD_RELEASE_BASE_URL=${BOXD_RELEASE_BASE_URL%/}

if [[ -n $BOXD_RELEASE && -n $BOXD_COMMIT ]]; then
  die "BOXD_RELEASE=$BOXD_RELEASE and BOXD_COMMIT=$BOXD_COMMIT are both set: an image carries one boxd, from one source"
fi

# A version that lands in the binary and in a file name must be a plain token:
# git describe adds -dirty and can add a branch name with a slash.
sanitize_version() {
  printf '%s' "$1" | tr -c 'A-Za-z0-9._+-' '_' | sed -e 's/^_*//' -e 's/_*$//'
}

mkdir -p "$OUTDIR"
OUTDIR=$(cd "$OUTDIR" && pwd)
BOX_IN=$OUTDIR/boxd
rm -f "$BOX_IN" "$OUTDIR/boxd.json.tmp"

# The build command of deploy/build.sh, with the same version stamping: the two
# must not drift, because boxd --version is what a box reports.
build_boxd() { # build_boxd <source-tree> <version> <output>
  local tree=$1 version=$2 out=$3
  local ldflags
  ldflags="-s -w -X github.com/shady2k/nomad-driver-lima/internal/version.Version=$version"
  ( cd "$tree" && CGO_ENABLED=0 GOOS=linux GOARCH=$ARCH GOFLAGS='' \
      "${GO:-go}" build -trimpath -buildvcs=false -ldflags "$ldflags" -o "$out" ./cmd/boxd ) \
    || die "cannot build ./cmd/boxd for linux/$ARCH from $tree"
}

# A binary of this guest's architecture runs here only when the host is a Linux
# of the same architecture; then its own version report is the recorded version.
host_runs_arch() {
  [[ $(uname -s) == Linux ]] || return 1
  local host
  case $(uname -m) in
    x86_64|amd64) host=amd64 ;;
    aarch64|arm64) host=arm64 ;;
    *) return 1 ;;
  esac
  [[ $host == "$ARCH" ]]
}

sha256_of() { sha256sum "$1" | cut -d' ' -f1; }

kind=
ref=
tag=
commit=
version=
dirty="false"
url=

if [[ -n $BOXD_RELEASE ]]; then
  # --- a published release ---------------------------------------------------
  kind="release"
  tag=$BOXD_RELEASE
  [[ $tag =~ ^[0-9A-Za-z._+-]+$ ]] || die "BOXD_RELEASE=$tag is not a plain tag name"
  if [[ $tag != v* ]]; then
    die "BOXD_RELEASE=$tag is not a release tag: the assets live under a vX.Y.Z tag (for example v1.2.3)"
  fi
  url=$BOXD_RELEASE_BASE_URL/$BOXD_REPO/releases/download/$tag
  asset=boxd_linux_$ARCH
  log "downloading $url/$asset"
  curl --fail --silent --show-error --location --retry 3 --retry-delay 2 \
       --proto '=https,http' --proto-redir '=https,http' \
       -o "$OUTDIR/$asset" "$url/$asset" \
    || die "cannot download $url/$asset"
  curl --fail --silent --show-error --location --retry 3 --retry-delay 2 \
       --proto '=https,http' --proto-redir '=https,http' \
       -o "$OUTDIR/SHA256SUMS" "$url/SHA256SUMS" \
    || die "cannot download $url/SHA256SUMS: a release asset without its digest is not usable"
  want=$(awk -v name="$asset" '$2 == name || $2 == "*" name { print $1 }' "$OUTDIR/SHA256SUMS" | head -1)
  [[ -n $want ]] || die "$url/SHA256SUMS does not list $asset"
  got=$(sha256_of "$OUTDIR/$asset")
  [[ $got == "$want" ]] || die "downloaded $asset has sha256 $got, SHA256SUMS says $want"
  log "sha256 verified: $got"
  install -m 0755 "$OUTDIR/$asset" "$BOX_IN"
  rm -f "$OUTDIR/$asset" "$OUTDIR/SHA256SUMS"
  # release.yml stamps the tag into the binaries, so the tag names the version--
  # and when the asset can run here, it is asked, and the two must agree.
  version=$(sanitize_version "${tag#v}")
  [[ -n $version ]] || die "release $tag does not name a version"
  if host_runs_arch; then
    reported=$("$BOX_IN" --version 2>/dev/null | head -1) || reported=""
    [[ -n $reported ]] || die "$BOX_IN --version reports nothing: not the release binary?"
    reported=$(sanitize_version "$reported")
    [[ $reported == "$version" ]] \
      || die "release $tag carries boxd version $reported, the tag names $version"
    log "boxd reports version $reported, as $tag names it"
  fi
  # The commit of the tag, when this repository has it: recorded as what the tag
  # resolved to, and left empty when the tag is not in this clone.
  if commit=$(git -C "$ROOT" rev-parse --verify --quiet "refs/tags/$tag^{commit}"); then
    log "tag $tag resolves to commit $commit here"
  else
    commit=
  fi
elif [[ -n $BOXD_COMMIT ]]; then
  # --- one commit of this repository -----------------------------------------
  kind="commit"
  ref=$BOXD_COMMIT
  commit=$(git -C "$ROOT" rev-parse --verify --quiet "$ref^{commit}") \
    || die "BOXD_COMMIT=$ref does not name a commit of $ROOT"
  version=$(git -C "$ROOT" describe --tags --always "$commit" 2>/dev/null || true)
  [[ -n $version ]] || version=${commit:0:12}
  version=$(sanitize_version "${version#v}")
  [[ -n $version ]] || die "commit $commit does not name a version"
  TREE=$OUTDIR/tree
  rm -rf "$TREE"
  mkdir -p "$TREE"
  log "extracting commit $commit into $TREE"
  git -C "$ROOT" archive --format=tar "$commit" | tar -x -C "$TREE" \
    || die "cannot extract commit $commit"
  log "building boxd $version from commit $commit"
  build_boxd "$TREE" "$version" "$OUTDIR/boxd.build"
  rm -rf "$TREE"
  install -m 0755 "$OUTDIR/boxd.build" "$BOX_IN"
  rm -f "$OUTDIR/boxd.build"
else
  # --- this checkout ---------------------------------------------------------
  kind="head"
  if ! commit=$(git -C "$ROOT" rev-parse --verify --quiet "HEAD^{commit}"); then
    die "the tree at $ROOT is not a git checkout: set BOXD_COMMIT or BOXD_RELEASE to name the boxd source"
  fi
  ref=$commit
  dirty_tree=$(git -C "$ROOT" status --porcelain -- cmd internal 2>/dev/null || true)
  if [[ -n $dirty_tree ]]; then
    if [[ $BOXD_ALLOW_DIRTY != 1 ]]; then
      die "the tree has uncommitted changes under cmd/ or internal/; an image must name the commit it carries:
$(printf '%s\n' "$dirty_tree" | sed 's/^/  /')
commit them, or set BOXD_COMMIT=<commit> to build a recorded one, or BOXD_ALLOW_DIRTY=1 to build this tree and record it as dirty"
    fi
    dirty="true"
    log "BOXD_ALLOW_DIRTY=1: building the working tree, which has uncommitted changes under cmd/ or internal/"
  fi
  version=$(git -C "$ROOT" describe --tags --always 2>/dev/null || true)
  [[ -n $version ]] || version=${commit:0:12}
  # -dirty only when the code itself is uncommitted: a change to images/ or docs/
  # is not a different boxd.
  [[ $dirty == true ]] && version=$version-dirty
  version=$(sanitize_version "${version#v}")
  [[ -n $version ]] || die "the checkout does not name a version"
  log "building boxd $version from HEAD $commit"
  build_boxd "$ROOT" "$version" "$OUTDIR/boxd.build"
  install -m 0755 "$OUTDIR/boxd.build" "$BOX_IN"
  rm -f "$OUTDIR/boxd.build"
fi

BOX_SHA=$(sha256_of "$BOX_IN")
[[ -s $BOX_IN ]] || die "the resolved boxd binary is empty"
log "boxd $BOX_IN ($(stat -c %s "$BOX_IN") bytes, sha256 $BOX_SHA)"

# The record of the source. build.sh puts this into the image manifest and into
# the image itself (/etc/box/image.json), so "which boxd is in this image" is
# answered by the image and by the manifest, the same way.
python3 - "$OUTDIR/boxd.json.tmp" <<PY
import json, sys
record = {
    "kind": "$kind",
    "repository": "$BOXD_REPO",
    "ref": "${ref:-}",
    "tag": "${tag:-}",
    "resolved_commit": "${commit:-}",
    "url": "${url:-}",
    "version": "$version",
    "sha256": "$BOX_SHA",
    "size": $(stat -c %s "$BOX_IN"),
    "dirty": "$([[ $dirty == true ]] && echo true || echo false)" == "true",
}
json.dump(record, open(sys.argv[1], "w"), indent=2, sort_keys=False)
open(sys.argv[1], "a").write("\n")
PY
mv "$OUTDIR/boxd.json.tmp" "$OUTDIR/boxd.json"
cat "$OUTDIR/boxd.json" >&2
