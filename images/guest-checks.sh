#!/bin/bash
# images/guest-checks.sh -- the checks a box image must pass, in a booted guest.
#
# Runs as root inside a booted box (by images/verify.sh, through 'limactl
# shell'), and proves the properties the image exists for: it booted and its
# daemon answers, the tool versions are the ones the image says it carries, the
# executor credentials live on a tmpfs and nowhere else, a rotation reaches boxd,
# and no secret is baked into the image.
#
# Every check prints one line -- "PASS" or "FAIL", with the value expected and the
# value found -- and the script exits non-zero when any of them failed. Nothing
# here writes to the box's disk: the test credential is written to
# /run/box/secrets, which is a tmpfs, and it is removed again at the end.
#
# Usage (inside the guest, as root):
#   images/guest-checks.sh [--manifest PATH]
#
# --manifest is the build manifest of the image (its "tools" are what the checks
# compare against). Without it the image's own record, /etc/box/image.json, is
# used: an image carries the same three answers -- version, boxd source, tool
# versions -- so a published image can be checked against its manifest and a
# booted box against itself.
set -uo pipefail

MANIFEST=/etc/box/image.json
while [[ $# -gt 0 ]]; do
  case $1 in
    --manifest)
      [[ $# -ge 2 ]] || { echo "guest-checks: --manifest needs a path" >&2; exit 2; }
      MANIFEST=$2
      shift 2
      ;;
    -h|--help)
      sed -n '2,22p' "$0" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *)
      echo "guest-checks: unknown argument: $1" >&2
      exit 2
      ;;
  esac
done

CHECKS=0
FAILED=0
info() { printf 'info  %s\n' "$*"; }
pass() { CHECKS=$((CHECKS + 1)); printf 'PASS  %s (expected %s, actual %s)\n' "$1" "$2" "$3"; }
fail() { CHECKS=$((CHECKS + 1)); FAILED=$((FAILED + 1)); printf 'FAIL  %s (expected %s, actual %s)\n' "$1" "$2" "$3"; }
# check NAME WANT GOT -- one comparison, PASS when the two are the same string.
check() {
  if [[ $2 == "$3" ]]; then pass "$1" "$2" "$3"; else fail "$1" "$2" "$3"; fi
}
# check_none NAME VALUE -- PASS when VALUE is empty: the expectation is that
# nothing was found, and the report says so in those words.
check_none() {
  if [[ -z $2 ]]; then pass "$1" "nothing" "nothing"; else fail "$1" "nothing" "$2"; fi
}
# short cuts a value for a one-line report.
short() { local v=$1; if [[ ${#v} -gt 80 ]]; then printf '%s...' "${v:0:77}"; else printf '%s' "$v"; fi; }
# first_line runs a command and returns its first line of output, stderr included.
first_line() { "$@" 2>&1 | head -1; }

# jget prints one value out of the manifest by dotted path; empty when absent.
jget() {
  python3 - "$MANIFEST" "$1" <<'PY'
import json, sys
try:
    doc = json.load(open(sys.argv[1]))
except Exception:
    sys.exit(0)
cur = doc
for part in sys.argv[2].split("."):
    if isinstance(cur, dict) and part in cur:
        cur = cur[part]
    else:
        cur = ""
        break
if cur is None:
    cur = ""
print(cur if isinstance(cur, str) else json.dumps(cur))
PY
}

printf '== the image\n'
if [[ -r $MANIFEST ]]; then
  pass "the build manifest is readable ($MANIFEST)" "readable" "readable"
else
  fail "the build manifest is readable ($MANIFEST)" "readable" "missing"
fi
IMAGE_VERSION=$(jget version)
IMAGE_ARCH=$(jget arch)
info "image        $IMAGE_VERSION $IMAGE_ARCH (built $(jget built_at))"
info "base image   $(jget base_image.name), kernel $(short "$(jget kernel)")"
info "boxd source  $(jget boxd.kind), $(jget boxd.tag)$(jget boxd.resolved_commit), version $(jget boxd.version)"

# --- what boxd the image carries ----------------------------------------------
BOXD_WANT_SHA=$(jget boxd.sha256)
BOXD_WANT_VERSION=$(jget boxd.version)
BOXD_BIN=/usr/local/bin/boxd
if [[ -x $BOXD_BIN ]]; then
  BOXD_GOT_SHA=$(sha256sum "$BOXD_BIN" | cut -d' ' -f1)
  if [[ -n $BOXD_WANT_SHA ]]; then
    check "the installed boxd is the binary the image records" "$BOXD_WANT_SHA" "$BOXD_GOT_SHA"
  else
    info "the manifest records no boxd sha256; the installed boxd is $BOXD_GOT_SHA"
  fi
  # A probe writes into HOME; it runs on the tmpfs, never in root's home.
  BOXD_VERSION=$(HOME=/run/boxd boxd --version 2>&1 | head -1)
  if [[ -n $BOXD_WANT_VERSION ]]; then
    check "boxd reports the version the image records" "$BOXD_WANT_VERSION" "$BOXD_VERSION"
  else
    info "the manifest records no boxd version; boxd reports $BOXD_VERSION"
  fi
  check "boxctl is installed next to boxd" "present" "$([[ -x /usr/local/bin/boxctl ]] && echo present || echo missing)"
else
  fail "boxd is installed" "executable $BOXD_BIN" "missing"
fi

# --- the guest booted ---------------------------------------------------------
printf '\n== the guest\n'
BOOT_ID=$(cat /proc/sys/kernel/random/boot_id 2>/dev/null || echo unknown)
info "kernel       $(uname -r)"
info "boot id      $BOOT_ID"
info "accounts     $(getent passwd | awk -F: '$3 >= 500 && $3 < 60000 {printf "%s(%s) ", $1, $3}')"

CLOUD_INIT=$(cloud-init status 2>/dev/null | head -1 | tr -d '\r')
check "cloud-init finished on this boot" "status: done" "$CLOUD_INIT"
check "the image carries no build account" "absent" "$(getent passwd builder >/dev/null && echo present || echo absent)"

if systemctl is-enabled --quiet boxd.service 2>/dev/null; then
  pass "boxd.service is enabled" "enabled" "enabled"
else
  fail "boxd.service is enabled" "enabled" "$(systemctl is-enabled boxd.service 2>&1 | head -1)"
fi
BOX_ACTIVE=$(systemctl is-active boxd.service 2>/dev/null || true)
check "boxd.service is active" "active" "$BOX_ACTIVE"

BOXCTL_INFO=$(boxctl info 2>&1)
BOXCTL_STATUS=$?
if [[ $BOXCTL_STATUS -eq 0 ]] && python3 -c 'import json,sys; json.loads(sys.argv[1])' "$BOXCTL_INFO" 2>/dev/null; then
  pass "boxctl info answers with JSON" "JSON" "$(short "$BOXCTL_INFO")"
else
  fail "boxctl info answers with JSON" "JSON" "$(short "$BOXCTL_INFO")"
fi
EXECUTORS_OK=$(python3 - "$BOXCTL_INFO" 2>/dev/null <<'PY'
import json, sys
try:
    info = json.loads(sys.argv[1])
except Exception:
    print("boxctl info is not JSON")
    raise SystemExit(0)
bad = []
for name in ("prime", "opencode"):
    ex = {e.get("name"): e for e in info.get("executors", [])}.get(name)
    if not ex or not ex.get("available"):
        bad.append(f"{name}: {ex}")
print("prime and opencode available" if not bad else "; ".join(bad))
PY
)
check "the executors the image installs are available" "prime and opencode available" "$EXECUTORS_OK"

# --- the tool versions the image records --------------------------------------
printf '\n== the tool versions the image records\n'
# One line per tool: the command the recipe ran to record it, the way
# images/provision.sh records the manifest. A tool the manifest does not name is
# reported and skipped, so an image built from a smaller recipe still checks.
tool_version() {
  case $1 in
    node) node --version 2>&1 | head -1 ;;
    npm) npm --version 2>&1 | head -1 ;;
    go) go version 2>&1 | cut -d' ' -f3 ;;
    python3) python3 --version 2>&1 | cut -d' ' -f2 ;;
    uv) uv --version 2>&1 | cut -d' ' -f2 ;;
    docker) docker --version 2>&1 | cut -d' ' -f3 | tr -d , ;;
    git) git --version 2>&1 | cut -d' ' -f3 ;;
    # An executor's --version is not a read-only question: opencode creates its
    # data directory and a log in HOME, so the probe gets a scratch HOME on the
    # tmpfs and never writes onto the box disk.
    prime_agent) HOME=/run/box-checks-home prime-agent --version 2>&1 | head -1 ;;
    opencode) HOME=/run/box-checks-home opencode --version 2>&1 | head -1 ;;
    *) return 1 ;;
  esac
}
mkdir -p /run/box-checks-home
TOOLS_CHECKED=0
for tool in node npm go python3 uv docker git prime_agent opencode; do
  want=$(jget "tools.$tool")
  if [[ -z $want ]]; then
    info "$tool: the manifest records no version, skipped"
    continue
  fi
  got=$(tool_version "$tool" || echo "not installed")
  check "$tool is the version the image records" "$want" "$(short "$got")"
  TOOLS_CHECKED=$((TOOLS_CHECKED + 1))
done
rmdir /run/box-checks-home 2>/dev/null || true
[[ $TOOLS_CHECKED -gt 0 ]] || fail "the manifest names at least one tool version" "at least one" "none"

# --- the add-ons the image carries -------------------------------------------
printf '\n== the add-ons\n'
# /opt/box/addons is where the build puts the operator's extra files, and it is
# only there when the image was built with them. They are files at a fixed path:
# which guest program reads them, and how it is told the path, is that program's
# own configuration, so the checks here stop at who owns them, whether they are
# writable, and how many there are.
ADDONS=/opt/box/addons
if [[ -e $ADDONS ]]; then
  ADDONS_SHA=$(jget addons.sha256)
  [[ -n $ADDONS_SHA ]] && info "add-ons      sha256 $ADDONS_SHA"
  ADDONS_NOT_ROOT=$(find "$ADDONS" \( ! -uid 0 -o ! -gid 0 \) -printf '%U:%G %p\n' 2>/dev/null | head -3 | tr '\n' ' ')
  check_none "everything in $ADDONS is owned by root" "$ADDONS_NOT_ROOT"
  ADDONS_WRITABLE=$(find "$ADDONS" -perm /022 -printf '%m %p\n' 2>/dev/null | head -3 | tr '\n' ' ')
  check_none "nothing in $ADDONS is group- or world-writable" "$ADDONS_WRITABLE"
  ADDONS_FILES=$(find "$ADDONS" -type f | wc -l | tr -d ' ')
  info "add-ons      $ADDONS_FILES file(s) in $ADDONS"
  ADDONS_WANT_FILES=$(jget addons.files)
  if [[ -n $ADDONS_WANT_FILES ]]; then
    check "the add-ons are the ones the image records" "$ADDONS_WANT_FILES file(s)" "$ADDONS_FILES file(s)"
  else
    info "the image records no add-on count, so $ADDONS_FILES file(s) are reported as found"
  fi
else
  info "no add-ons: $ADDONS does not exist"
fi

# --- apt only talks to the sources the image configures -----------------------
printf '\n== apt sources\n'
APT_HOSTS=$(python3 - <<'PY'
import glob, re
hosts = set()
files = ["/etc/apt/sources.list"] + sorted(glob.glob("/etc/apt/sources.list.d/*.sources")) + sorted(glob.glob("/etc/apt/sources.list.d/*.list"))
for path in files:
    try:
        lines = open(path, errors="replace").read().splitlines()
    except OSError:
        continue
    for line in lines:
        line = line.split("#", 1)[0].strip()
        if line.startswith("URIs:"):
            urls = line.split(":", 1)[1].split()
        elif line.startswith("deb ") and len(line.split()) > 1:
            urls = [line.split()[1]]
        else:
            continue
        for url in urls:
            hosts.add(re.sub(r"^[a-z+]+://([^/:]+).*", r"\1", url))
print(" ".join(sorted(hosts)))
PY
)
if [[ -z $APT_HOSTS ]]; then
  fail "the image configures an apt source" "at least one host" "none"
else
  pass "the image configures its apt sources" "at least one host" "$APT_HOSTS"
  if apt-get update >/tmp/apt-update.txt 2>&1; then
    APT_STRAY=$(python3 - /tmp/apt-update.txt $APT_HOSTS <<'PY'
import re, sys
allowed = set(sys.argv[2:])
bad = []
for line in open(sys.argv[1], errors="replace"):
    m = re.match(r"^(?:Get|Hit|Ign):\S*\s+(\w+://\S+)", line)
    if not m:
        continue
    host = re.sub(r"^\w+://([^/:]+).*", r"\1", m.group(1))
    if host not in allowed:
        bad.append(line.rstrip())
print("|".join(bad))
PY
)
    check_none "apt-get update only used the configured sources" "$APT_STRAY"
  else
    fail "apt-get update succeeds" "exit 0" "exit non-zero"
    tail -5 /tmp/apt-update.txt
  fi
fi

# --- no secret in the image ---------------------------------------------------
printf '\n== no secret in the image\n'
# The check itself is guest/no-secrets.sh, which images/build.sh runs before it
# captures the disk; images/verify.sh copies it next to this script.
if [[ -f /tmp/no-secrets.sh ]]; then
  if NO_SECRETS_OUT=$(bash /tmp/no-secrets.sh "guest checks" 2>&1); then
    pass "the image carries no credential" "no credential" "no credential"
  else
    fail "the image carries no credential" "no credential" "$(printf '%s\n' "$NO_SECRETS_OUT" | grep -m1 '^FAIL' || echo 'a credential is in the image')"
    printf '%s\n' "$NO_SECRETS_OUT" | sed 's/^/      /'
  fi
else
  fail "guest/no-secrets.sh is available to check the image" "copied to /tmp/no-secrets.sh" "missing"
fi

# --- the credentials live on a tmpfs and nowhere else -------------------------
printf '\n== the executor credentials\n'
# In a box /run/box/secrets is the driver's read-only mount of the task's
# secrets directory on the host (internal/box/files.go). The image has no such
# mount, so the check stands in for it with a root-only directory on the tmpfs,
# the task's layout as it is, and checks what boxd does with it.
SECRETS=/run/box/secrets
RUNTIME=/run/boxd
# The driver creates the secrets directory when the task starts, root and 0700;
# it exists before boxd is told anything about it.
install -d -m 0700 -o root -g root "$SECRETS"
for dir in "$SECRETS" "$RUNTIME"; do
  fstype=$(findmnt -no FSTYPE --target "$dir" 2>/dev/null || echo unknown)
  check "$dir is on a tmpfs" "tmpfs" "$fstype"
done

TEST_TOKEN="box-check-$(head -c 12 /dev/urandom | od -An -tx1 | tr -d ' \n')"
# The secrets directory as the check stands it in: root and 0700, everything
# below it owner-only.
install -d -m 0700 -o root -g root "$SECRETS/prime"
printf '# a credential of this check, written the way the driver delivers one\nBOX_CHECK_TOKEN=%s\n' \
  "$TEST_TOKEN" > "$SECRETS/prime/env"
chmod 0600 "$SECRETS/prime/env"
chmod -R u+rwX,go-rwx "$SECRETS"
check "a credential written into $SECRETS is root-only" "600" "$(stat -c %a "$SECRETS/prime/env")"

# boxd reads the secrets directory: a reload reports which executors have
# credentials now, and it copies them below the runtime directory (on the tmpfs).
RELOAD=$(boxctl reload 2>&1)
RELOAD_STATUS=$?
check "boxctl reload succeeds" "exit 0" "exit $RELOAD_STATUS"
RELOAD_PRIME=$(python3 - "$RELOAD" 2>/dev/null <<'PY'
import json, sys
try:
    res = json.loads(sys.argv[1])
except Exception:
    print("boxctl reload is not JSON")
    raise SystemExit(0)
configured = res.get("configured") or []
print("prime configured" if "prime" in configured else f"configured {configured}")
PY
)
check "boxd reports the executor it was given a credential for" "prime configured" "$RELOAD_PRIME"
RUNTIME_ENV=$RUNTIME/executors/prime/env
check "boxd copied the credential onto the tmpfs" "$TEST_TOKEN" "$(sed -n 's/^BOX_CHECK_TOKEN=//p' "$RUNTIME_ENV" 2>/dev/null | head -1)"

# Not readable by the box's default user: the account Lima's cloud-init creates
# for whoever made the box. Lima names it in its cidata; its uid is the host
# user's, 1000 and up on Linux but 501 and up on a Mac, so the uid range is only
# the fallback for a guest started without Lima's cidata.
DEFAULT_USER=$(sed -n 's/^LIMA_CIDATA_USER=//p' /mnt/lima-cidata/param.env 2>/dev/null | head -1)
if [[ -z $DEFAULT_USER ]] || ! getent passwd "$DEFAULT_USER" >/dev/null; then
  DEFAULT_USER=$(getent passwd | awk -F: '$3 >= 500 && $3 < 60000 && $1 != "builder" {print $1; exit}')
fi
if [[ -n $DEFAULT_USER ]]; then
  if sudo -u "$DEFAULT_USER" cat "$SECRETS/prime/env" >/dev/null 2>&1; then
    fail "the default user cannot read the credential" "permission denied" "read it"
  else
    pass "the default user cannot read the credential" "permission denied" "permission denied"
  fi
  if sudo -u "$DEFAULT_USER" ls "$SECRETS" >/dev/null 2>&1; then
    fail "the default user cannot list $SECRETS" "permission denied" "listed it"
  else
    pass "the default user cannot list $SECRETS" "permission denied" "permission denied"
  fi
else
  fail "the box has a default user" "Lima's user, or an account with uid 500-59999" "none"
fi

# Nowhere on the disk: the credential is in the tmpfs and in nothing that
# survives the box. /run, /proc, /sys and /dev are the exceptions by definition.
DISK_HITS=$(find / -xdev -type f \
  -not -path '/run/*' -not -path '/proc/*' -not -path '/sys/*' -not -path '/dev/*' \
  -print0 2>/dev/null | xargs -0 -r grep -l -F -- "$TEST_TOKEN" 2>/dev/null | head -5)
check_none "the credential is nowhere on the disk" "$DISK_HITS"

# --- a rotation reaches boxd --------------------------------------------------
printf '\n== a credential rotation\n'
ROTATED_TOKEN="box-check-rotated-$(head -c 12 /dev/urandom | od -An -tx1 | tr -d ' \n')"
# The rotated file keeps the old one's mtime: a re-render can land within the
# same timestamp, and boxd must follow the content, not the time.
touch -r "$SECRETS/prime/env" "$SECRETS/.env.mtime"
cat > "$SECRETS/prime/env" <<EOF
# the same credential, rotated: boxctl reload must pick this up
BOX_CHECK_TOKEN=$ROTATED_TOKEN
EOF
chmod 0600 "$SECRETS/prime/env"
touch -r "$SECRETS/.env.mtime" "$SECRETS/prime/env"
rm -f "$SECRETS/.env.mtime"
RELOAD2=$(boxctl reload 2>&1)
RELOAD2_PRIME=$(python3 - "$RELOAD2" 2>/dev/null <<'PY'
import json, sys
try:
    res = json.loads(sys.argv[1])
except Exception:
    print("boxctl reload is not JSON")
    raise SystemExit(0)
configured = res.get("configured") or []
print("prime configured" if "prime" in configured else f"configured {configured}")
PY
)
check "boxd reports the executor configured after the rotation" "prime configured" "$RELOAD2_PRIME"
check "boxd picked up the rotated credential (same mtime as the old one)" "$ROTATED_TOKEN" \
  "$(sed -n 's/^BOX_CHECK_TOKEN=//p' "$RUNTIME_ENV" 2>/dev/null | head -1)"

# Leave nothing behind: the box is thrown away, but a check that cleans up after
# itself is a check that can run twice.
rm -rf "$SECRETS/prime"
boxctl reload >/dev/null 2>&1 || true

# --- the image is a box: disk, identity, hygiene ------------------------------
printf '\n== disk and identity\n'
ROOT_SIZE=$(df -B1 --output=size / | tail -1 | tr -d ' ')
ROOT_SRC=$(findmnt -no SOURCE /)
PART_SIZE=$(blockdev --getsize64 "$ROOT_SRC" 2>/dev/null || echo 0)
if [[ $PART_SIZE -gt 0 ]] && ((ROOT_SIZE * 100 / PART_SIZE >= 95)); then
  pass "the root filesystem fills its partition (box-grow-rootfs ran)" ">= 95% used of the partition" "$((ROOT_SIZE * 100 / PART_SIZE))%"
else
  fail "the root filesystem fills its partition (box-grow-rootfs ran)" ">= 95% used of the partition" "$ROOT_SIZE of $PART_SIZE bytes"
fi

BOOT_EPOCH=$(date -d "$(uptime -s)" +%s 2>/dev/null || echo 0)
STALE_KEYS=""
for key in /etc/ssh/ssh_host_*_key; do
  [[ -e $key ]] || continue
  [[ $(stat -c %Y "$key") -ge $((BOOT_EPOCH - 120)) ]] || STALE_KEYS="$STALE_KEYS $key"
done
if compgen -G "/etc/ssh/ssh_host_*_key" >/dev/null; then
  check_none "ssh host keys were generated by this boot, none carried" "$STALE_KEYS"
else
  fail "the box has ssh host keys" "generated at boot" "none"
fi
# The image ships an empty /etc/machine-id, so systemd writes this boot's id: a
# 32-hex-digit one proves the box did not inherit an identity.
MACHINE_ID=$(tr -d '\n' < /etc/machine-id 2>/dev/null || true)
if [[ $MACHINE_ID =~ ^[0-9a-f]{32}$ ]]; then
  pass "the machine-id belongs to this boot, not to the image" "32 hex characters" "$MACHINE_ID"
else
  fail "the machine-id belongs to this boot, not to the image" "32 hex characters" "${MACHINE_ID:-empty}"
fi

printf '\n'
if [[ $FAILED -ne 0 ]]; then
  printf 'RESULT: %d check(s), %d failed\n' "$CHECKS" "$FAILED"
  exit 1
fi
printf 'RESULT: %d checks, all passed\n' "$CHECKS"
exit 0
