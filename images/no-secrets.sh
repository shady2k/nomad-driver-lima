#!/bin/bash
# images/no-secrets.sh -- prove that an image carries no credential.
#
# Runs as root inside a box or a build VM (by images/build.sh before the disk is
# captured, and by images/verify.sh against a booted image). A published image
# goes to anyone, so nothing a box ever receives as a credential may be in it:
# the rule is "no secret in an image", and this is where it is checked.
#
# In a box the same paths exist but live on tmpfs: /run/box/secrets is where the
# Nomad driver puts the credentials and /run/boxd is where boxd derives the
# executor configuration from them. Neither survives a reboot, and neither is in
# an image: /run of a fresh boot is empty.
#
# Usage (inside the guest, as root): images/no-secrets.sh [version]
# Prints one line per check and exits non-zero on the first failed one.
set -uo pipefail

VERSION=${1:-unknown}
FAILED=0
say() { printf '%s\n' "$*"; }
ok() { printf 'ok    %s\n' "$*"; }
bad() { printf 'FAIL  %s\n' "$*"; FAILED=1; }

say "== no credential in the image ($VERSION)"

# Directories that must be missing or empty: every one of them is somewhere an
# executor, boxd or the driver writes a credential or a credential-derived
# configuration.
DIRS=(
  /etc/box/secrets
  /var/lib/boxd/executors
  /root/.prime
  /root/.config/opencode
  /root/.local/share/opencode
  /root/.claude
)
for home in /home/*; do
  [ -d "$home" ] || continue
  DIRS+=("$home/.prime" "$home/.config/opencode" "$home/.local/share/opencode" "$home/.claude")
done
for dir in "${DIRS[@]}"; do
  if [ ! -e "$dir" ]; then
    ok "$dir does not exist"
    continue
  fi
  entries=$(find "$dir" -mindepth 1 -maxdepth 3 2>/dev/null | head -5)
  if [ -z "$entries" ]; then
    ok "$dir is empty"
  else
    bad "$dir is not empty:"
    printf '%s\n' "$entries" | sed 's/^/        /'
  fi
done

# A credential file that reached the disk anywhere below an account or the box
# directories. The names are the ones every executor writes a credential to; the
# image's own /etc/opencode/opencode.json is the system configuration, which is
# why /etc/opencode is not scanned.
say "== credential file names on the disk"
HITS=$(for root in /root /home /etc/box /var/lib/boxd /opt/box; do
  [ -d "$root" ] || continue
  find "$root" -xdev \
    \( -name node_modules -o -name .git -o -name .npm -o -name .cache -o -name mod \) -prune -o \
    -type f \( -name auth.json -o -name credentials.json -o -name .credentials.json \) -print 2>/dev/null
done)
if [ -z "$HITS" ]; then
  ok "no auth.json, credentials.json or .credentials.json below /root, /home, /etc/box, /var/lib/boxd, /opt/box"
else
  bad "credential files are in the image:"
  printf '%s\n' "$HITS" | sed 's/^/        /'
fi

# The tmpfs locations: in a running box they hold the credentials the driver put
# there and whatever boxd derived from them, and that is exactly where they may
# be. What must not be there is a credential *file* of an executor, and the
# location must really be a tmpfs (a directory on the disk would defeat the
# point). In an image build they either do not exist or are an empty boxd runtime
# directory.
say "== tmpfs locations"
for dir in /run/box/secrets /run/boxd; do
  if [ ! -e "$dir" ]; then
    ok "$dir does not exist"
    continue
  fi
  fstype=$(findmnt -no FSTYPE --target "$dir" 2>/dev/null || echo unknown)
  if [ "$fstype" != tmpfs ]; then
    bad "$dir is on $fstype, not tmpfs: a credential there would reach the disk"
    continue
  fi
  hits=$(find "$dir" -type f \( -name auth.json -o -name settings.json -o -name opencode.json \
      -o -name credentials.json -o -name .credentials.json -o -name env \) 2>/dev/null)
  if [ -z "$hits" ]; then
    ok "$dir is a tmpfs and holds no executor configuration"
  else
    bad "$dir holds executor configuration in an image:"
    printf '%s\n' "$hits" | sed 's/^/        /'
  fi
done

if [ "$FAILED" -ne 0 ]; then
  say ""
  say "RESULT: FAILED (this image must not be published)"
  exit 1
fi
say ""
say "RESULT: no credential in the image"
