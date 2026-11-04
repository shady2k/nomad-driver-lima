#!/bin/sh
# boxd-install.sh: put the boxd daemon and the boxctl client into a box image.
#
# This is the operator's tool. An image is the operator's own -- built from the
# recipe in images/, or written by hand against docs/guest-image.md -- and this
# script is how boxd gets into it: into an image being built, or into a guest that
# is already running. It copies one binary to
# /usr/local/bin/boxd, links /usr/local/bin/boxctl to it (the binary dispatches
# on argv[0]), installs /etc/systemd/system/boxd.service, creates the state and
# runtime directories, and either starts the service (when this runs inside a
# booted guest) or enables it for the next boot (when it runs in an image build
# chroot).
#
# The release ships it next to boxd_linux_amd64 and boxd_linux_arm64;
# docs/guest-image.md says what the rest of an image has to provide.
#
# This is the layout for an image that carries boxd itself. An image built for a
# platform bundle carries none: its boxd.service runs /opt/box/bin/boxd, which
# the node mounts, and `boxd -print-unit` prints that unit instead.
#
# usage-begin
# usage: guest/boxd-install.sh --from /path/to/boxd [--prefix /usr/local/bin]
#                              [--no-start] [--dry-run]
#
# The binary is looked up in this order: --from, $BOXD_BIN, boxd_linux_$ARCH next
# to this script, boxd-linux-$ARCH next to it, ./dist/boxd_linux_$ARCH, then
# `boxd` on PATH.
# usage-end
set -eu

PREFIX=/usr/local/bin
BIN=
START=1
DRY_RUN=0
HEALTH_TIMEOUT=20
# Image builds and tests set DESTDIR to install into a staging root.
DESTDIR=${DESTDIR:-}

usage() {
	sed -n '/^# usage-begin/,/^# usage-end/p' "$0" | sed -e '1d;$d' -e 's/^# \{0,1\}//'
}

log() { printf 'boxd-install: %s\n' "$*" >&2; }
die() { log "error: $*"; exit 1; }

while [ $# -gt 0 ]; do
	case "$1" in
	--from)
		[ $# -ge 2 ] || die "--from needs a path"
		BIN=$2
		shift 2
		;;
	--prefix)
		[ $# -ge 2 ] || die "--prefix needs a path"
		PREFIX=$2
		shift 2
		;;
	--no-start)
		START=0
		shift
		;;
	--dry-run)
		DRY_RUN=1
		shift
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		usage >&2
		die "unknown argument: $1"
		;;
	esac
done

[ "$(id -u)" = 0 ] || [ "$DRY_RUN" = 1 ] || [ -n "$DESTDIR" ] || die "run as root, pass --dry-run, or set DESTDIR"

arch=$(uname -m 2>/dev/null || echo unknown)
case "$arch" in
x86_64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
esac

if [ -z "$BIN" ]; then
	here=$(dirname "$0")
	for cand in "${BOXD_BIN:-}" "$here/boxd_linux_$arch" "$here/boxd-linux-$arch" \
		"$here/../dist/boxd_linux_$arch" "$here/boxd"; do
		[ -n "$cand" ] || continue
		if [ -x "$cand" ]; then
			BIN=$cand
			break
		fi
	done
fi
if [ -z "$BIN" ]; then
	BIN=$(command -v boxd 2>/dev/null || true)
fi
[ -n "$BIN" ] || die "no boxd binary found (pass --from /path/to/boxd or set BOXD_BIN)"
[ -x "$BIN" ] || [ "$DRY_RUN" = 1 ] || die "$BIN is not executable"

log "binary:  $BIN"
log "prefix:  $PREFIX"
log "service: /etc/systemd/system/boxd.service"

if [ "$DRY_RUN" = 1 ]; then
	log "dry run: nothing changed"
	exit 0
fi

install -d -m 0755 "$DESTDIR$PREFIX" "$DESTDIR/etc/systemd/system" "$DESTDIR/var/lib/boxd" "$DESTDIR/run/boxd"
install -m 0755 "$BIN" "$DESTDIR$PREFIX/boxd"
ln -sf boxd "$DESTDIR$PREFIX/boxctl"

# The unit is embedded in the binary, so the image and the daemon never drift.
# -print-unit-installed is the unit for a boxd at $PREFIX: the same binary also
# carries the platform-bundle unit, which runs /opt/box/bin/boxd.
"$DESTDIR$PREFIX/boxd" -print-unit-installed >"$DESTDIR/etc/systemd/system/boxd.service"
chmod 0644 "$DESTDIR/etc/systemd/system/boxd.service"

if [ -z "$DESTDIR" ] && [ -d /run/systemd/system ]; then
	systemctl daemon-reload
	systemctl enable boxd.service >/dev/null
	if [ "$START" = 1 ]; then
		systemctl restart boxd.service
		i=0
		while [ $i -lt "$HEALTH_TIMEOUT" ]; do
			if [ -S /run/boxd/boxd.sock ] && "$PREFIX/boxctl" info >/dev/null 2>&1; then
				log "boxd is up: $("$PREFIX/boxctl" info)"
				exit 0
			fi
			i=$((i + 1))
			sleep 1
		done
		journalctl -u boxd.service -n 20 --no-pager >&2 || true
		die "boxd did not answer on /run/boxd/boxd.sock within ${HEALTH_TIMEOUT}s"
	fi
	log "installed; boxd.service enabled but not restarted (--no-start)"
	exit 0
fi

# Image build (no systemd running): enable the unit for the first boot.
install -d -m 0755 "$DESTDIR/etc/systemd/system/multi-user.target.wants"
ln -sf ../boxd.service "$DESTDIR/etc/systemd/system/multi-user.target.wants/boxd.service"
log "installed under ${DESTDIR:-/}; boxd.service starts on the next boot"
