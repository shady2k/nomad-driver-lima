#!/usr/bin/env bash
# images/publish.sh -- publish a built image to an HTTP PUT store.
#
# Usage: images/publish.sh <arch> <version>
#
# The store is any server that accepts PUT, GET and HEAD on a path below a base
# URL (a raw repository, an object store with an S3-style endpoint, a static
# web server with a small upload daemon in front of it). Uploads, under
# $BOX_STORE_URL:
#   box-base/<version>/<arch>/box-base-<version>-<arch>.qcow2
#   box-base/<version>/<arch>/box-base-<version>-<arch>.qcow2.sha256
#   box-base/<version>/<arch>/box-base-<version>-<arch>.json    (build manifest)
# and then updates   box-base/index.json   (versions x arches, with tool versions).
#
# A published version is immutable. The script refuses a version that already
# exists (HEAD on all three files) and there is no override: an upload usually
# cannot be taken back, and a version that boxes already pulled must keep
# meaning one image. Build a new version instead.
#
# Order, and why:
#   1. take box-base/.lock (owner and time; fresh beats stale after 30 minutes);
#   2. refuse an already published version;
#   3. upload the qcow2, its .sha256 and the build manifest;
#   4. download the qcow2 back and compare its sha256, and the two small files
#      byte for byte -- the index never names an image that did not arrive;
#   5. re-read index.json, merge this version in, write it, read it back and
#      verify that this entry and every entry that was there before survived;
#   6. release the lock.
#
# The lock is one more object in the store (box-base/.lock), not a local file:
# every publisher of the store sees it. A record says `state=held` or
# `state=released`, the owner (host and pid) and the epoch it was written; the
# holder refreshes it after every upload, and a held record older than 30
# minutes is taken over (a crashed run cannot block the store for ever). A store
# that cannot delete keeps the last record; a `released` record blocks nobody.
#
# The write token comes from the environment or from a file named by the
# environment and stays in memory: it is written into a 0600 header file, handed
# to curl as '-H @file', and neither the token nor the file path is printed. Do
# not run this script with 'bash -x'.
#
# Environment:
#   OUT                     directory holding the built image (default <repo>/out)
#   BOX_STORE_URL           base URL of the store (required); the layout above is
#                           written below it
#   BOX_STORE_TOKEN         the write token (-H "<header>: <scheme> <token>")
#   BOX_STORE_TOKEN_FILE    a file holding the token, for a secret mount; used
#                           when BOX_STORE_TOKEN is not set
#   BOX_STORE_TOKEN_HEADER  the header the store reads (default Authorization)
#   BOX_STORE_TOKEN_SCHEME  the scheme in front of the token (default Bearer;
#                           set it empty for a bare key header)
#   LOCK_STALE_SECONDS      how old a held lock may be before it is taken over
#                           (default 1800)
#
# There is no authentication at all without a token: a store that needs none
# (a local static server, an internal bucket with an allow-all PUT policy) is
# published to the same way.

set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

log() { printf '\n==> %s\n' "$*" >&2; }
die() { printf 'publish: %s\n' "$*" >&2; exit 1; }

ARCH=${1:-}
VERSION=${2:-}
case $ARCH in
  amd64|arm64) ;;
  "") die "usage: images/publish.sh <amd64|arm64> <version>" ;;
  *) die "unsupported arch '$ARCH' (want amd64 or arm64)" ;;
esac
[[ -n $VERSION ]] || die "usage: images/publish.sh <arch> <version>"
[[ $VERSION =~ ^[0-9A-Za-z][0-9A-Za-z._-]*$ ]] || die "version '$VERSION' is not a safe path component"

OUT=${OUT:-$ROOT/out}
BOX_STORE_URL=${BOX_STORE_URL:-}
BOX_STORE_URL=${BOX_STORE_URL%/}
BOX_STORE_TOKEN=${BOX_STORE_TOKEN:-}
BOX_STORE_TOKEN_FILE=${BOX_STORE_TOKEN_FILE:-}
BOX_STORE_TOKEN_HEADER=${BOX_STORE_TOKEN_HEADER:-Authorization}
BOX_STORE_TOKEN_SCHEME=${BOX_STORE_TOKEN_SCHEME-Bearer}
[[ -n $BOX_STORE_URL ]] || die "BOX_STORE_URL is not set: it is the base URL of the HTTP PUT store to publish under (an index goes to <url>/box-base/index.json)"

IMAGE=$OUT/box-base-$VERSION-$ARCH.qcow2
SHA_FILE=$IMAGE.sha256
MANIFEST=$OUT/box-base-$VERSION-$ARCH.json

[[ -s $IMAGE ]] || die "missing $IMAGE: run images/build.sh $ARCH $VERSION first"
[[ -s $SHA_FILE ]] || die "missing $SHA_FILE"
[[ -s $MANIFEST ]] || die "missing $MANIFEST"
python3 -c "import json,sys; json.load(open(sys.argv[1]))" "$MANIFEST" \
  || die "$MANIFEST is not valid JSON"
SHA256=$(cut -d' ' -f1 "$SHA_FILE")
[[ $(sha256sum "$IMAGE" | cut -d' ' -f1) == "$SHA256" ]] \
  || die "$SHA_FILE does not match $IMAGE"

REPO_URL=$BOX_STORE_URL
BASE_PATH=box-base
VERSION_PATH=$BASE_PATH/$VERSION/$ARCH
IMAGE_NAME=$(basename "$IMAGE")
SHA_NAME=$IMAGE_NAME.sha256
MANIFEST_NAME=$(basename "$MANIFEST")
INDEX_PATH=$BASE_PATH/index.json
LOCK_PATH=$BASE_PATH/.lock
LOCK_STALE_SECONDS=${LOCK_STALE_SECONDS:-1800}

WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/box-publish.XXXXXX")
chmod 0700 "$WORK_DIR"
cleanup() { rm -rf "$WORK_DIR"; }
trap cleanup EXIT INT TERM

# --- credential --------------------------------------------------------------
# The token is read from the environment or from a file the environment names,
# and it goes into a 0600 header file that only curl ever reads: it never appears
# in a process argument and never on stdout.
if [[ -z $BOX_STORE_TOKEN && -n $BOX_STORE_TOKEN_FILE ]]; then
  [[ -r $BOX_STORE_TOKEN_FILE ]] || die "BOX_STORE_TOKEN_FILE=$BOX_STORE_TOKEN_FILE is not readable"
  BOX_STORE_TOKEN=$(< "$BOX_STORE_TOKEN_FILE")
  BOX_STORE_TOKEN=${BOX_STORE_TOKEN%$'\n'}
fi
CURL_AUTH=()
if [[ -n $BOX_STORE_TOKEN ]]; then
  if [[ -n $BOX_STORE_TOKEN_SCHEME ]]; then
    printf '%s: %s %s\n' "$BOX_STORE_TOKEN_HEADER" "$BOX_STORE_TOKEN_SCHEME" "$BOX_STORE_TOKEN" > "$WORK_DIR/headers"
  else
    printf '%s: %s\n' "$BOX_STORE_TOKEN_HEADER" "$BOX_STORE_TOKEN" > "$WORK_DIR/headers"
  fi
  chmod 0600 "$WORK_DIR/headers"
  unset BOX_STORE_TOKEN
  CURL_AUTH=(-H @"$WORK_DIR/headers")
  log "sending the write token in $BOX_STORE_TOKEN_HEADER"
else
  log "no token configured: publishing without authentication"
fi

# with_auth is the only place the credential is used, and it is passed as a file
# so that it never appears in a process argument.
with_auth() { curl --fail-with-body -sS --retry 3 --retry-delay 2 "${CURL_AUTH[@]}" "$@"; }
# status_of is one request without the credential: its HTTP status on stdout.
status_of() { curl -sS -o /dev/null -w '%{http_code}' --max-time 60 "${CURL_AUTH[@]}" "$@" || true; }
# head_status is the HEAD status of one published path.
head_status() { status_of -I --max-time 30 "$1"; }

# --- lock --------------------------------------------------------------------
LOCK_OWNER="$(hostname 2>/dev/null || echo unknown)-$$"

# lock_record writes one lock document: its state, who holds it, and when.
lock_record() {
  printf 'state=%s\nowner=%s\npid=%s\nversion=%s\narch=%s\ntime=%s\n' \
    "$1" "$LOCK_OWNER" "$$" "$VERSION" "$ARCH" "$(date -u +%s)"
}

# lock_field reads one field out of a lock document.
lock_field() { sed -n "s/^$1=//p" <<<"$2" | head -1; }

# lock_put writes a lock document and reads it back, so two publishers that
# wrote at the same moment notice that one of them lost.
lock_put() {
  local body=$1 got status
  status=$(with_auth -X PUT --data-binary "$body" -o /dev/null -w '%{http_code}' \
    --max-time 60 "$REPO_URL/$LOCK_PATH" || true)
  case $status in
    2*) ;;
    *) die "cannot write the lock $REPO_URL/$LOCK_PATH (HTTP $status)" ;;
  esac
  status=$(curl -sS -o "$WORK_DIR/lock" -w '%{http_code}' --max-time 30 "${CURL_AUTH[@]}" "$REPO_URL/$LOCK_PATH" || true)
  [[ $status == 200 ]] || die "cannot read the lock back (HTTP $status): refusing to publish without it"
  got=$(cat "$WORK_DIR/lock")
  [[ $got == "$body" ]] || die "another publisher took the lock $LOCK_PATH while this one wrote it:
$got
Wait for that run to finish (or for the lock to go stale after $((LOCK_STALE_SECONDS / 60)) minutes)."
}

lock_refresh() { lock_put "$(lock_record held)"; }

lock_acquire() {
  local status current state held_for now since
  status=$(curl -sS -o "$WORK_DIR/lock" -w '%{http_code}' --max-time 30 "${CURL_AUTH[@]}" "$REPO_URL/$LOCK_PATH" || true)
  case $status in
    404) ;;
    200)
      current=$(cat "$WORK_DIR/lock")
      state=$(lock_field state "$current")
      since=$(lock_field time "$current")
      if [[ $state == held && $since =~ ^[0-9]+$ ]]; then
        now=$(date -u +%s)
        held_for=$((now - since))
        if ((held_for < LOCK_STALE_SECONDS)); then
          die "$REPO_URL/$LOCK_PATH is held by $(lock_field owner "$current") since $(date -u -d "@$since" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || echo "$since") ($((held_for / 60)) min ago): another publisher is running. Wait for it, or delete the lock once you know that run is gone."
        fi
        log "taking over a stale lock: held by $(lock_field owner "$current") for $((held_for / 60)) min (stale after $((LOCK_STALE_SECONDS / 60)) min)"
      fi
      ;;
    *) die "cannot read the lock $REPO_URL/$LOCK_PATH (HTTP $status)" ;;
  esac
  lock_put "$(lock_record held)"
  LOCK_HELD=1
  log "locked $LOCK_PATH as $LOCK_OWNER"
}

lock_release() {
  [[ ${LOCK_HELD:-0} == 1 ]] || return 0
  LOCK_HELD=0
  lock_put "$(lock_record released)" || true
}

LOCK_HELD=0
trap 'lock_release; cleanup' EXIT INT TERM

lock_acquire

# --- refuse to overwrite -----------------------------------------------------
# A version is immutable: every file of it must be absent before anything is
# uploaded. There is no replace switch on purpose.
log "checking that $VERSION $ARCH is not published yet"
for name in "$IMAGE_NAME" "$SHA_NAME" "$MANIFEST_NAME"; do
  status=$(head_status "$REPO_URL/$VERSION_PATH/$name")
  case $status in
    200) die "$REPO_URL/$VERSION_PATH/$name already exists: $VERSION $ARCH is published and immutable. Build and publish a new version instead." ;;
    404) ;;
    *) die "cannot check $REPO_URL/$VERSION_PATH/$name (HTTP $status)" ;;
  esac
done

# --- read the index (for the merge; it is read again right before writing) ----
read_index() {
  local target=$1 status
  rm -f "$target"
  status=$(curl -sS -o "$target" --max-time 60 -w '%{http_code}' "${CURL_AUTH[@]}" "$REPO_URL/$INDEX_PATH" || true)
  case $status in
    200) ;;
    404) rm -f "$target" ;;
    *) die "cannot read $REPO_URL/$INDEX_PATH (HTTP $status)" ;;
  esac
}

INDEX_SEEN=$WORK_DIR/index-seen.json
read_index "$INDEX_SEEN"
if [[ -f $INDEX_SEEN ]] && python3 - "$INDEX_SEEN" "$VERSION" "$ARCH" <<'PY'
import json, sys
try:
    doc = json.load(open(sys.argv[1]))
except Exception:
    sys.exit(0)
sys.exit(0 if sys.argv[2] in doc.get("images", {}) and sys.argv[3] in doc["images"][sys.argv[2]] else 1)
PY
then
  die "index.json already lists $VERSION $ARCH: the version is published, build a new one"
fi

# --- upload ------------------------------------------------------------------
# upload_put FILE URL LABEL -- one PUT, and a chunked retry when the store or a
# proxy in front of it refuses the declared body size.
#
# Measured on one such store: a 1 GiB image uploads, a 1.19 GiB one is answered
# with HTTP 413 after about a megabyte, because the reverse proxy applies a
# body-size limit to Content-Length. A chunked PUT carries no Content-Length, so
# the same image goes through; the server still stores exactly the bytes it
# received (the sha256 check below proves it).
upload_put() {
  local file=$1 url=$2 label=$3 code
  code=$(curl -sS -o /dev/null -w '%{http_code}' --retry 3 --retry-delay 2 \
    "${CURL_AUTH[@]}" -X PUT --upload-file "$file" "$url" || true)
  case $code in
    2*) printf 'PUT %s %s (%s bytes)\n' "$label" "$code" "$(stat -c %s "$file")"; return 0 ;;
    413)
      log "the store refused $(stat -c %s "$file") bytes (HTTP 413); retrying $label in chunked form"
      code=$(curl -sS -o /dev/null -w '%{http_code}' --retry 3 --retry-delay 2 \
        "${CURL_AUTH[@]}" -H 'Transfer-Encoding: chunked' \
        -X PUT --data-binary @- "$url" < "$file" || true)
      case $code in
        2*) printf 'PUT %s %s (chunked, %s bytes)\n' "$label" "$code" "$(stat -c %s "$file")"; return 0 ;;
      esac
      die "uploading $label failed: HTTP $code even in chunked form"
      ;;
  esac
  die "uploading $label failed: HTTP $code"
}

log "uploading $IMAGE_NAME ($(stat -c %s "$IMAGE") bytes)"
upload_put "$IMAGE" "$REPO_URL/$VERSION_PATH/$IMAGE_NAME" "$IMAGE_NAME"
lock_refresh
log "uploading $SHA_NAME"
upload_put "$SHA_FILE" "$REPO_URL/$VERSION_PATH/$SHA_NAME" "$SHA_NAME"
lock_refresh
log "uploading $MANIFEST_NAME"
upload_put "$MANIFEST" "$REPO_URL/$VERSION_PATH/$MANIFEST_NAME" "$MANIFEST_NAME"
lock_refresh

# --- verify what arrived, before the index names it --------------------------
log "downloading the published image back and comparing its sha256"
TMP_DL=$WORK_DIR/download.qcow2
curl -fL --retry 3 -sS -o "$TMP_DL" "${CURL_AUTH[@]}" "$REPO_URL/$VERSION_PATH/$IMAGE_NAME"
GOT_SHA=$(sha256sum "$TMP_DL" | cut -d' ' -f1)
[[ $GOT_SHA == "$SHA256" ]] || die "the published image has sha256 $GOT_SHA, want $SHA256: index.json was not touched"
[[ $(stat -c %s "$TMP_DL") == $(stat -c %s "$IMAGE") ]] \
  || die "the published image is $(stat -c %s "$TMP_DL") bytes, want $(stat -c %s "$IMAGE"): index.json was not touched"
rm -f "$TMP_DL"

compare_small_file() {
  local local_file=$1 remote_name=$2
  curl -fsS -o "$WORK_DIR/small" "${CURL_AUTH[@]}" "$REPO_URL/$VERSION_PATH/$remote_name" \
    || die "cannot download the published $remote_name back: index.json was not touched"
  cmp -s "$local_file" "$WORK_DIR/small" \
    || die "the published $remote_name differs from $local_file: index.json was not touched"
  rm -f "$WORK_DIR/small"
}
compare_small_file "$SHA_FILE" "$SHA_NAME"
compare_small_file "$MANIFEST" "$MANIFEST_NAME"

# --- index: re-read, merge, write, read back ---------------------------------
log "updating $INDEX_PATH"
INDEX_FRESH=$WORK_DIR/index-fresh.json
read_index "$INDEX_FRESH"
NEW_INDEX=$WORK_DIR/index-new.json
python3 - "$INDEX_FRESH" "$MANIFEST" "$NEW_INDEX" "$REPO_URL" "$VERSION_PATH" "$VERSION" "$ARCH" "$IMAGE_NAME" "$SHA_NAME" "$MANIFEST_NAME" <<'PY'
import datetime, json, os, sys

old_path, manifest_path, out_path = sys.argv[1], sys.argv[2], sys.argv[3]
repo_url, version_path, version, arch = sys.argv[4], sys.argv[5], sys.argv[6], sys.argv[7]
image_name, sha_name, manifest_name = sys.argv[8], sys.argv[9], sys.argv[10]

doc = None
if os.path.exists(old_path):
    try:
        doc = json.load(open(old_path))
    except Exception:
        doc = None
if not isinstance(doc, dict) or "images" not in doc:
    doc = {"schema": 1, "images": {}}

manifest = json.load(open(manifest_path))
now = datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")

tools = manifest.get("tools", {})
contents = {
    "base_image": manifest.get("base_image", {}).get("name", ""),
    "kernel": manifest.get("kernel", ""),
    "tools": tools,
    "sources": manifest.get("sources", {}),
    "gaps": manifest.get("gaps", []),
}
base_url = f"{repo_url}/{version_path}"
doc["images"].setdefault(version, {})[arch] = {
    "url": f"{base_url}/{image_name}",
    "sha256": manifest["sha256"],
    "sha256_url": f"{base_url}/{sha_name}",
    "size": manifest["size"],
    "built_at": manifest["built_at"],
    "manifest_url": f"{base_url}/{manifest_name}",
    "disk": manifest.get("disk", {}),
    "contents": contents,
}
doc["schema"] = doc.get("schema", 1)
doc["updated_at"] = now
def version_key(text):
    # numeric components sort numerically, anything else after them
    parts = []
    for chunk in text.replace("-", ".").replace("_", ".").split("."):
        parts.append((0, int(chunk), "") if chunk.isdigit() else (1, 0, chunk))
    return parts


# newest version first keeps the file readable as it grows
doc["images"] = {v: doc["images"][v] for v in sorted(doc["images"], key=version_key, reverse=True)}
json.dump(doc, open(out_path, "w"), indent=2, sort_keys=False)
open(out_path, "a").write("\n")
PY

hash_before=$(sha256sum "$INDEX_FRESH" 2>/dev/null | cut -d' ' -f1 || echo none)
with_auth -X PUT --upload-file "$NEW_INDEX" -o /dev/null -w 'PUT %{http_code}\n' \
  "$REPO_URL/$INDEX_PATH"

log "reading $INDEX_PATH back and verifying it"
INDEX_BACK=$WORK_DIR/index-back.json
status=$(curl -sS -o "$INDEX_BACK" --max-time 60 -w '%{http_code}' "${CURL_AUTH[@]}" "$REPO_URL/$INDEX_PATH" || true)
[[ $status == 200 ]] || die "cannot read index.json back (HTTP $status): the index may be half written, check $REPO_URL/$INDEX_PATH"
python3 - "$NEW_INDEX" "$INDEX_BACK" "$VERSION" "$ARCH" "$SHA256" "$hash_before" "$INDEX_FRESH" <<'PY'
import json, sys

written_path, back_path, version, arch, sha256, hash_before, before_path = sys.argv[1:8]
written = json.load(open(written_path))
back = json.load(open(back_path))

if not isinstance(back, dict) or not isinstance(back.get("images"), dict):
    raise SystemExit("index.json read back is not an index document")

entry = back["images"].get(version, {}).get(arch)
if not isinstance(entry, dict):
    raise SystemExit(f"index.json read back does not list {version} {arch}")
if entry.get("sha256") != sha256:
    raise SystemExit(f"index.json lists sha256 {entry.get('sha256')} for {version} {arch}, wrote {sha256}")

# Every entry that was there before our merge must still be there, unchanged:
# a concurrent writer that overwrote the file must not drop a version.
import os
if os.path.exists(before_path):
    try:
        before = json.load(open(before_path))
    except Exception:
        before = {}
    for ver, arches in (before.get("images") or {}).items():
        for arc, old in arches.items():
            if ver == version and arc == arch:
                continue
            now = back["images"].get(ver, {}).get(arc)
            if not isinstance(now, dict) or now.get("sha256") != (old or {}).get("sha256"):
                raise SystemExit(f"index.json lost or changed the entry {ver} {arc} during this publish")
print(f"index.json lists {version} {arch}, sha256 {sha256[:16]}..., and every earlier entry is intact")
PY

lock_release

cat <<EOF

published $VERSION $ARCH
  image    $REPO_URL/$VERSION_PATH/$IMAGE_NAME
  sha256   $SHA256
  size     $(stat -c %s "$IMAGE") bytes
  index    $REPO_URL/$INDEX_PATH
EOF
