#!/bin/bash
# images/provision.sh -- the complete guest recipe for the box base image.
#
# This script runs as root inside a Lima instance built from the Ubuntu 24.04
# (noble) cloud image. Everything the qcow2 contains is created here, apart from
# two steps images/build.sh takes after it: the stub boxd.service -- whose
# contents are boxd's own, printed with "boxd -print-unit" -- and
# /etc/box/image.json, the record of what the image is. guest/no-secrets.sh is
# the last check before the disk is captured.
#
# The image carries the OS and the toolchain, not the part that changes often:
# boxd and the executors are in a versioned platform bundle that the node mounts
# read-only at /opt/box before the guest boots (docs/guest-image.md). So /opt/box
# is an empty mount point here, /etc/profile.d/box-platform.sh puts its bin/ on
# the PATH, the manifest records "platform": "mount", and boxd, the executors and
# the add-ons are built by images/platform.sh instead.
#
# The recipe is this repository's, and the image it builds is the operator's:
# nothing here publishes anything. docs/guest-image.md is the contract the image
# meets, and what an image of your own has to provide instead.
#
# Inputs (environment; images/build.sh exports them before the script body):
#   BOX_ARCH         amd64 | arm64            (default: the guest's architecture)
#   BOX_VERSION      image version being built (default: dev)
#
# Every package and archive comes from its public upstream unless the matching
# BOX_MIRROR_* variable points it somewhere else. These are deployment settings:
# an operator who runs a mirror sets them for their own build, and this
# repository names no private host. The versions and their sha256 pins stay in
# force either way, so a mirror has to serve the same bytes or the build stops.
# The pins below were re-checked against their public upstreams when this recipe
# came back (2026-11-03): the noble cloud images, Node 24.22.2, Go 1.27.3 and
# uv 0.13.5 are the bytes their own checksum files describe. The executors are not
# in this image any more; their pins are in images/pins.sh, where the platform
# bundle's recipe reads them.
#
#   BOX_MIRROR_APT           Ubuntu archive
#                            (default http://archive.ubuntu.com/ubuntu, or
#                            http://ports.ubuntu.com/ubuntu-ports on arm64)
#   BOX_MIRROR_APT_SECURITY  Ubuntu security archive (default
#                            http://security.ubuntu.com/ubuntu, or the arm64
#                            ports archive; BOX_MIRROR_APT alone covers both)
#   BOX_MIRROR_NPM           npm registry (default https://registry.npmjs.org/)
#   BOX_MIRROR_PYPI          pip and uv index (default https://pypi.org/simple)
#   BOX_MIRROR_GO            Go module proxy (default https://proxy.golang.org)
#   BOX_MIRROR_GO_DIST       Go release tarballs (default https://go.dev/dl)
#   BOX_MIRROR_GITHUB        GitHub release assets (default https://github.com)
#   BOX_MIRROR_NODE          Node.js release files (default https://nodejs.org/dist)
#   BOX_MIRROR_DOCKER        Docker Hub registry mirror (default: none, the
#                            daemon pulls from Docker Hub itself)
#
# The one difference a mirror makes inside a box is the Go sum database: see
# GOSUMDB below.
set -euo pipefail

export DEBIAN_FRONTEND=noninteractive
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export LC_ALL=C
# This script runs as root from Lima's per-boot cloud-init script, which has no
# HOME. Go, npm and uv all derive their caches from it, so it is set explicitly
# (without it: 'go: module cache not found: neither GOMODCACHE nor GOPATH is set').
export HOME=${HOME:-/root}

BOX_VERSION=${BOX_VERSION:-dev}
STATE=/var/lib/box-image-build
REPORT=$STATE/report.txt
GUEST_CHECKS=$STATE/guest-checks.txt
MANIFEST=$STATE/manifest.json

log() { printf '\n=== %s\n' "$*"; }
fail() { printf 'provision: %s\n' "$*" >&2; exit 1; }
# tee_report writes a command's output to the report as well as the console.
tee_report() { "$@" 2>&1 | tee -a "$REPORT"; return "${PIPESTATUS[0]}"; }

case "${BOX_ARCH:-$(uname -m)}" in
  amd64|x86_64) BOX_ARCH=amd64 ;;
  arm64|aarch64) BOX_ARCH=arm64 ;;
  *) fail "unsupported architecture ${BOX_ARCH:-$(uname -m)}" ;;
esac

. /etc/os-release
[ "$ID:$VERSION_ID" = ubuntu:24.04 ] || fail "the guest is $ID:$VERSION_ID, want ubuntu:24.04"
[ "$(dpkg --print-architecture)" = "$BOX_ARCH" ] || \
  fail "the guest architecture is $(dpkg --print-architecture), want $BOX_ARCH"

# Pinned versions. Changing any of them changes the image, so bump BOX_VERSION
# (or build a new version) rather than editing an already published one.
NODE_VERSION=24.22.2
GO_VERSION=1.27.3
UV_VERSION=0.13.5

case $BOX_ARCH in
  amd64)
    DEFAULT_APT_ARCHIVE=http://archive.ubuntu.com/ubuntu
    DEFAULT_APT_SECURITY=http://security.ubuntu.com/ubuntu
    NODE_ARCH=x64
    NODE_SHA256=48cc0e21e048790e54c0cf5749c2725005313d652651c40b7a21feb9697aaf75
    GO_SHA256=65efbc80fdc1fa2cd2631cad5b481777a68e8725f34e89f50fc6ac714e379b40
    ;;
  arm64)
    DEFAULT_APT_ARCHIVE=http://ports.ubuntu.com/ubuntu-ports
    DEFAULT_APT_SECURITY=http://ports.ubuntu.com/ubuntu-ports
    NODE_ARCH=arm64
    NODE_SHA256=eb187837a2230d4047161ac0be008584e16b46f726e9c9490ee35dc6b17d9ba8
    GO_SHA256=a06e99309dbcbdca1a8ccd2282c3ee146e55930886e26b4f1193269587165f49
    ;;
esac

# A mirror for the archive covers the security suite too; a mirror that serves
# only one of them sets BOX_MIRROR_APT_SECURITY as well.
APT_ARCHIVE_URL=${BOX_MIRROR_APT:-$DEFAULT_APT_ARCHIVE}
APT_SECURITY_URL=${BOX_MIRROR_APT_SECURITY:-${BOX_MIRROR_APT:-$DEFAULT_APT_SECURITY}}
NPM_REGISTRY=${BOX_MIRROR_NPM:-https://registry.npmjs.org/}
NPM_REGISTRY=${NPM_REGISTRY%/}/
PIP_INDEX=${BOX_MIRROR_PYPI:-https://pypi.org/simple}
GOPROXY_URL=${BOX_MIRROR_GO:-https://proxy.golang.org}
GO_DIST_URL=${BOX_MIRROR_GO_DIST:-https://go.dev/dl}
GITHUB_URL=${BOX_MIRROR_GITHUB:-https://github.com}
NODE_DIST_URL=${BOX_MIRROR_NODE:-https://nodejs.org/dist}
DOCKER_MIRROR=${BOX_MIRROR_DOCKER:-}
# uv downloads CPython from python-build-standalone's GitHub releases, so a
# GitHub mirror serves those too.
UV_PYTHON_MIRROR=$GITHUB_URL/astral-sh/python-build-standalone/releases/download

NODE_URL=$NODE_DIST_URL/v$NODE_VERSION/node-v$NODE_VERSION-linux-$NODE_ARCH.tar.xz
GO_URL=$GO_DIST_URL/go$GO_VERSION.linux-$BOX_ARCH.tar.gz

# The public Go proxy serves the checksum database, so a box verifies module
# sums by default. A mirror rarely does (proxy.golang.org answers
# /sumdb/sum.golang.org/... , a raw proxy does not), so the sum database is
# turned off exactly when a Go mirror is configured.
if [ -n "${BOX_MIRROR_GO:-}" ]; then
  GOSUMDB=off
else
  GOSUMDB=sum.golang.org
fi

mkdir -p "$STATE"
: > "$REPORT"
# The build host polls this file: 'limactl start' returns as soon as the
# instance is running, which is before (and independently of) this script, so
# the image build needs the recipe's own verdict. RUNNING -> DONE, or FAILED
# with the line that failed.
printf 'RUNNING\n' > "$STATE/state"
trap 'printf "FAILED at line %s of %s\n" "$LINENO" "$(basename "$0")" > "$STATE/state"' ERR
exec > >(tee -a "$REPORT") 2>&1

log "provisioning box base image $BOX_VERSION for $BOX_ARCH"
# The sources this build really uses: a mirror or the public upstream.
printf 'apt archive      %s\n' "$APT_ARCHIVE_URL"
printf 'apt security     %s\n' "$APT_SECURITY_URL"
printf 'node             %s\n' "$NODE_URL"
printf 'go               %s\n' "$GO_URL"
printf 'go modules       %s (GOSUMDB %s)\n' "$GOPROXY_URL" "$GOSUMDB"
printf 'npm              %s\n' "$NPM_REGISTRY"
printf 'pypi             %s\n' "$PIP_INDEX"
printf 'docker mirror    %s\n' "${DOCKER_MIRROR:-none}"

# download URL DEST SHA256 -- fetch and refuse to continue on a mismatch.
# http is allowed for a mirror on a private network; the sha256 pin is what
# decides whether the bytes are the ones this recipe expects.
download() {
  local url=$1 dest=$2 sha=$3
  curl --fail --silent --show-error --location --retry 3 --retry-delay 2 \
       --proto '=https,http' --proto-redir '=https,http' --tlsv1.2 \
       "$url" -o "$dest.part"
  printf '%s  %s\n' "$sha" "$dest.part" | sha256sum -c --status - \
    || { rm -f "$dest.part"; fail "sha256 mismatch for $url"; }
  mv "$dest.part" "$dest"
}

# --- apt: the two sources this image uses, and the suites they serve ---------
log "apt sources"
# Three suites, noble, noble-updates and noble-security. The cloud image ships
# packages from noble-updates, so without that suite a -dev package of an
# installed library has no candidate of the same version and cannot install
# (libdbus-1-3 from -updates, libdbus-1-dev only from noble). A mirror that
# carries the archive carries -updates; backports stay out.
rm -f /etc/apt/sources.list /etc/apt/sources.list.d/*.list /etc/apt/sources.list.d/*.sources
cat > /etc/apt/sources.list.d/box-apt.sources <<EOF
# Written by images/provision.sh: the apt sources of this image, the public
# Ubuntu archive unless BOX_MIRROR_APT pointed it at a mirror. Nothing else may
# be added: 'apt-get update' must not reach another host.
Types: deb
URIs: $APT_ARCHIVE_URL
Suites: noble noble-updates
Components: main restricted universe multiverse
Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg

Types: deb
URIs: $APT_SECURITY_URL
Suites: noble-security
Components: main restricted universe multiverse
Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg
EOF

# cloud-init stays enabled (Lima needs it to create its user at boot), but it
# must not write the distribution's own sources back on first boot: it is told to
# leave the sources of this image alone.
cat > /etc/cloud/cloud.cfg.d/90-box-apt.cfg <<EOF
# Written by images/provision.sh. cloud-init keeps the sources this image ships
# instead of writing the distribution's own sources into
# /etc/apt/sources.list.d/ubuntu.sources at the box's first boot (they would
# bypass a configured mirror, and they duplicate the suites above).
apt:
  preserve_sources_list: true
package_update: false
package_upgrade: false
package_reboot_if_required: false
# The disk is grown by box-grow-rootfs.service after the root filesystem is
# mounted read-write; cloud-init's own growpart/resizefs would run earlier and
# leave the kernel with the image's size.
growpart:
  mode: "off"
resize_rootfs: false
EOF

# --- packages ---------------------------------------------------------------
log "installing packages"
apt-get update
apt-get install -y --no-install-recommends \
  ca-certificates curl wget git jq ripgrep build-essential make gcc \
  python3 python3-venv python3-pip \
  unzip xz-utils tar gzip zstd procps psmisc lsof file less vim htop tmux \
  iproute2 net-tools iputils-ping dnsutils rsync sqlite3 strace sudo parted \
  docker.io docker-buildx docker-compose-v2 \
  qemu-guest-agent openssh-server cloud-init earlyoom
apt-get clean

# --- docker ------------------------------------------------------------------
log "docker"
install -d -m 0755 /etc/docker
# Docker Hub is the default: a registry mirror is written only when one is
# configured, because an empty entry would be a mirror that answers nothing.
if [ -n "$DOCKER_MIRROR" ]; then
  cat > /etc/docker/daemon.json <<EOF
{
  "registry-mirrors": ["$DOCKER_MIRROR"]
}
EOF
else
  # No daemon.json at all: the defaults of the package are the ones wanted.
  rm -f /etc/docker/daemon.json
fi
systemctl enable docker.service >/dev/null
# Start the daemon now: the build verifies the mirror through the real daemon
# instead of trusting the configuration file, then leaves an empty state behind.
systemctl start docker.service
for _ in $(seq 1 60); do docker info >/dev/null 2>&1 && break; sleep 1; done
docker info --format 'docker server version: {{.ServerVersion}}' || echo 'docker daemon is not answering'

# --- earlyoom -----------------------------------------------------------------
# A box that ran out of memory did not answer at all: the guest thrashed, qemu
# spent twenty minutes inflating the clusters of its disk, no session could be
# asked anything, and nothing in the journal said why. earlyoom kills the process
# that takes the memory while the guest can still run, and its choice reaches the
# journal - which is the task log - so a box that was frozen by memory says which
# process it was.
#
# The arguments are the whole of its configuration: it runs with no secrets and
# knows nothing about the executors, and every box of this image runs it the same
# way.
#   -m 10,5     SIGTERM when 10% of the memory is available, SIGKILL below 5%.
#               Written down rather than left to the default, so an image keeps
#               the numbers it was checked with.
#   -r 0        no periodic "mem avail" report: the journal is the record of what
#               happened, not a counter that fills it.
#   --avoid     never the processes that keep the box reachable: boxd (every
#               boxctl call goes through it), sshd, systemd, lima-guestagent
#               (what limactl shell talks to) and qemu-ga (the guest agent Lima's
#               hostagent talks to). earlyoom matches the process name (comm,
#               15 characters), which is what these are.
#
# The distribution's unit is kept and its ExecStart is emptied and written again,
# so the arguments cannot drift from this recipe by way of /etc/default/earlyoom.
log "earlyoom"
install -d -m 0755 /etc/systemd/system/earlyoom.service.d
cat > /etc/systemd/system/earlyoom.service.d/box.conf <<'EOF'
# Written by images/provision.sh: what earlyoom kills, and when. An empty
# ExecStart= drops the arguments the package's unit carries.
[Service]
ExecStart=
ExecStart=/usr/bin/earlyoom -m 10,5 -r 0 --avoid '(^|/)(boxd|sshd|systemd|lima-guestagent|qemu-ga)$'
EOF
chmod 0644 /etc/systemd/system/earlyoom.service.d/box.conf
systemctl daemon-reload
systemctl enable earlyoom.service >/dev/null
systemctl start earlyoom.service
systemctl is-active --quiet earlyoom.service || fail "earlyoom.service did not start"
EARLYOOM_ARGS=$(systemctl show -p ExecStart --value earlyoom.service)
printf 'earlyoom args    %s\n' "$EARLYOOM_ARGS"
# The build continues with installs that take most of this VM's memory, and a
# process killer must not race them: the unit is enabled for a box's own boots,
# and images/guest-checks.sh checks that it is active in one.
systemctl stop earlyoom.service

# --- node --------------------------------------------------------------------
log "node $NODE_VERSION"
download "$NODE_URL" /tmp/node.tar.xz "$NODE_SHA256"
tar -xJf /tmp/node.tar.xz -C /opt
rm -f /tmp/node.tar.xz
NODE_PREFIX=/opt/node-v$NODE_VERSION-linux-$NODE_ARCH
for cmd in node npm npx corepack; do
  [ -e "$NODE_PREFIX/bin/$cmd" ] && ln -sfn "$NODE_PREFIX/bin/$cmd" "/usr/local/bin/$cmd"
done

# --- go ----------------------------------------------------------------------
log "go $GO_VERSION"
download "$GO_URL" /tmp/go.tar.gz "$GO_SHA256"
rm -rf /usr/local/go
tar -xzf /tmp/go.tar.gz -C /usr/local
rm -f /tmp/go.tar.gz
ln -sfn /usr/local/go/bin/go /usr/local/bin/go
ln -sfn /usr/local/go/bin/gofmt /usr/local/bin/gofmt

# --- the executors --------------------------------------------------------------
# The image installs no executor. prime-agent, opencode (and any other) come from
# the platform bundle the node mounts at /opt/box: they change far more often
# than a base image should, and an image that shipped them would have to be
# rebuilt and republished for every executor release. images/pins.sh holds the
# versions a bundle built by images/platform.sh carries, and docs/guest-image.md
# is what a bundle has to look like.
#
# /etc/opencode is opencode's system configuration directory, which outranks the
# user's and the project's files: a box cannot be talked into updating itself
# while the coordinator runs it. Providers and credentials are not set here; the
# box's own config file comes from the executors variable (see boxd). It stays in
# the image so that the setting applies to whatever opencode the bundle carries.
install -d -m 0755 /etc/opencode
cat > /etc/opencode/opencode.json <<'EOF'
{
  "$schema": "https://opencode.ai/config.json",
  "update": "disable"
}
EOF

# --- uv -----------------------------------------------------------------------
log "uv $UV_VERSION"
# --index-url, not the pip.conf below: uv comes before the configuration files
# are written, and a mirror-only build must not reach pypi.org here.
python3 -m pip install --break-system-packages --no-cache-dir --no-input \
  --index-url "$PIP_INDEX" "uv==$UV_VERSION"
command -v uv >/dev/null || fail "uv is not on PATH after pip install"

# --- package sources for every user ------------------------------------------
log "npm, pip, uv and Go sources for every user"
cat > /etc/npmrc <<EOF
; Written by images/provision.sh: where npm installs from. The public registry
; unless BOX_MIRROR_NPM pointed it at a mirror.
registry=$NPM_REGISTRY
audit=false
fund=false
EOF
# npm reads its global configuration from '\$prefix/etc/npmrc', and this node
# lives in /opt, so the image's /etc/npmrc is linked into the node install.
install -d -m 0755 "$NODE_PREFIX/etc"
ln -sfn /etc/npmrc "$NODE_PREFIX/etc/npmrc"
cat > /etc/pip.conf <<EOF
# Written by images/provision.sh: where pip installs from (PyPI unless
# BOX_MIRROR_PYPI pointed it at a mirror).
[global]
index-url = $PIP_INDEX
break-system-packages = true
EOF
install -d -m 0755 /etc/uv
cat > /etc/uv/uv.toml <<EOF
# Written by images/provision.sh: the system-wide uv configuration. The box has
# no per-user virtual environment, so uv installs into the system Python the way
# /etc/pip.conf lets pip do it (Ubuntu marks its Python externally managed).
python-install-mirror = "$UV_PYTHON_MIRROR"

[[index]]
url = "$PIP_INDEX"
default = true

[pip]
# Ubuntu marks its Python externally managed; a box has no per-user virtual
# environment, so uv installs into the system Python (as /etc/pip.conf lets pip).
break-system-packages = true
EOF
cat > /etc/profile.d/box-sources.sh <<EOF
# Written by images/provision.sh: the package sources of this box, for every
# shell. Public upstreams unless BOX_MIRROR_* pointed one at a mirror.
export GOPROXY=$GOPROXY_URL
export GOSUMDB=$GOSUMDB
export GOTOOLCHAIN=local
export PIP_INDEX_URL=$PIP_INDEX
export UV_DEFAULT_INDEX=$PIP_INDEX
export NPM_CONFIG_REGISTRY=$NPM_REGISTRY
EOF
chmod 0644 /etc/profile.d/box-sources.sh
# /etc/environment is what sshd (and so 'limactl shell' and the box driver)
# applies to a session that is not a login shell.
cat > /etc/environment <<EOF
# Written by images/provision.sh: the package sources of this box, and the PATH
# of its platform bundle, for sshd sessions. sshd reads this file through PAM and
# not /etc/profile.d, so a command the driver runs with 'limactl shell' finds
# boxctl at /opt/box/bin/boxctl only because the PATH is here too.
PATH=/opt/box/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
GOPROXY=$GOPROXY_URL
GOSUMDB=$GOSUMDB
GOTOOLCHAIN=local
PIP_INDEX_URL=$PIP_INDEX
UV_DEFAULT_INDEX=$PIP_INDEX
NPM_CONFIG_REGISTRY=$NPM_REGISTRY
EOF

# --- grow the root filesystem into the box's disk -----------------------------
# A box asks for a disk larger than this image's 8 GiB, and the guest has to use
# it. cloud-init's own growpart/resizefs can run while the root filesystem is
# still read-only, and growpart refuses a root partition that does not carry the
# highest partition number (this image numbers its EFI and /boot partitions 14 to
# 16), so the growing is a script of its own, run after the root is mounted
# read-write. parted resizes the partition whatever its number, partx tells the
# kernel, resize2fs grows the filesystem. It is idempotent.
install -d -m 0755 /usr/local/sbin
cat > /usr/local/sbin/box-grow-rootfs.sh <<'EOF'
#!/bin/sh
# Written by images/provision.sh: grow the root partition and filesystem into
# whatever disk the box was given. Safe to run on every boot.
set -eu
src=$(findmnt -no SOURCE /)
part=${src##*/}
disk=/dev/${part%%[0-9]*}
num=${part#"${part%%[0-9]*}"}
[ -b "$src" ] || exit 0
if command -v parted >/dev/null 2>&1; then
  parted -s -f "$disk" resizepart "$num" 100% >/dev/null 2>&1 || true
  partx -u "$disk" >/dev/null 2>&1 || true
fi
resize2fs "$src" || true
EOF
chmod 0755 /usr/local/sbin/box-grow-rootfs.sh
cat > /etc/systemd/system/box-grow-rootfs.service <<'EOF'
# Written by images/provision.sh: run box-grow-rootfs.sh once the root
# filesystem is mounted read-write.
[Unit]
Description=Grow the root filesystem into the box's disk
After=local-fs.target systemd-remount-fs.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/sbin/box-grow-rootfs.sh

[Install]
WantedBy=multi-user.target
EOF
systemctl enable box-grow-rootfs.service >/dev/null
chmod 0644 /etc/systemd/system/box-grow-rootfs.service

# --- the platform bundle's mount point ----------------------------------------
# boxd and the executors are not in this image: the node downloads a versioned
# platform bundle and mounts it read-only at /opt/box before the guest boots
# (docs/guest-image.md). The image provides the empty mount point and the PATH
# that finds what the mount holds. boxd keeps its durable state in /var/lib/boxd,
# which its unit creates; nothing runs from /opt/box in the build VM.
install -d -m 0755 /opt/box
cat > /etc/profile.d/box-platform.sh <<'EOF'
# Written by images/provision.sh: the platform bundle the node mounts read-only
# at /opt/box is where this box's boxd, boxctl and executors are. An image
# without a bundle mounted leaves the PATH alone.
if [ -d /opt/box/bin ]; then
  PATH=/opt/box/bin:$PATH
  export PATH
fi
EOF
chmod 0644 /etc/profile.d/box-platform.sh

# box and the driver call the guest through this path, on every image: an image
# with its own boxd has the binary here, and this one has a link into the bundle.
ln -sfn /opt/box/bin/boxctl /usr/local/bin/boxctl

# --- checks: they fail the build, so a published image is a checked image -----
log "checks"
: > "$GUEST_CHECKS"
{ printf 'image      %s %s\n' "$BOX_VERSION" "$BOX_ARCH"
  printf 'base       %s\n' "$(grep -m1 PRETTY_NAME /etc/os-release | cut -d= -f2- | tr -d '\"')"
  printf 'kernel     %s\n' "$(uname -r)"
  printf 'git        %s\n' "$(git --version)"
  printf 'curl       %s\n' "$(curl --version | head -1)"
  printf 'jq         %s\n' "$(jq --version)"
  printf 'rg         %s\n' "$(rg --version | head -1)"
  printf 'gcc        %s\n' "$(gcc --version | head -1)"
  printf 'python3    %s\n' "$(python3 --version)"
  printf 'pip        %s\n' "$(python3 -m pip --version | cut -d' ' -f1-2)"
  printf 'uv         %s\n' "$(uv --version)"
  printf 'node       %s\n' "$(node --version)"
  printf 'npm        %s\n' "$(npm --version)"
  printf 'go         %s\n' "$(go version)"
  printf 'docker     %s\n' "$(docker --version)"
  printf 'buildx     %s\n' "$(docker buildx version 2>/dev/null || echo absent)"
  printf 'compose    %s\n' "$(docker compose version --short 2>/dev/null || echo absent)"
  printf 'opencode cfg %s\n' "$(tr -d '\n ' < /etc/opencode/opencode.json)"
  printf 'earlyoom   %s\n' "$(earlyoom -v 2>&1 | head -1)"
  printf 'earlyoom args %s\n' "$EARLYOOM_ARGS"
  printf 'platform   mount (the node mounts a bundle at /opt/box)\n'
} | tee "$GUEST_CHECKS"

# apt must only talk to the sources this image configures. 'apt-get update'
# lists every URL it fetched; a host that is not one of them is a leak and fails
# the build.
apt-get update 2>&1 | tee "$STATE/apt-update.txt"
python3 - "$STATE/apt-update.txt" "$APT_ARCHIVE_URL" "$APT_SECURITY_URL" <<'PY' \
  || fail "apt-get update reached a host that is not a configured source"
import re, sys

log, urls = sys.argv[1], sys.argv[2:]
hosts = {re.sub(r"^[a-z+]+://([^/:]+).*", r"\1", u) for u in urls}
bad = []
for line in open(log, errors="replace"):
    m = re.match(r"^(?:Get|Hit|Ign):\S*\s+(\w+://\S+)", line)
    if not m:
        continue
    host = re.sub(r"^\w+://([^/:]+).*", r"\1", m.group(1))
    if host not in hosts:
        bad.append(line.rstrip())
if bad:
    print("apt fetched from a host that is not a configured source:")
    print("\n".join(bad))
    sys.exit(1)
PY
printf 'apt sources  %s\n' "$(grep -c '^URIs:' /etc/apt/sources.list.d/box-apt.sources) stanzas, every fetch from $(printf '%s ' "$APT_ARCHIVE_URL" "$APT_SECURITY_URL")" \
  | tee -a "$GUEST_CHECKS"

log "check: docker pull (through $DOCKER_MIRROR if one is configured, Docker Hub otherwise)"
docker pull alpine:3.20 | tee -a "$GUEST_CHECKS"
docker rmi alpine:3.20 >/dev/null

log "check: npm from $NPM_REGISTRY"
npm view left-pad version | tee -a "$GUEST_CHECKS"
log "check: pip from $PIP_INDEX"
( cd /tmp && python3 -m pip download --no-deps --no-cache-dir -q -d /tmp/piptest six \
  && ls /tmp/piptest ) | tee -a "$GUEST_CHECKS"
rm -rf /tmp/piptest
log "check: uv from $PIP_INDEX"
( cd /tmp && uv pip install --system --dry-run --no-cache six 2>&1 | tail -3 ) | tee -a "$GUEST_CHECKS"
log "check: go get through $GOPROXY_URL (GOSUMDB $GOSUMDB)"
GO_WORK=$(mktemp -d)
# The provision script runs from cloud-init, which does not read
# /etc/environment (that is a PAM file), so the sources are passed here.
( cd "$GO_WORK" \
  && go mod init boxcheck \
  && GOPROXY="$GOPROXY_URL" GOSUMDB="$GOSUMDB" GOFLAGS=-mod=mod go get github.com/pkg/errors@v0.9.1 \
  && cat go.sum ) 2>&1 | tee -a "$GUEST_CHECKS"
rm -rf "$GO_WORK"

# --- manifest ----------------------------------------------------------------
log "manifest"
# "gaps" is where the recipe would record a download it had to take from
# somewhere other than a configured source. Every source above is the one that
# was configured (public upstream unless a BOX_MIRROR_* said otherwise), so the
# list is empty; the field stays because the index schema carries it.
#
# The versions are collected first: inside the heredoc below, quotes and $ are
# expanded by the same shell that would have to parse them.
TOOL_NODE=$(node --version)
TOOL_NPM=$(npm --version)
TOOL_GO=$(go version | cut -d' ' -f3)
TOOL_PYTHON=$(python3 --version | cut -d' ' -f2)
TOOL_UV=$(uv --version | cut -d' ' -f2)
TOOL_DOCKER=$(docker --version | cut -d' ' -f3 | tr -d ,)
TOOL_GIT=$(git --version | cut -d' ' -f3)
BASE_PRETTY=$(grep -m1 PRETTY_NAME /etc/os-release | cut -d= -f2- | tr -d '"')
KERNEL=$(uname -r)
BUILT_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)

# "platform": "mount" is the image saying it expects a platform bundle at
# /opt/box. images/build.sh copies it into /etc/box/image.json and
# images/publish.sh into the image's index entry, so the driver only mounts a
# bundle into an image that says it wants one.
cat > "$MANIFEST" <<EOF
{
  "version": "$BOX_VERSION",
  "arch": "$BOX_ARCH",
  "built_at": "$BUILT_AT",
  "base_image": "$BASE_PRETTY",
  "kernel": "$KERNEL",
  "platform": "mount",
  "tools": {
    "node": "$TOOL_NODE",
    "npm": "$TOOL_NPM",
    "go": "$TOOL_GO",
    "python3": "$TOOL_PYTHON",
    "uv": "$TOOL_UV",
    "docker": "$TOOL_DOCKER",
    "git": "$TOOL_GIT"
  },
  "sources": {
    "apt": ["$APT_ARCHIVE_URL", "$APT_SECURITY_URL"],
    "npm": "$NPM_REGISTRY",
    "pypi": "$PIP_INDEX",
    "go": "$GOPROXY_URL",
    "go_dist": "$GO_DIST_URL",
    "node": "$NODE_DIST_URL",
    "github": "$GITHUB_URL",
    "docker_mirror": "${DOCKER_MIRROR:-none}"
  },
  "gaps": []
}
EOF
# Every tool version must be there: an empty string would silently publish an
# index entry that says nothing about the image.
for name in node npm go python3 uv docker git; do
  python3 - "$MANIFEST" "$name" <<'PY' || fail "the manifest has no version for $name"
import json, sys
value = json.load(open(sys.argv[1]))["tools"][sys.argv[2]]
sys.exit(0 if value else 1)
PY
done

python3 -c "import json,sys; json.load(open('$MANIFEST'))" || fail "the manifest is not valid JSON"
cat "$MANIFEST"

# --- trim: what a box never uses, and the space a store should not have to take -
# An HTTP store (or a proxy in front of one) may refuse a body above 1 GiB --
# measured on one: 1024 MiB is accepted, 1100 MiB is answered with HTTP 413 --
# so the published qcow2 is kept well below that. The server image carries two
# things a box does not need: the eBPF tracing stack
# (bpftrace, libbpfcc and with them llvm-18, 180 MiB on its own) and snapd, plus
# the kernel headers of the build kernel. Removing them and the documentation is
# what makes the image fit; nothing the box runs depends on any of it.
log "trimming what a box does not use"
# The kernel headers are named by the pinned base image, not here: whatever
# linux-headers-* the cloud image carries is what nothing in a box compiles
# against at boot.
KERNEL_HEADERS=$(dpkg-query -W -f='${Package}\n' 'linux-headers-*' 2>/dev/null || true)
# shellcheck disable=SC2086  # one word per package, which is what apt-get wants
apt-get purge -y --auto-remove \
  bpftrace libbpfcc python3-bpfcc snapd $KERNEL_HEADERS 2>&1 | tail -5 || true
apt-get autoremove -y --purge 2>&1 | tail -5 || true
rm -rf /usr/share/doc/* /usr/share/man/* /usr/share/info/* /usr/share/locale/* \
  /usr/share/i18n /usr/share/ieee-data /usr/src/* \
  /usr/lib/python3/dist-packages/botocore /usr/lib/python3/dist-packages/twisted \
  /usr/lib/python3/dist-packages/sos /usr/lib/python3/dist-packages/pygments \
  /var/lib/snapd /snap /var/cache/*
apt-get clean
# The size the proxy has to accept, so a build that grows past it says so here
# and not at the upload.
ROOT_USED=$(du -x -s --block-size=1 / | cut -f1)
printf 'root used    %s bytes\n' "$ROOT_USED" | tee -a "$GUEST_CHECKS"

# --- cleanup: the image must carry no identity, no history, no free-space map -
log "cleanup"
systemctl stop docker.service docker.socket 2>/dev/null || true
rm -rf /var/lib/docker/* /var/lib/containerd/* 2>/dev/null || true
apt-get clean
rm -rf /var/lib/apt/lists/* /tmp/* /var/tmp/* "$STATE/apt-update.txt"
# The npm cache the npm check below fills is build state, not image content. No
# executor runs in this build -- they are not in the image -- so there is no
# executor home or configuration to remove; guest/no-secrets.sh checks that
# anyway, in the build VM and in a booted image.
rm -rf /root/.npm
journalctl --rotate --vacuum-time=1s >/dev/null 2>&1 || true
rm -rf /var/log/journal/* /var/log/*.log /var/log/apt/* /var/log/nginx/* 2>/dev/null || true
rm -f /root/.bash_history /root/.wget-hsts /home/*/.bash_history /home/*/.wget-hsts /home/*/.npmrc /home/*/.config/pip/pip.conf
# No SSH host keys and no authorized keys: the target boot generates its own.
rm -f /etc/ssh/ssh_host_*
rm -f /home/*/.ssh/authorized_keys /home/*/.ssh/id_* /root/.ssh/authorized_keys /root/.ssh/id_*
# The builder account cannot log in with a password.
passwd -l builder >/dev/null 2>&1 || true
# cloud-init must treat the next boot as the first boot of a new machine.
cloud-init clean --logs --machine-id
rm -rf /var/lib/cloud/instances/* /var/lib/cloud/instance
truncate -s 0 /etc/machine-id
rm -f /var/lib/dbus/machine-id

# Zero the free space so that the published qcow2 compresses well.
dd if=/dev/zero of=/zero.fill bs=1M status=none || true
sync
rm -f /zero.fill
sync

printf 'DONE %s %s %s\n' "$BOX_VERSION" "$BOX_ARCH" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > "$STATE/state"
log "provisioning finished"
