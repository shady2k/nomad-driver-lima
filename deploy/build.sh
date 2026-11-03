#!/usr/bin/env bash
#
# Build the box release binaries for the deploy kit.
#
#   ./deploy/build.sh                  # every target, version from git describe
#   ./deploy/build.sh --linux-only     # skip the darwin (Mac node) artifacts
#   ./deploy/build.sh --version 1.2.3  # override the version
#
# The result is one directory:
#
#   dist/<version>/box-driver_linux_amd64
#   dist/<version>/box-volume_linux_amd64
#   dist/<version>/box_linux_amd64
#   dist/<version>/boxd_linux_amd64
#   dist/<version>/boxd_linux_arm64
#   dist/<version>/boxd-install.sh
#   dist/<version>/box-driver_darwin_arm64
#   dist/<version>/box-volume_darwin_arm64
#   dist/<version>/box_darwin_arm64
#   dist/<version>/SHA256SUMS
#
# The Ansible roles install box-driver as <plugin_dir>/box (Nomad matches an
# external plugin to its `plugin` block by executable name) and box-volume as
# <host_volume_plugin_dir>/box-volume, so the artifact names here are not the
# installed names. See deploy/README.md.
#
# boxd is not a node binary: it runs inside a box, so it is built for the guest's
# architecture (linux/amd64 and linux/arm64). boxd-install.sh is the operator's
# tool that puts it into an image of their own, next to the binary of the image's
# architecture; docs/guest-image.md is what the rest of that image needs.
set -euo pipefail

readonly PROG=${0##*/}

usage() {
	cat <<EOF
usage: $PROG [--version V] [--out DIR] [--linux-only] [-h]

  --version V    release version (default: git describe --tags --always --dirty)
  --out DIR      output directory (default: dist/<version> at the repo root)
  --linux-only   build only linux/amd64 (the node the owner deploys first)
  -h, --help     this text

Exit status: 0 built and checksummed, 1 build failed, 2 usage.
EOF
}

die() {
	printf '%s: %s\n' "$PROG" "$*" >&2
	exit 1
}

usage_error() {
	printf '%s: %s\n' "$PROG" "$*" >&2
	usage >&2
	exit 2
}

version=''
out_dir=''
linux_only=false

while (($# > 0)); do
	case $1 in
	--version)
		(($# >= 2)) || usage_error "--version needs a value"
		version=$2
		shift 2
		;;
	--version=*)
		version=${1#--version=}
		shift
		;;
	--out)
		(($# >= 2)) || usage_error "--out needs a value"
		out_dir=$2
		shift 2
		;;
	--out=*)
		out_dir=${1#--out=}
		shift
		;;
	--linux-only)
		linux_only=true
		shift
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		usage_error "unknown argument: $1"
		;;
	esac
done

repo_root=$(cd -- "$(dirname -- "$0")/.." && pwd)
cd -- "$repo_root" || die "cannot enter $repo_root"

command -v go >/dev/null 2>&1 || die "go is not in PATH; install the Go toolchain the module needs (see go.mod)"

# The version lands in the binaries and in the directory name, so it must be
# a plain token: git describe can add -dirty, and a branch name can hold a slash.
sanitize_version() {
	local v=$1
	printf '%s' "$v" | tr -c 'A-Za-z0-9._+-' '_' | sed -e 's/^_*//' -e 's/_*$//'
}

if [[ -z $version ]]; then
	if raw=$(git describe --tags --always --dirty 2>/dev/null) && [[ -n $raw ]]; then
		version=$raw
	else
		version=0.0.0-unknown
	fi
fi
version=$(sanitize_version "$version")
[[ -n $version ]] || die "the version is empty after sanitising"

# Nomad parses the version a plugin reports as semver and refuses to load a plugin
# whose version is not ("failed to parse plugin ... malformed version"). A vX.Y.Z tag
# gives a semver version through git describe; a tag with a prefix
# (linux-prototype-0.1.29) does not.
if [[ ! $version =~ ^v?[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$ ]]; then
	printf '%s: warning: %s is not a semver version.\n' "$PROG" "$version" >&2
	printf '%s: Nomad refuses to load a plugin that reports it. Pass --version 0.1.0 or\n' "$PROG" >&2
	printf '%s: tag the release vX.Y.Z.\n' "$PROG" >&2
fi

if [[ -z $out_dir ]]; then
	out_dir=$repo_root/dist/$version
elif [[ $out_dir != /* ]]; then
	out_dir=$repo_root/$out_dir
fi

mkdir -p -- "$out_dir" || die "cannot create $out_dir"

ldflags="-s -w -X github.com/shady2k/nomad-driver-lima/internal/version.Version=$version"

# target -> GOOS GOARCH, one line each; the darwin artifacts are for the Mac
# node and for an operator laptop that runs the CLI. These are the node binaries.
targets=(
	'linux amd64'
)
if [[ $linux_only == false ]]; then
	targets+=('linux arm64' 'darwin arm64')
fi

# The guest binary. boxd is never installed on a node: a box runs the boxd of
# its own architecture, so a release carries both guest architectures. --linux-only
# (a build of the node the owner deploys first) builds the host's own guest
# architecture.
guest_targets=(
	'linux amd64'
)
if [[ $linux_only == false ]]; then
	guest_targets+=('linux arm64')
fi

# artifact name for a command and a target
artifact() {
	local cmd=$1 os_name=$2 arch=$3
	case $cmd in
	box-driver) printf 'box-driver_%s_%s' "$os_name" "$arch" ;;
	box-volume) printf 'box-volume_%s_%s' "$os_name" "$arch" ;;
	box) printf 'box_%s_%s' "$os_name" "$arch" ;;
	boxd) printf 'boxd_%s_%s' "$os_name" "$arch" ;;
	*) die "unknown command $cmd" ;;
	esac
}

# build_one CMD GOOS GOARCH -- build one binary into out_dir.
build_one() {
	local cmd=$1 gos=$2 goarch=$3 name tmp
	name=$(artifact "$cmd" "$gos" "$goarch")
	printf 'building %s (%s/%s)\n' "$name" "$gos" "$goarch"
	# Build into a temporary name and move it, so a failed build never leaves a
	# half-written binary that an install could pick up.
	tmp=$out_dir/.$name.tmp
	rm -f -- "$tmp"
	if ! CGO_ENABLED=0 GOOS=$gos GOARCH=$goarch GOFLAGS='' \
		go build -trimpath -buildvcs=false -ldflags "$ldflags" -o "$tmp" "./cmd/$cmd"; then
		rm -f -- "$tmp"
		die "go build failed for $name"
	fi
	# No "--": BSD chmod and install (macOS) take it for a file name.
	chmod 0755 "$tmp"
	mv -f -- "$tmp" "$out_dir/$name"
	built+=("$name")
}

built=()
for target in "${targets[@]}"; do
	read -r gos goarch <<<"$target"
	for cmd in box-driver box-volume box; do
		build_one "$cmd" "$gos" "$goarch"
	done
done
for target in "${guest_targets[@]}"; do
	read -r gos goarch <<<"$target"
	build_one boxd "$gos" "$goarch"
done

# The operator's installer goes next to the guest binaries it looks for
# (boxd_linux_$arch), so an image build needs nothing else from this repository.
printf 'including %s\n' boxd-install.sh
install -m 0755 "$repo_root/guest/boxd-install.sh" "$out_dir/boxd-install.sh"
built+=("boxd-install.sh")

# SHA256SUMS covers exactly the artifacts in this directory, in name order.
(
	cd -- "$out_dir" || exit 1
	rm -f -- SHA256SUMS
	# macOS has shasum, not always sha256sum; both write "<digest>  <name>".
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum -- "${built[@]}" >SHA256SUMS
	else
		shasum -a 256 -- "${built[@]}" >SHA256SUMS
	fi
) || die "cannot write $out_dir/SHA256SUMS"

printf '\nbuilt version %s\n' "$version"
printf '  %s\n' "$out_dir"
sed 's/^/  /' "$out_dir/SHA256SUMS"
printf '\ninstall with deploy/ansible/site.yml: the nodes (box_nodes, box_nodes_mac) and\n'
printf 'the coordinator (box_coordinators) are one run; see deploy/README.md.\n'
