#!/usr/bin/env bash
# images/addons_pack_test.sh -- test images/addons-pack.sh.
#
# Runs the pack script against small trees in a temporary directory and checks
# what it promises: the archive's paths, modes, owner and mtime; the JSON object
# on stdout against the listing it documents; one tree giving byte-identical
# archives twice; and the two refusals (a symlink, an empty directory).
#
# Nothing is built and nothing is installed: no Lima, no root, no image, and
# everything it writes is under one mktemp -d that it removes. CI runs it in the
# image-tools job next to publish_test.py.
#
# Usage: images/addons_pack_test.sh
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
PACK=$ROOT/images/addons-pack.sh

CHECKS=0
FAILED=0
pass() { CHECKS=$((CHECKS + 1)); printf 'PASS  %s\n' "$*"; }
fail() { CHECKS=$((CHECKS + 1)); FAILED=$((FAILED + 1)); printf 'FAIL  %s\n' "$*"; }
# check NAME WANT GOT -- one comparison, PASS when the two are the same string.
check() {
  if [[ $2 == "$3" ]]; then pass "$1"; else fail "$1 (expected '$2', actual '$3')"; fi
}
# check_absent NAME VALUE -- PASS when VALUE is empty.
check_absent() {
  if [[ -z $2 ]]; then pass "$1"; else fail "$1 (expected nothing, actual '$2')"; fi
}

WORK=$(mktemp -d "${TMPDIR:-/tmp}/addons-pack-test-XXXXXX")
trap 'rm -rf "$WORK"' EXIT

# run_pack DIR TAR -- run the script and keep everything it said, because the
# refusals are tested as much as the successes.
run_pack() {
  set +e
  PACK_OUT=$("$PACK" "$1" "$2" 2>"$WORK/pack.err")
  PACK_STATUS=$?
  set -e
  PACK_ERR=$(cat "$WORK/pack.err")
}

# make_tree DIR -- a tree of the shape an add-on package has: nested
# directories, an executable file, files with modes that must be normalised.
make_tree() {
  local dir=$1
  mkdir -p "$dir/divider/plugins" "$dir/bin" "$dir/data"
  printf 'module.exports = {}\n' >"$dir/divider/index.js"
  printf 'plugin\n' >"$dir/divider/plugins/one.js"
  printf '#!/bin/sh\necho run\n' >"$dir/bin/run.sh"
  chmod 0755 "$dir/bin/run.sh"
  printf 'a=1\n' >"$dir/data/settings.ini"
  chmod 0600 "$dir/data/settings.ini"
  chmod 0700 "$dir/data"
}

# The fields of one listing line, by its path: the owner, the mtime and the mode
# are three separate promises, so each has its own answer here.
LISTING=
listing_field() { printf '%s\n' "$LISTING" | awk -v want="$1" -v field="$2" '$NF == want {print $field; exit}'; }
listing_mtime() { printf '%s\n' "$LISTING" | awk -v want="$1" '$NF == want {print $4, $5; exit}'; }

printf '== a tree of add-ons\n'
SRC=$WORK/addons
make_tree "$SRC"
TAR1=$WORK/first.tar
run_pack "$SRC" "$TAR1"
check "images/addons-pack.sh exits 0 on a tree" "0" "$PACK_STATUS"
JSON1=$PACK_OUT
if [[ -s $TAR1 ]]; then
  pass "the archive is written"
else
  fail "the archive is written ($TAR1 is empty or missing)"
fi

# The tar is GNU tar's: --utc makes the mtime a value and not a time zone, and
# --numeric-owner keeps the owner out of the host's /etc/passwd.
LISTING=$(tar --utc --numeric-owner -tvf "$TAR1")

check "the archive holds the tree's contents, not the directory itself" \
  "./ ./bin/ ./bin/run.sh ./data/ ./data/settings.ini ./divider/ ./divider/index.js ./divider/plugins/ ./divider/plugins/one.js" \
  "$(printf '%s\n' "$LISTING" | awk '{print $NF}' | tr '\n' ' ' | sed 's/ $//')"

for path in ./ ./bin/ ./data/ ./divider/; do
  check "the directory $path is normalised to 0755" "drwxr-xr-x" "$(listing_field "$path" 1)"
done
check "an executable file keeps its execute bit, as 0755" "-rwxr-xr-x" "$(listing_field ./bin/run.sh 1)"
check "a file without one is normalised to 0644" "-rw-r--r--" "$(listing_field ./data/settings.ini 1)"
check "every file is normalised, not only the executable" "-rw-r--r--" "$(listing_field ./divider/plugins/one.js 1)"
for path in ./ ./bin/run.sh ./data/settings.ini ./divider/plugins/one.js; do
  check "$path is owned by 0/0" "0/0" "$(listing_field "$path" 2)"
  check "$path carries mtime 0" "1970-01-01 00:00" "$(listing_mtime "$path")"
done

# The JSON is what images/build.sh records, so its numbers are worked out again
# here from the tree itself, not taken from the script's own answer.
WANT_JSON=$(python3 - "$SRC" <<'PY'
import hashlib, json, os, sys
src = sys.argv[1]
lines = []
total = 0
for root, _, names in os.walk(src):
    for name in names:
        path = os.path.join(root, name)
        rel = os.path.relpath(path, src)
        lines.append(hashlib.sha256(open(path, "rb").read()).hexdigest() + "  " + rel)
        total += os.path.getsize(path)
listing = "".join(line + "\n" for line in sorted(lines))
print(json.dumps({"files": len(lines), "bytes": total,
                  "sha256": hashlib.sha256(listing.encode()).hexdigest()}))
PY
)
check "stdout is the documented JSON line for the tree" "$WANT_JSON" "$JSON1"

printf '\n== the same tree twice\n'
TAR2=$WORK/second.tar
run_pack "$SRC" "$TAR2"
check "the second run exits 0" "0" "$PACK_STATUS"
if cmp -s "$TAR1" "$TAR2"; then
  pass "the same tree gives the same archive bytes"
else
  fail "the same tree gives the same archive bytes (the two differ)"
fi
check "the same tree gives the same JSON" "$JSON1" "$PACK_OUT"

printf '\n== what is refused\n'
# A symlink: inside the image it would point at a path this host chose, or at
# nothing, and the operator never sees that.
LINKED=$WORK/with-a-link
mkdir -p "$LINKED"
printf 'x\n' >"$LINKED/a.txt"
ln -s a.txt "$LINKED/link"
run_pack "$LINKED" "$WORK/linked.tar"
if [[ $PACK_STATUS -ne 0 ]]; then pass "a symlink is refused (exit $PACK_STATUS)"; else fail "a symlink is refused (it was packed)"; fi
case $PACK_ERR in
  *"$LINKED/link"*) pass "the refusal names the symlink" ;;
  *) fail "the refusal names the symlink (it said '$PACK_ERR')" ;;
esac
if [[ -e $WORK/linked.tar ]]; then
  fail "no archive is written for a symlink"
else
  pass "no archive is written for a symlink"
fi

EMPTY=$WORK/empty
mkdir -p "$EMPTY"
run_pack "$EMPTY" "$WORK/empty.tar"
if [[ $PACK_STATUS -ne 0 ]]; then pass "an empty directory is refused (exit $PACK_STATUS)"; else fail "an empty directory is refused (it was packed)"; fi
case $PACK_ERR in
  *"$EMPTY"*) pass "the refusal names the empty directory" ;;
  *) fail "the refusal names the empty directory (it said '$PACK_ERR')" ;;
esac
if [[ -e $WORK/empty.tar ]]; then
  fail "no archive is written for an empty directory"
else
  pass "no archive is written for an empty directory"
fi

printf '\n'
if [[ $FAILED -ne 0 ]]; then
  printf 'RESULT: %d check(s), %d failed\n' "$CHECKS" "$FAILED"
  exit 1
fi
printf 'RESULT: %d checks, all passed\n' "$CHECKS"
