#!/usr/bin/env bash
# images/platform_test.sh -- test images/platform.sh without the real executors.
#
# A small stdlib HTTP server plays both upstreams -- a GitHub release tree and an
# npm registry -- with tiny fixtures of the shape the real artifacts have (a
# prime-agent tarball with its binary and package.json at the root, an npm tarball
# with package/bin/opencode). A generated pin file carries the digests of those
# fixtures, which is the one place images/platform.sh reads a pin from.
#
# What is checked: the archive's tree, links and manifest; owner, modes and mtime
# of every entry; the JSON line on stdout; one build giving byte-identical
# archives twice and a different built_at giving a different one; add-ons packed
# by images/addons-pack.sh; an arm64 bundle built on an amd64 host; and the
# refusals (a boxd of the wrong architecture, a pin the bytes do not match, a
# missing boxd).
#
# Nothing is built, mounted or installed: no Lima, no root, no network beyond
# 127.0.0.1, and everything it writes is under one mktemp -d it removes.
#
# Usage: images/platform_test.sh
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
PLATFORM=$ROOT/images/platform.sh

CHECKS=0
FAILED=0
pass() { CHECKS=$((CHECKS + 1)); printf 'PASS  %s\n' "$*"; }
fail() { CHECKS=$((CHECKS + 1)); FAILED=$((FAILED + 1)); printf 'FAIL  %s\n' "$*"; }
check() {
  if [[ $2 == "$3" ]]; then pass "$1"; else fail "$1 (expected '$2', actual '$3')"; fi
}
check_absent() {
  if [[ -z $2 ]]; then pass "$1"; else fail "$1 (expected nothing, actual '$2')"; fi
}

WORK=$(mktemp -d "${TMPDIR:-/tmp}/platform-test-XXXXXX")
SERVER_PID=
cleanup() {
  if [[ -n $SERVER_PID ]]; then kill "$SERVER_PID" 2>/dev/null || true; fi
  rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

# --- fixtures -----------------------------------------------------------------
# The upstream trees, under the paths the script asks for: the GitHub release
# asset by its repository path, the npm metadata and tarball below the registry
# root.
PRIME_VERSION=0.0.1
OPENCODE_VERSION=0.0.2
GH=$WORK/upstream/gh
NPM=$WORK/upstream/npm
ASSET_DIR=$GH/PrimeIntellect-ai/prime-agent/releases/download/v$PRIME_VERSION
mkdir -p "$ASSET_DIR" "$NPM/@opencode/cli-linux-x64" "$NPM/@opencode/cli-linux-arm64" "$WORK/fixtures"

make_prime_tarball() { # make_prime_tarball ARCH
  local arch=$1 dir
  dir=$(mktemp -d "$WORK/fixtures/prime-$arch-XXXXXX")
  printf '#!/bin/sh\necho a fixture of the prime-agent release\n' > "$dir/prime-agent"
  chmod 0755 "$dir/prime-agent"
  printf '{"name":"prime-agent","version":"%s"}\n' "$PRIME_VERSION" > "$dir/package.json"
  mkdir -p "$dir/runtime"
  printf 'fixture runtime\n' > "$dir/runtime/README"
  tar -czf "$ASSET_DIR/prime-agent-$PRIME_VERSION-linux-$arch.tar.gz" -C "$dir" .
  rm -rf "$dir"
}

make_opencode_tarball() { # make_opencode_tarball ARCH NAME
  local arch=$1 pkg=@opencode/cli-linux-$1 dir
  dir=$(mktemp -d "$WORK/fixtures/opencode-$arch-XXXXXX")
  mkdir -p "$dir/package/bin"
  printf '#!/bin/sh\necho a fixture of the opencode package\n' > "$dir/package/bin/opencode"
  chmod 0755 "$dir/package/bin/opencode"
  printf '{"name":"%s","version":"%s"}\n' "$pkg" "$OPENCODE_VERSION" > "$dir/package/package.json"
  tar -czf "$NPM/cli-linux-$arch-$OPENCODE_VERSION.tgz" -C "$dir" package
  rm -rf "$dir"
  local digest
  digest=$(python3 - "$NPM/cli-linux-$arch-$OPENCODE_VERSION.tgz" <<'PY'
import base64, hashlib, sys
print("sha512-" + base64.b64encode(hashlib.sha512(open(sys.argv[1], "rb").read()).digest()).decode())
PY
)
  python3 - "$NPM/@opencode/cli-linux-$arch/$OPENCODE_VERSION" "$pkg" "http://127.0.0.1:PORT/npm/cli-linux-$arch-$OPENCODE_VERSION.tgz" "$digest" <<'PY'
import json, sys
path, name, tarball, integrity = sys.argv[1:5]
json.dump({"name": name, "version": "0.0.2", "dist": {"tarball": tarball, "integrity": integrity}},
          open(path, "w"), indent=2)
PY
}

make_elf() { # make_elf PATH MACHINE  -- a 64-byte ELF header of the given machine
  python3 - "$1" "$2" <<'PY'
import struct, sys

path, want = sys.argv[1], sys.argv[2]
machine = {"amd64": 0x3E, "arm64": 0xB7}[want]
head = b"\x7fELF" + bytes([2, 1, 1]) + b"\0" * 9
head += struct.pack("<H", 2) + struct.pack("<H", machine) + struct.pack("<I", 1) + b"\0" * 40
open(path, "wb").write(head)
PY
  chmod 0755 "$1"
}

make_prime_tarball x64
make_prime_tarball arm64
make_opencode_tarball x64
make_opencode_tarball arm64
make_elf "$WORK/fixtures/boxd-amd64" amd64
make_elf "$WORK/fixtures/boxd-arm64" arm64

# The pins images/platform.sh reads, for the fixtures: the same three variables
# per architecture the repository's images/pins.sh sets.
sha256_of() { sha256sum "$1" | cut -d' ' -f1; }
prime_x64=$(sha256_of "$ASSET_DIR/prime-agent-$PRIME_VERSION-linux-x64.tar.gz")
prime_arm64=$(sha256_of "$ASSET_DIR/prime-agent-$PRIME_VERSION-linux-arm64.tar.gz")
oc_x64=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["dist"]["integrity"])' "$NPM/@opencode/cli-linux-x64/$OPENCODE_VERSION")
oc_arm64=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["dist"]["integrity"])' "$NPM/@opencode/cli-linux-arm64/$OPENCODE_VERSION")
cat > "$WORK/pins.sh" <<EOF
PRIME_AGENT_VERSION=$PRIME_VERSION
OPENCODE_VERSION=$OPENCODE_VERSION
case \${BOX_ARCH:-} in
  amd64)
    PRIME_AGENT_ARCH=x64
    PRIME_AGENT_SHA256=$prime_x64
    OPENCODE_NPM_PKG=@opencode/cli-linux-x64
    OPENCODE_INTEGRITY=$oc_x64
    ;;
  arm64)
    PRIME_AGENT_ARCH=arm64
    PRIME_AGENT_SHA256=$prime_arm64
    OPENCODE_NPM_PKG=@opencode/cli-linux-arm64
    OPENCODE_INTEGRITY=$oc_arm64
    ;;
  *) printf 'pins.sh: BOX_ARCH must be amd64 or arm64\n' >&2; exit 1 ;;
esac
EOF

# --- the two upstreams on 127.0.0.1 -------------------------------------------
PORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')
# The npm metadata carries the tarball URL, which needs the port: it is written
# with a placeholder above and fixed here.
python3 - "$NPM" "$PORT" <<'PY'
import glob, json, sys

npm, port = sys.argv[1], sys.argv[2]
for path in glob.glob(f"{npm}/@opencode/*/*"):
    doc = json.load(open(path))
    doc["dist"]["tarball"] = doc["dist"]["tarball"].replace("PORT", port)
    # A proxy registry answers a version's URL with the whole package document.
    if "arm64" in path:
        doc = {"name": doc["name"], "versions": {doc["version"]: doc}}
    json.dump(doc, open(path, "w"), indent=2)
PY
python3 -m http.server "$PORT" --bind 127.0.0.1 --directory "$WORK/upstream" >"$WORK/server.log" 2>&1 &
SERVER_PID=$!
for _ in $(seq 1 50); do
  if curl -fsS -o /dev/null "http://127.0.0.1:$PORT/"; then break; fi
  sleep 0.1
done
if [[ ! -d /proc/$SERVER_PID ]]; then
  echo "the fixture server did not start:" >&2
  cat "$WORK/server.log" >&2
  exit 1
fi

# --- one build ----------------------------------------------------------------
printf '== a bundle for amd64\n'
OUT=$WORK/out
BUILD_AT=2026-11-04T00:00:00Z
run_platform() {
  set +e
  PLATFORM_OUT=$(BOX_PINS_FILE=$WORK/pins.sh \
    BOX_MIRROR_GITHUB=http://127.0.0.1:$PORT/gh \
    BOX_MIRROR_NPM=http://127.0.0.1:$PORT/npm \
    "$PLATFORM" "$@" 2>"$WORK/platform.err")
  PLATFORM_STATUS=$?
  set -e
  PLATFORM_ERR=$(cat "$WORK/platform.err")
}
run_platform --version 9.9.9 --arch amd64 --boxd "$WORK/fixtures/boxd-amd64" \
  --boxd-version 1.0.0-test --out "$OUT" --built-at "$BUILD_AT"
check "images/platform.sh exits 0" "0" "$PLATFORM_STATUS"
ARCHIVE=$OUT/box-platform-9.9.9-linux-amd64.tar.gz
[[ -s $ARCHIVE ]] && pass "the archive is written" || fail "the archive is written ($ARCHIVE is empty or missing)"
[[ -s $ARCHIVE.sha256 ]] && pass "the .sha256 is written" || fail "the .sha256 is written"
check "the .sha256 is the archive's digest" "$(sha256_of "$ARCHIVE")" "$(cut -d' ' -f1 "$ARCHIVE.sha256")"
check "the .sha256 names the archive" "$(basename "$ARCHIVE")" "$(sed 's/.*  //' "$ARCHIVE.sha256")"

# stdout is one JSON line with the fields the index entry needs.
JSON_LINE=$(printf '%s\n' "$PLATFORM_OUT")
check "stdout is one line" "1" "$(printf '%s\n' "$JSON_LINE" | wc -l | tr -d ' ')"
check "stdout is the index entry's JSON" \
  "$(python3 - "$ARCHIVE" "$BUILD_AT" <<'PY'
import hashlib, json, os, sys

path, built_at = sys.argv[1], sys.argv[2]
print(json.dumps({
    "version": "9.9.9",
    "arch": "amd64",
    "file": os.path.basename(path),
    "sha256": hashlib.sha256(open(path, "rb").read()).hexdigest(),
    "size": os.path.getsize(path),
    "built_at": built_at,
    "boxd": "1.0.0-test",
    "tools": {"prime-agent": "0.0.1", "opencode": "0.0.2"},
}))
PY
)" "$JSON_LINE"

# --- what is inside -----------------------------------------------------------
printf '\n== the archive tree, links and manifest\n'
UNPACKED=$WORK/unpacked
mkdir -p "$UNPACKED"
tar -xzf "$ARCHIVE" -C "$UNPACKED"
TREE=$(cd "$UNPACKED" && find . ! -type d | LC_ALL=C sort | tr '\n' ' ' | sed 's/ $//')
check "the archive's root is what the guest sees at /opt/box" "./bin/boxctl ./bin/boxd ./bin/opencode ./bin/prime-agent ./lib/no-secrets.sh ./manifest.json ./tools/opencode/bin/opencode ./tools/opencode/package.json ./tools/prime-agent/package.json ./tools/prime-agent/prime-agent ./tools/prime-agent/runtime/README" "$TREE"
check "bin/boxctl is a link to boxd" "boxd" "$(readlink "$UNPACKED/bin/boxctl")"
# The seal runs the bundle's copy of guest/no-secrets.sh: a bundle without it is a
# bundle whose boxes cannot be baked.
check "the bundle carries the guest's no-secrets check" "$(sha256_of "$ROOT/guest/no-secrets.sh")" \
  "$(sha256_of "$UNPACKED/lib/no-secrets.sh")"
check "the check is executable" "-rwxr-xr-x" "$(stat -c '%A' "$UNPACKED/lib/no-secrets.sh")"
check "bin/prime-agent links into the tool's directory" "../tools/prime-agent/prime-agent" "$(readlink "$UNPACKED/bin/prime-agent")"
check "bin/opencode links into the tool's directory" "../tools/opencode/bin/opencode" "$(readlink "$UNPACKED/bin/opencode")"
check "boxd is the binary that was given" "$(sha256_of "$WORK/fixtures/boxd-amd64")" "$(sha256_of "$UNPACKED/bin/boxd")"
check "the prime-agent package.json travels beside its binary" "present" "$([[ -f $UNPACKED/tools/prime-agent/package.json ]] && echo present || echo missing)"

MANIFEST=$UNPACKED/manifest.json
check "the manifest is the one the contract describes" \
  "$(python3 - "$BUILD_AT" <<'PY'
import json
print(json.dumps({
    "schema": 1,
    "version": "9.9.9",
    "arch": "amd64",
    "boxd": "1.0.0-test",
    "tools": {"prime-agent": "0.0.1", "opencode": "0.0.2"},
    "addons": None,
    "built_at": "2026-11-04T00:00:00Z",
}, sort_keys=True))
PY
)" "$(python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); print(json.dumps(d, sort_keys=True))' "$MANIFEST")"

# Owner, mode and mtime of every entry: what makes the archive reproducible and
# what a read-only mount relies on.
LISTING=$(tar --utc --numeric-owner -tvzf "$ARCHIVE")
ODD=$(printf '%s\n' "$LISTING" | awk '$2 != "0/0" || $4 != "1970-01-01" || $5 != "00:00" {print}')
check_absent "every entry is owned by 0/0 and carries mtime 0" "$ODD"
WRITABLE=$(printf '%s\n' "$LISTING" | awk '$1 ~ /^[d-]/ && $1 ~ /w.$/ {print $NF}')
check_absent "no file or directory is world-writable" "$WRITABLE"
LINKS=$(cd "$UNPACKED" && find . -type l -printf '%p -> %l\n' | sort)
check_absent "every link in the archive is relative and inside it" "$(printf '%s\n' "$LINKS" | awk '{ if ($3 ~ /^\//) print; else { n = split($3, parts, "/"); depth = split($1, own, "/"); if (parts[1] == ".." && parts[2] == "..") print } }')"

# --- the same inputs, twice ---------------------------------------------------
printf '\n== determinism\n'
run_platform --version 9.9.9 --arch amd64 --boxd "$WORK/fixtures/boxd-amd64" \
  --boxd-version 1.0.0-test --out "$OUT" --built-at "$BUILD_AT"
check "the second build exits 0" "0" "$PLATFORM_STATUS"
check "the same inputs give the same sha256" "$(cut -d' ' -f1 "$ARCHIVE.sha256")" "$(sha256_of "$ARCHIVE")"
check "the same inputs give the same JSON" "$JSON_LINE" "$PLATFORM_OUT"
FIRST_SHA=$(cut -d' ' -f1 "$ARCHIVE.sha256")
run_platform --version 9.9.9 --arch amd64 --boxd "$WORK/fixtures/boxd-amd64" \
  --boxd-version 1.0.0-test --out "$WORK/out-other" --built-at 2026-11-05T00:00:00Z
check "a different built_at exits 0" "0" "$PLATFORM_STATUS"
if [[ $(sha256_of "$WORK/out-other/box-platform-9.9.9-linux-amd64.tar.gz") == "$FIRST_SHA" ]]; then
  fail "a different built_at gives different bytes"
else
  pass "a different built_at gives different bytes"
fi

# --- add-ons ------------------------------------------------------------------
printf '\n== the add-ons\n'
ADDONS_SRC=$WORK/addons
mkdir -p "$ADDONS_SRC/plugin"
printf 'module.exports = {}\n' > "$ADDONS_SRC/plugin/index.js"
printf 'settings\n' > "$ADDONS_SRC/settings.ini"
chmod 0600 "$ADDONS_SRC/settings.ini"
ADDONS_JSON=$("$ROOT/images/addons-pack.sh" "$ADDONS_SRC" "$WORK/addons.tar")
run_platform --version 9.9.10 --arch amd64 --boxd "$WORK/fixtures/boxd-amd64" \
  --boxd-version 1.0.0-test --addons "$ADDONS_SRC" --out "$OUT" --built-at "$BUILD_AT"
check "a build with add-ons exits 0" "0" "$PLATFORM_STATUS"
WITH_ADDONS=$OUT/box-platform-9.9.10-linux-amd64.tar.gz
if tar -tzf "$WITH_ADDONS" | grep -qx './addons/plugin/index.js'; then
  pass "the add-ons are in the bundle at addons/"
else
  fail "the add-ons are in the bundle at addons/ ($(tar -tzf "$WITH_ADDONS" | tr '\n' ' '))"
fi
check "the manifest records what was packed" \
  "$(python3 -c 'import json,sys; print(json.dumps(json.loads(sys.argv[1]), sort_keys=True))' "$ADDONS_JSON")" \
  "$(python3 - "$WITH_ADDONS" <<'PY'
import json, subprocess, sys
raw = subprocess.run(["tar", "-xzOf", sys.argv[1], "./manifest.json"], capture_output=True, check=True).stdout
print(json.dumps(json.loads(raw)["addons"], sort_keys=True))
PY
)"
# The packed tar's own modes and owner survive the second extraction unchanged.
check "an add-on file keeps the packed mode" "-rw-r--r--" \
  "$(tar --utc --numeric-owner -tvzf "$WITH_ADDONS" ./addons/settings.ini | awk '{print $1}')"

# --- arm64 on an amd64 host ---------------------------------------------------
printf '\n== an arm64 bundle on this host\n'
if [[ $(uname -m) == aarch64 || $(uname -m) == arm64 ]]; then
  pass "this host is arm64; the amd64 case above already proves the cross build"
else
  run_platform --version 9.9.9 --arch arm64 --boxd "$WORK/fixtures/boxd-arm64" \
    --boxd-version 1.0.0-test --out "$WORK/out-arm64" --built-at "$BUILD_AT"
  check "an arm64 bundle builds on this host" "0" "$PLATFORM_STATUS"
  ARM_ARCHIVE=$WORK/out-arm64/box-platform-9.9.9-linux-arm64.tar.gz
  check "the arm64 archive names its architecture" "9.9.9 arm64" \
    "$(python3 - "$ARM_ARCHIVE" <<'PY'
import json, subprocess, sys
raw = subprocess.run(["tar", "-xzOf", sys.argv[1], "./manifest.json"], capture_output=True, check=True).stdout
doc = json.loads(raw)
print(doc["version"], doc["arch"])
PY
)"
  check "the arm64 bundle carries the arm64 executors" "present" \
    "$(tar -tzf "$ARM_ARCHIVE" | grep -qx './tools/prime-agent/prime-agent' && echo present || echo missing)"
fi

# --- what is refused ----------------------------------------------------------
printf '\n== what is refused\n'
run_platform --version 9.9.9 --arch amd64 --boxd "$WORK/fixtures/boxd-arm64" \
  --boxd-version 1.0.0-test --out "$WORK/out-bad" --built-at "$BUILD_AT"
if [[ $PLATFORM_STATUS -ne 0 ]]; then pass "a boxd of the wrong architecture is refused"; else fail "a boxd of the wrong architecture is refused"; fi
case $PLATFORM_ERR in
  *arm64*) pass "the refusal names the architecture it found" ;;
  *) fail "the refusal names the architecture it found (it said '$PLATFORM_ERR')" ;;
esac

sed -e 's/^    PRIME_AGENT_SHA256=.*/    PRIME_AGENT_SHA256=0000000000000000000000000000000000000000000000000000000000000000/' \
  "$WORK/pins.sh" > "$WORK/pins-bad.sh"
set +e
PLATFORM_OUT=$(BOX_PINS_FILE=$WORK/pins-bad.sh \
  BOX_MIRROR_GITHUB=http://127.0.0.1:$PORT/gh \
  BOX_MIRROR_NPM=http://127.0.0.1:$PORT/npm \
  "$PLATFORM" --version 9.9.9 --arch amd64 --boxd "$WORK/fixtures/boxd-amd64" \
  --boxd-version 1.0.0-test --out "$WORK/out-bad" --built-at "$BUILD_AT" 2>"$WORK/platform.err")
PLATFORM_STATUS=$?
set -e
PLATFORM_ERR=$(cat "$WORK/platform.err")
if [[ $PLATFORM_STATUS -ne 0 ]]; then pass "bytes that do not match the pin are refused"; else fail "bytes that do not match the pin are refused"; fi
case $PLATFORM_ERR in
  *sha256*) pass "the refusal names the digest" ;;
  *) fail "the refusal names the digest (it said '$PLATFORM_ERR')" ;;
esac
if [[ -e $WORK/out-bad/box-platform-9.9.9-linux-amd64.tar.gz ]]; then
  fail "no archive is written for a refused build"
else
  pass "no archive is written for a refused build"
fi

run_platform --version 9.9.9 --arch amd64 --out "$WORK/out-bad" --built-at "$BUILD_AT"
if [[ $PLATFORM_STATUS -ne 0 ]]; then pass "--boxd is required"; else fail "--boxd is required"; fi

printf '\n'
if [[ $FAILED -ne 0 ]]; then
  printf 'RESULT: %d check(s), %d failed\n' "$CHECKS" "$FAILED"
  exit 1
fi
printf 'RESULT: %d checks, all passed\n' "$CHECKS"
