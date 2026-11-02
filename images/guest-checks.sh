#!/bin/bash
# images/guest-checks.sh -- the checks a published box image must pass.
#
# Runs as root inside a booted box (by images/verify.sh, through 'limactl
# shell'), and proves the two properties the image exists for: the package
# sources it configures are the only ones it talks to, and the tool set is
# installed and working. It prints one report and exits non-zero when any check
# failed.
#
# It takes no arguments: what the image should use is read from the image's own
# configuration (the apt sources file, the npm/pip/go configuration, the docker
# daemon), so it makes no assumption about a mirror or a public upstream.
#
# Usage (inside the guest): images/guest-checks.sh
set -uo pipefail

# The versions this image is expected to carry (images/provision.sh).
PRIME_WANT=${PRIME_AGENT_VERSION:-0.10.2}
OPENCODE_WANT=${OPENCODE_VERSION:-2.2.5}
FAILED=0
say() { printf '%s\n' "$*"; }
ok() { printf 'ok    %s\n' "$*"; }
bad() { printf 'FAIL  %s\n' "$*"; FAILED=1; }
check() { # check <description> <command...>
  local what=$1; shift
  if "$@" >/tmp/check.out 2>&1; then ok "$what"; else bad "$what"; sed 's/^/        /' /tmp/check.out; fi
  rm -f /tmp/check.out
}

say "== identity"
say "kernel       $(uname -r)"
say "os           $(grep -m1 PRETTY_NAME /etc/os-release | cut -d= -f2- | tr -d '\"')"
say "user         $(id -un) ($(id -u):$(id -g))"
say "machine-id   $(cat /etc/machine-id)"
say "cloud-init   $(cloud-init --version 2>/dev/null | head -1)"

say "== apt: only the sources this image configures"
for f in /etc/apt/sources.list /etc/apt/sources.list.d/*.sources /etc/apt/sources.list.d/*.list; do
  [ -f "$f" ] || continue
  say "$f:"
  sed 's/^/  /' "$f"
done
# The hosts the image itself points apt at: whatever they are, apt may fetch
# from those and from nothing else.
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
if [ -z "$APT_HOSTS" ]; then
  bad "the image configures no apt source at all"
else
  ok "apt sources are configured: $APT_HOSTS"
fi
if apt-get update >/tmp/apt-update.txt 2>&1; then
  sed 's/^/  /' /tmp/apt-update.txt
  if python3 - /tmp/apt-update.txt $APT_HOSTS <<'PY'
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
if bad:
    print("apt fetched from a host the image does not configure:")
    print("\n".join(bad))
    raise SystemExit(1)
PY
  then
    ok "apt-get update only used the configured sources"
  else
    bad "apt-get update reached a host the image does not configure"
  fi
else
  bad "apt-get update failed"
  sed 's/^/        /' /tmp/apt-update.txt
fi

say "== docker"
if systemctl start docker.service 2>/dev/null; then ok "docker.service starts"; else bad "docker.service does not start"; fi
check "docker version" docker --version
say "registry mirrors: $(docker info --format '{{join .RegistryConfig.Mirrors ","}}' 2>/dev/null)"
if docker pull alpine:3.20 2>&1 | tail -3; then ok "docker pull alpine:3.20"; else bad "docker pull alpine:3.20"; fi
docker image inspect alpine:3.20 --format 'image alpine:3.20 id {{.Id}}' >/dev/null 2>&1 \
  && ok "the pulled image is present" || bad "the pulled image is missing"

say "== package managers (npm $(npm config get registry 2>/dev/null), pip, uv, Go $(go env GOPROXY 2>/dev/null))"
check "npm view left-pad version ($(npm config get registry 2>/dev/null))" npm view left-pad version
check "pip download six" bash -c 'rm -rf /tmp/piptest && python3 -m pip download --no-deps -q -d /tmp/piptest six && ls /tmp/piptest'
check "uv index ($(uv --version))" bash -c 'rm -rf /tmp/uvtest && uv pip install --dry-run --no-cache six 2>&1 | tail -3'
check "go get github.com/pkg/errors" bash -c 'd=$(mktemp -d) && cd "$d" && go mod init check >/dev/null 2>&1 && go get github.com/pkg/errors@v0.9.1 >/dev/null 2>&1 && grep -q "github.com/pkg/errors v" go.sum'
check "go mod download github.com/pkg/errors" bash -c 'd=$(mktemp -d) && cd "$d" && go mod init check >/dev/null 2>&1 && go mod download github.com/pkg/errors@v0.9.1'

say "== tools"
say "git          $(git --version)"
say "curl         $(curl --version | head -1)"
say "jq           $(jq --version)"
say "rg           $(rg --version | head -1)"
say "gcc          $(gcc --version | head -1)"
say "python3      $(python3 --version)"
say "pip          $(python3 -m pip --version | cut -d' ' -f1-2)"
say "uv           $(uv --version)"
say "node         $(node --version)"
say "npm          $(npm --version)"
say "go           $(go version)"
say "docker       $(docker --version)"
say "buildx       $(docker buildx version 2>/dev/null || echo absent)"
say "compose      $(docker compose version --short 2>/dev/null || echo absent)"
# An executor's --version is not a read-only question: opencode creates its data
# directory and a log in $HOME, and this script runs as root, whose home is on the
# box disk. The probes therefore run with a scratch HOME on the tmpfs, so a check
# never leaves anything an image must not carry (see no-secrets.sh below).
CHECK_HOME=/run/box-checks-home
mkdir -p "$CHECK_HOME"
say "prime-agent  $(HOME=$CHECK_HOME prime-agent --version 2>&1 | head -1)"
check "prime-agent --version" env HOME=$CHECK_HOME prime-agent --version
say "opencode     $(HOME=$CHECK_HOME opencode --version 2>&1 | head -1)"
check "opencode --version is v$OPENCODE_WANT" bash -c "[ \"\$(HOME=$CHECK_HOME opencode --version 2>&1 | head -1)\" = \"opencode v$OPENCODE_WANT\" ]"
check "/etc/opencode/opencode.json disables updates" bash -c "python3 -c 'import json,sys; sys.exit(0 if json.load(open(\"/etc/opencode/opencode.json\")).get(\"update\") == \"disable\" else 1)'"
say "go env       GOPROXY=$(go env GOPROXY) GOSUMDB=$(go env GOSUMDB)"
say "npm registry $(npm config get registry)"
say "pip index    $(python3 -m pip config get global.index-url 2>/dev/null || python3 -m pip config list 2>/dev/null | tr '\n' ' ')"
say "environment  $(grep -c . /etc/environment) variables in /etc/environment"

say "== disk"
ROOT_SIZE=$(df -B1 --output=size / | tail -1 | tr -d ' ')
ROOT_SRC=$(findmnt -no SOURCE /)
PART_SIZE=$(blockdev --getsize64 "$ROOT_SRC" 2>/dev/null || echo 0)
say "root          $ROOT_SRC: filesystem $ROOT_SIZE bytes, partition $PART_SIZE bytes"
if [[ $PART_SIZE -gt 0 ]] && ((ROOT_SIZE * 100 / PART_SIZE >= 95)); then
  ok "the root filesystem fills its partition ($ROOT_SIZE of $PART_SIZE bytes)"
else
  bad "the root filesystem does not fill its partition: $ROOT_SIZE of $PART_SIZE bytes (box-grow-rootfs)"
fi

say "== box"
if [[ -d /opt/box ]]; then ok "/opt/box exists"; ls -ld /opt/box; else bad "/opt/box is missing"; fi

say "== boxd"
say "boxd binary  $(command -v boxd 2>/dev/null || echo missing)"
say "boxd version $(boxd --version 2>&1 | head -1)"
say "boxctl       $(command -v boxctl 2>/dev/null || echo missing)"
if systemctl is-enabled --quiet boxd.service 2>/dev/null; then ok "boxd.service is enabled"; else bad "boxd.service is not enabled"; fi
if systemctl is-active --quiet boxd.service 2>/dev/null; then ok "boxd.service is active"; else bad "boxd.service is not active"; fi
BOXCTL_INFO=$(boxctl info 2>&1)
BOXCTL_STATUS=$?
say "boxctl info: $BOXCTL_INFO"
if [[ $BOXCTL_STATUS -ne 0 ]]; then
  bad "boxctl info failed"
elif python3 - "$BOXCTL_INFO" "$PRIME_WANT" "$OPENCODE_WANT" <<'PY'
import json, sys
try:
    info = json.loads(sys.argv[1])
except Exception as exc:
    print("boxctl info is not JSON:", exc)
    raise SystemExit(1)
prime_want, opencode_want = sys.argv[2], sys.argv[3]
executors = {ex.get("name"): ex for ex in info.get("executors", [])}
missing = []
for name, want, match in (("prime", prime_want, lambda v: v == want),
                          ("opencode", "v" + opencode_want, lambda v: want in v)):
    ex = executors.get(name)
    if not ex or not ex.get("available") or not match(ex.get("version", "")):
        missing.append(f"{name}: {ex}")
if missing:
    print("; ".join(missing))
    raise SystemExit(1)
PY
then
  ok "boxctl info lists prime $PRIME_WANT and opencode $OPENCODE_WANT as available"
else
  bad "boxctl info does not list prime $PRIME_WANT and opencode $OPENCODE_WANT as available"
fi

say "== no credential in the image"
# A published image goes to anyone: no secret may be in it. The check itself is
# images/no-secrets.sh, which images/build.sh also runs before it captures the
# disk; verify.sh copies it next to this script.
if [[ -f /tmp/no-secrets.sh ]]; then
  if bash /tmp/no-secrets.sh "guest checks" 2>&1 | sed 's/^/  /'; then
    ok "the image carries no credential"
  else
    bad "the image carries a credential (see above)"
  fi
else
  bad "images/no-secrets.sh is missing; the image could carry a credential"
fi

say "== cloud-init and accounts"
CI_STATUS=$(cloud-init status --long 2>/dev/null | tr '\n' ' ')
say "cloud-init status: $CI_STATUS"
if grep -q 'status: done' <<<"$CI_STATUS"; then ok "cloud-init finished without a failed stage"; else bad "cloud-init did not finish cleanly"; fi
say "accounts with uid >= 1000: $(getent passwd | awk -F: '$3 >= 1000 {printf "%s(%s) ", $1, $3}')"
if getent passwd builder >/dev/null; then bad "the build account 'builder' is in the image"; else ok "no build account in the image"; fi

say "== hygiene"
# The image ships no host keys; the ones here must be younger than this boot.
BOOT_EPOCH=$(date -d "$(uptime -s)" +%s 2>/dev/null || echo 0)
STALE=0
for key in /etc/ssh/ssh_host_*_key; do
  [[ -e $key ]] || continue
  [[ $(stat -c %Y "$key") -ge $((BOOT_EPOCH - 120)) ]] || { STALE=1; say "old host key: $key"; }
done
if [[ $STALE -eq 0 ]] && compgen -G "/etc/ssh/ssh_host_*_key" >/dev/null; then
  ok "ssh host keys were generated by this boot, not carried in the image"
else
  bad "the image carries ssh host keys from before this boot"
fi
say "authorized keys in /home: $(find /home -name authorized_keys 2>/dev/null | wc -l) files (created by cloud-init at boot)"

if [[ $FAILED -ne 0 ]]; then
  say ""
  say "RESULT: FAILED"
  exit 1
fi
say ""
say "RESULT: all checks passed"
