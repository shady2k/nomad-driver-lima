#!/usr/bin/env bash
#
# The checks the deploy kit runs on itself.
#
#   ./deploy/ansible/tests/run-tests.sh
#
# No VM, no root, no service manager, and nothing outside the repository: the roles
# are applied into throwaway prefixes under the worktree (.test/, gitignored), and
# the Nomad agents the last steps start are non-root `-dev` agents on their own
# ports.
#
# One read-only probe of box_node's preflight (`test -r /dev/kvm` and
# `limactl --version`) runs as the runtime user, which Ansible does with sudo: the
# user that runs these checks needs passwordless sudo. Nothing else escalates.
#
# What it proves, in order:
#
#   1. ansible-lint is clean at the production profile
#   2. the four playbooks parse (site, acl, rollback, uninstall), and the example
#      inventory does too
#   2b. the Claude Code plugin: the skill lives in claude-plugin/, the marketplace
#      at the repository root points at it, the plugin carries nothing else, and
#      `claude plugin validate` accepts both (when the CLI is here)
#   3. two releases build (deploy/build.sh) and their versions are semver - Nomad
#      refuses to load a plugin whose version is not
#   3b. the release download: a local HTTP server serves the built dist directory,
#      and the node roles install from box_release_base_url - the artifacts reach
#      the controller cache and the node, a second run with the server gone is
#      changed=0, and --check on an empty cache fetches into the cache and touches
#      no host; 3c: an artifact whose bytes are not the ones SHA256SUMS names, and
#      a release without the artifacts this node installs, are both refused and
#      install nothing
#   4. --check against localhost changes nothing on disk
#   5. a real apply into a prefix: the release layout and the live symlink, the
#      rendered JSON accepted by `nomad config validate`, the unit (KillMode,
#      ExecStart, XDG_RUNTIME_DIR), the secrets file 0600, /etc/box/volume.json
#      with exactly the three keys the volume plugin reads, the launchd plist
#      (valid XML, AbandonProcessGroup), and a second run reporting changed=0
#   5c. the coordinator: the CLI as a release and a symlink, the skill for the
#      Prime Agent, the Claude Code plugin (or the copied skill), and a second run
#      reporting changed=0
#   6. rollback: two releases on the node, a switch between them by one symlink,
#      and nothing else touched - the configuration, the Lima home, the volumes
#      and the client's data directory are the same bytes afterwards; a release
#      that is not on the node is refused
#   7. the ACL play against an agent with ACLs: the operator policy and a
#      job-attached workload policy land, and a second run changes nothing
#   8. a real non-root Nomad dev agent started on the rendered configuration
#      directory (ports 54646-54658, data under the prefix): the box driver is
#      healthy and the box-volume host volume plugin is fingerprinted - the
#      plugins are loaded through the `current` symlink
#   9. uninstall: the release, the settings file and the configuration go, the
#      Lima home and the client's data directory stay
#
# Exit status: 0 every check passed, 1 something failed, 2 a tool is missing.
set -uo pipefail

readonly PROG=${0##*/}
readonly HTTP_PORT=54646
readonly ACL_PORT=54656
# The "release host" of the download checks: two local HTTP servers over a directory
# of release artifacts, one serving them as published and one serving broken ones.
readonly RELEASE_PORT=54660
readonly RELEASE_BAD_PORT=54661
readonly NODE_NAME=box-test-node
readonly V1=0.1.0-test
readonly V2=0.1.1-test

REPO=$(cd -- "$(dirname -- "$0")/../../.." && pwd)
ANSIBLE_DIR=$REPO/deploy/ansible
TESTS_DIR=$ANSIBLE_DIR/tests
TEST_ROOT=${NOMAD_TEST_ROOT:-$REPO/.test}
# A run that already has the releases - a CI build job, or a tree whose own build
# is in flight - points NOMAD_TEST_DIST_ROOT at them and skips the build below.
DIST_ROOT=${NOMAD_TEST_DIST_ROOT:-$REPO/dist}

passed=0
failed=0

ok() {
	printf 'PASS  %s\n' "$*"
	passed=$((passed + 1))
}

bad() {
	printf 'FAIL  %s\n' "$*"
	failed=$((failed + 1))
}

# check <description> <command...>: PASS when the command succeeds.
check() {
	local what=$1
	shift
	if "$@" >/dev/null 2>&1; then ok "$what"; else bad "$what"; fi
}

# check_grep <description> <pattern> <file>
check_grep() {
	local what=$1 pattern=$2 file=$3
	if grep -qE -- "$pattern" "$file"; then
		ok "$what"
	else
		bad "$what (no match for $pattern in ${file##*/})"
	fi
}

die() {
	printf '%s: %s\n' "$PROG" "$*" >&2
	exit 2
}

# ------------------------------------------------------------------ the tools
need() {
	command -v "$1" >/dev/null 2>&1 || die "$1 is not in PATH. Put it there first:
  nomad, limactl, qemu-system-x86_64 (the roles check them), ansible-playbook,
  ansible-lint and go (the release build)."
}

for tool in ansible-playbook ansible-lint go nomad limactl qemu-system-x86_64 python3; do
	need "$tool"
done

# The coordinator gives the skill to Claude Code through the plugin of this
# repository. Without a claude CLI on this host the checks install it as a file
# instead, which is the other way the role supports.
claude_mode=plugin
if ! command -v claude >/dev/null 2>&1; then
	claude_mode=copy
	printf '   no claude CLI in PATH: the coordinator checks install the skill as a file\n'
fi

NOMAD=$(command -v nomad)
LIMACTL=$(command -v limactl)
QEMU=$(command -v qemu-system-x86_64)
RUN_USER=$(id -un)
RUN_GROUP=$(id -gn)

printf '== the deploy kit checks (%s)\n' "$(nomad version | head -1)"
printf '   nomad %s\n   limactl %s\n   qemu %s\n   prefix %s\n\n' \
	"$NOMAD" "$LIMACTL" "$QEMU" "$TEST_ROOT"

rm -rf -- "$TEST_ROOT"
mkdir -p -- "$TEST_ROOT"

# The role checks share one values file; these depend on who runs the checks (the
# prefix belongs to that user, not to root).
common_args=(
	-i "$TESTS_DIR/inventory.test.ini"
	-e "@$TESTS_DIR/vars-test.yml"
	-e "box_plugin_owner=$RUN_USER"
	-e "box_plugin_group=$RUN_GROUP"
	-e "nomad_file_owner=$RUN_USER"
	-e "nomad_file_group=$RUN_GROUP"
	-e "box_runtime_user=$RUN_USER"
	-e "box_coordinator_group=$RUN_GROUP"
	-e "box_claude_skill_mode=$claude_mode"
)

# ansible.cfg and the relative roles_path in it are found from the working
# directory, so every run starts in deploy/ansible.
play() {
	(cd -- "$ANSIBLE_DIR" && ansible-playbook "${common_args[@]}" "$@")
}

# acl.yml is once per cluster, on localhost, and takes no other variables.
play_acl() {
	(cd -- "$ANSIBLE_DIR" && ansible-playbook -i "$TESTS_DIR/inventory.test.ini" acl.yml "$@")
}

# claude_plugin <args...>: the Claude Code CLI as the coordinator sees it - its
# own configuration directory inside the prefix, so no check touches ~/.claude.
claude_plugin() {
	CLAUDE_CONFIG_DIR=$C/home/.claude-config claude "$@"
}

export NOMAD_TEST_LIMACTL=$LIMACTL
export NOMAD_TEST_QEMU=$QEMU
export NOMAD_TEST_ZIP=${NOMAD_TEST_ZIP:-}
export NOMAD_TEST_ZIP_SHA256=${NOMAD_TEST_ZIP_SHA256:-}

# prepare_prefix <dir>: a throwaway node prefix with the nomad binary in it, so
# the roles validate the configuration they rendered without downloading one.
prepare_prefix() {
	rm -rf -- "$1" 2>/dev/null
	# A throwaway prefix can hold files of an earlier run that escalated (a play
	# with become: true writes as root): rm -rf as the operator cannot remove
	# them, and every later task would fail on the ownership instead.
	if [[ -e $1 ]]; then
		die "cannot remove $1: it holds files of another user (an earlier run as root). Remove it with: sudo rm -rf $1"
	fi
	mkdir -p -- "$1/usr/local/bin"
	install -m 0755 -- "$NOMAD" "$1/usr/local/bin/nomad"
}

# use_release <version>: the release the next run installs.
use_release() {
	export BOX_TEST_VERSION=$1
	export BOX_TEST_DIST=$DIST_ROOT/$1
}

# start_store <directory> <port>: an HTTP server serving one directory, answering
# before this returns; stop_store ends it.
start_store() {
	local directory=$1 port=$2
	if ss -lnt 2>/dev/null | grep -qE ":($port) "; then
		die "port $port is in use; stop what holds it (the checks use their own ports)"
	fi
	python3 -m http.server --bind 127.0.0.1 --directory "$directory" "$port" \
		>"$TEST_ROOT/http-$port.log" 2>&1 &
	store_pid=$!
	for _ in $(seq 1 50); do
		kill -0 "$store_pid" 2>/dev/null || break
		if python3 -c 'import sys, urllib.request
urllib.request.urlopen(sys.argv[1], timeout=2).read()' "http://127.0.0.1:$port/" 2>/dev/null; then
			return 0
		fi
		sleep 0.2
	done
	die "the release host on port $port did not start (tests/.test/http-$port.log)"
}

# stop_store: end the server start_store began, and wait for it.
stop_store() {
	[[ -n $store_pid ]] || return 0
	kill -TERM "$store_pid" 2>/dev/null
	for _ in $(seq 1 50); do
		kill -0 "$store_pid" 2>/dev/null || break
		sleep 0.2
	done
	kill -KILL "$store_pid" 2>/dev/null
	store_pid=''
}

# ------------------------------------------------------------------- 1. lint
printf -- '-- 1. ansible-lint\n'
if (cd -- "$ANSIBLE_DIR" && ansible-lint >"$TEST_ROOT/lint.log" 2>&1); then
	ok 'ansible-lint is clean'
else
	bad 'ansible-lint reported findings (tests/.test/lint.log)'
	tail -20 "$TEST_ROOT/lint.log"
fi

# ---------------------------------------------------------- 2. the playbooks
printf -- '-- 2. the playbooks parse\n'
for playbook in site.yml acl.yml rollback.yml uninstall.yml; do
	if (cd -- "$ANSIBLE_DIR" && ansible-playbook -i "$TESTS_DIR/inventory.test.ini" "$playbook" --syntax-check >"$TEST_ROOT/syntax-$playbook.log" 2>&1); then
		ok "$playbook --syntax-check"
	else
		bad "$playbook --syntax-check"
		tail -20 "$TEST_ROOT/syntax-$playbook.log"
	fi
done

# The example inventory is what an operator copies; it has to parse, and its
# group_vars have to resolve.
if (cd -- "$ANSIBLE_DIR" && ansible-inventory -i inventory.example/hosts.ini --list >"$TEST_ROOT/inventory.log" 2>&1); then
	ok 'inventory.example parses'
else
	bad 'inventory.example does not parse (tests/.test/inventory.log)'
	tail -10 "$TEST_ROOT/inventory.log"
fi

# ------------------------------------------- 2b. the Claude Code plugin
# One source of truth: the skill lives in the plugin, the marketplace at the
# repository root points at it, and the plugin carries the skill and nothing else.
printf -- '-- 2b. the Claude Code plugin\n'
if python3 - "$REPO" <<'PY'
import json, os, sys

repo = sys.argv[1]
plugin_dir = os.path.join(repo, "claude-plugin")
with open(os.path.join(plugin_dir, ".claude-plugin", "plugin.json"), encoding="utf-8") as f:
    plugin = json.load(f)
with open(os.path.join(repo, ".claude-plugin", "marketplace.json"), encoding="utf-8") as f:
    market = json.load(f)

assert plugin["name"] == "box", plugin["name"]
assert plugin["license"] == "Apache-2.0", plugin.get("license")
assert plugin["repository"].startswith("https://github.com/"), plugin.get("repository")
version = plugin["version"]
assert version.count(".") == 2 and version[0].isdigit(), version
assert market["plugins"], market
assert any(p["name"] == plugin["name"] and p["source"] == "./claude-plugin" for p in market["plugins"]), market["plugins"]
# The plugin is the skill only: no hook, no MCP server, no bundled binary.
assert os.path.isfile(os.path.join(plugin_dir, "skills", "box", "SKILL.md")), "no SKILL.md"
assert sorted(os.listdir(plugin_dir)) == [".claude-plugin", "skills"], sorted(os.listdir(plugin_dir))
PY
then
	ok 'the plugin manifest, the marketplace and the skill agree'
else
	bad 'the plugin manifest, the marketplace or the skill is not what this kit describes'
fi
if command -v claude >/dev/null 2>&1; then
	if claude plugin validate "$REPO/claude-plugin" >"$TEST_ROOT/plugin-validate.log" 2>&1; then
		ok 'claude plugin validate accepts the plugin'
	else
		bad 'claude plugin validate rejected the plugin (tests/.test/plugin-validate.log)'
		tail -20 "$TEST_ROOT/plugin-validate.log"
	fi
	if claude plugin validate "$REPO/.claude-plugin/marketplace.json" >"$TEST_ROOT/marketplace-validate.log" 2>&1; then
		ok 'claude plugin validate accepts the marketplace'
	else
		bad 'claude plugin validate rejected the marketplace (tests/.test/marketplace-validate.log)'
		tail -20 "$TEST_ROOT/marketplace-validate.log"
	fi
else
	printf 'SKIP  claude plugin validate (no claude CLI on this host)\n'
fi

# ------------------------------------------------- 3. the releases under test
printf -- '-- 3. the two releases\n'
for version in "$V1" "$V2"; do
	if [[ -n ${NOMAD_TEST_DIST_ROOT:-} ]]; then
		check "the release $version is in $DIST_ROOT" test -f "$DIST_ROOT/$version/SHA256SUMS"
	elif "$REPO/deploy/build.sh" --linux-only --version "$version" >"$TEST_ROOT/build-$version.log" 2>&1; then
		ok "deploy/build.sh built $version"
	else
		bad "deploy/build.sh failed for $version (tests/.test/build-$version.log)"
		tail -20 "$TEST_ROOT/build-$version.log"
	fi
	# Nomad parses the version a plugin reports as semver and refuses the plugin
	# otherwise, so the versions this run installs must be semver.
	if [[ $version =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
		ok "the release version $version is semver"
	else
		bad "the release version $version is not semver: Nomad will not load the plugin"
	fi
done

# ------------------------------------------------------- 3b. a downloaded release
# The default way to deploy: the roles fetch the published release once, on the
# machine that runs Ansible, check every artifact against the release's SHA256SUMS,
# and copy it to the host. The "release host" here is a local HTTP server over the
# dist directory the build above wrote.
printf -- '-- 3b. the release download\n'
store_pid=''

start_store "$DIST_ROOT" "$RELEASE_PORT"
export NOMAD_TEST_PREFIX=$TEST_ROOT/download
prepare_prefix "$NOMAD_TEST_PREFIX"
D=$NOMAD_TEST_PREFIX
use_release "$V2"
# The cache is inside the prefix (the machine that runs Ansible is the "node" of
# these checks), so a fresh prefix is a fresh cache.
release_url="http://127.0.0.1:$RELEASE_PORT/$V2"
download_args=(
	-e box_release_source=download
	-e "box_release_base_url=$release_url"
)
if play "$TESTS_DIR/play-node.yml" "${download_args[@]}" >"$TEST_ROOT/download.log" 2>&1; then
	ok 'the node roles install a release downloaded from box_release_base_url'
else
	bad 'the node roles failed with box_release_source: download (tests/.test/download.log)'
	tail -30 "$TEST_ROOT/download.log"
fi
check "the downloaded release is installed" test -x "$D/opt/box/releases/$V2/plugins/box"
check 'the installed driver is the artifact the release carries' \
	cmp -s "$D/opt/box/releases/$V2/plugins/box" "$DIST_ROOT/$V2/box-driver_linux_amd64"
check 'the installed volume plugin is the artifact the release carries' \
	cmp -s "$D/opt/box/releases/$V2/host_volume_plugins/box-volume" "$DIST_ROOT/$V2/box-volume_linux_amd64"
check 'the cache holds the artifacts of the release' \
	test -f "$D/var/cache/box-releases/$V2/box-driver_linux_amd64"
check 'the cache holds the checksums of the release' \
	test -f "$D/var/cache/box-releases/$V2/SHA256SUMS"

# Downloaded once: with the release host gone, the same play reads the cache and
# changes nothing. Nothing but the cache can answer it.
stop_store
if play "$TESTS_DIR/play-node.yml" "${download_args[@]}" >"$TEST_ROOT/download2.log" 2>&1; then
	if grep -qE 'changed=0 .*failed=0' "$TEST_ROOT/download2.log"; then
		ok 'a second run installs from the cache with the release host gone, changed=0'
	else
		bad 'a second downloaded install changed something'
		grep -E 'changed=[1-9]' "$TEST_ROOT/download2.log" | head -5
	fi
else
	bad 'the second downloaded install failed (tests/.test/download2.log)'
	tail -20 "$TEST_ROOT/download2.log"
fi

# --check on a machine whose cache is empty: the release is fetched so that every
# host step is checked against the real bytes, and the node itself is untouched.
export NOMAD_TEST_PREFIX=$TEST_ROOT/download-check
prepare_prefix "$NOMAD_TEST_PREFIX"
DC=$NOMAD_TEST_PREFIX
start_store "$DIST_ROOT" "$RELEASE_PORT"
if play "$TESTS_DIR/play-node.yml" "${download_args[@]}" --check >"$TEST_ROOT/download-check.log" 2>&1; then
	ok '--check runs against a release that is not in the cache yet'
else
	bad '--check failed with an empty release cache (tests/.test/download-check.log)'
	tail -30 "$TEST_ROOT/download-check.log"
fi
check '--check fetched the release into the cache on the controller' \
	test -f "$DC/var/cache/box-releases/$V2/SHA256SUMS"
check '--check installed nothing on the node' test ! -e "$DC/opt/box"
stop_store

# ---------------------------------------------------------------- 3c. a bad release
# A release host that serves bytes another than SHA256SUMS names must stop the
# play before anything reaches the node. Two cases: an artifact with the wrong
# bytes, and a release that does not carry the artifact this node installs.
printf -- '-- 3c. a release that does not match its checksums\n'
mkdir -p -- "$TEST_ROOT/store/bad/$V2" "$TEST_ROOT/store/incomplete/$V2"
cp -- "$DIST_ROOT/$V2/SHA256SUMS" "$TEST_ROOT/store/bad/$V2/SHA256SUMS"
cp -- "$DIST_ROOT/$V2/box-volume_linux_amd64" "$TEST_ROOT/store/bad/$V2/box-volume_linux_amd64"
printf 'this is not the driver of %s\n' "$V2" >"$TEST_ROOT/store/bad/$V2/box-driver_linux_amd64"
cp -- "$DIST_ROOT/$V2/box-driver_linux_amd64" "$TEST_ROOT/store/incomplete/$V2/box-driver_linux_amd64"
grep -v 'box-volume_linux_amd64$' "$DIST_ROOT/$V2/SHA256SUMS" \
	>"$TEST_ROOT/store/incomplete/$V2/SHA256SUMS"

start_store "$TEST_ROOT/store" "$RELEASE_BAD_PORT"
export NOMAD_TEST_PREFIX=$TEST_ROOT/download-bad
prepare_prefix "$NOMAD_TEST_PREFIX"
DB=$NOMAD_TEST_PREFIX
if play "$TESTS_DIR/play-node.yml" \
	-e box_release_source=download \
	-e "box_release_base_url=http://127.0.0.1:$RELEASE_BAD_PORT/bad/$V2" \
	>"$TEST_ROOT/download-bad.log" 2>&1; then
	bad 'an artifact that does not match SHA256SUMS was installed'
else
	if grep -qiE 'checksum' "$TEST_ROOT/download-bad.log"; then
		ok 'an artifact whose bytes are not the ones SHA256SUMS names is refused, and the failure says so'
	else
		bad 'the refused download does not mention the checksum'
		tail -20 "$TEST_ROOT/download-bad.log"
	fi
fi
check 'the refused release installed nothing on the node' test ! -e "$DB/opt/box"

export NOMAD_TEST_PREFIX=$TEST_ROOT/download-incomplete
prepare_prefix "$NOMAD_TEST_PREFIX"
DI=$NOMAD_TEST_PREFIX
if play "$TESTS_DIR/play-node.yml" \
	-e box_release_source=download \
	-e "box_release_base_url=http://127.0.0.1:$RELEASE_BAD_PORT/incomplete/$V2" \
	>"$TEST_ROOT/download-incomplete.log" 2>&1; then
	bad 'a release without the artifact this node installs was accepted'
else
	if grep -qE 'is not complete|has no entry' "$TEST_ROOT/download-incomplete.log"; then
		ok 'a release that does not carry the artifacts of this node is refused, and the failure names them'
	else
		bad 'the refusal does not say which artifact the release lacks'
		tail -20 "$TEST_ROOT/download-incomplete.log"
	fi
fi
check 'the incomplete release installed nothing on the node' test ! -e "$DI/opt/box"
stop_store

# ------------------------------------------------------------- 4. check mode
printf -- '-- 4. --check against localhost\n'
export NOMAD_TEST_PREFIX=$TEST_ROOT/check
prepare_prefix "$NOMAD_TEST_PREFIX"
use_release "$V1"
if play "$TESTS_DIR/play-node.yml" --check >"$TEST_ROOT/check.log" 2>&1; then
	ok 'the node roles run in check mode'
else
	bad 'the node roles failed in check mode (tests/.test/check.log)'
	tail -30 "$TEST_ROOT/check.log"
fi
if [[ -e $NOMAD_TEST_PREFIX/etc/nomad.d/10-base.json || -e $NOMAD_TEST_PREFIX/opt/box ]]; then
	bad '--check wrote the release or the configuration'
else
	ok '--check wrote nothing'
fi

# --------------------------------------------------------- 5. a real apply
printf -- '-- 5. a real apply into a prefix\n'
export NOMAD_TEST_PREFIX=$TEST_ROOT/node
prepare_prefix "$NOMAD_TEST_PREFIX"
P=$NOMAD_TEST_PREFIX
use_release "$V1"
if play "$TESTS_DIR/play-node.yml" >"$TEST_ROOT/apply.log" 2>&1; then
	ok 'the node roles apply into the prefix'
else
	bad 'the node roles failed (tests/.test/apply.log)'
	tail -30 "$TEST_ROOT/apply.log"
fi
check 'the base configuration is there' test -f "$P/etc/nomad.d/10-base.json"
check 'the box configuration is there' test -f "$P/etc/nomad.d/50-box.json"
check 'the unit is there' test -f "$P/etc/systemd/system/nomad.service"
check "release $V1 holds the box driver" test -x "$P/opt/box/releases/$V1/plugins/box"
check "release $V1 holds the volume plugin" test -x "$P/opt/box/releases/$V1/host_volume_plugins/box-volume"
check 'the live symlink points at that release' test "$P/opt/box/current" -ef "$P/opt/box/releases/$V1"
check 'the box driver is reachable through the live symlink' test -x "$P/opt/box/current/plugins/box"
check 'the volume plugin is reachable through the live symlink' test -x "$P/opt/box/current/host_volume_plugins/box-volume"
check_grep 'the unit carries KillMode=process' '^KillMode=process$' "$P/etc/systemd/system/nomad.service"
check_grep 'the unit loads the configuration directory' '^ExecStart=.*agent -config .*/etc/nomad.d$' "$P/etc/systemd/system/nomad.service"
check_grep 'the unit carries the runtime directory of the box user' '^Environment=XDG_RUNTIME_DIR=/run/user/[0-9]+$' "$P/etc/systemd/system/nomad.service"
check_grep 'nomad config validate accepted the configuration' 'Configuration is valid!' "$TEST_ROOT/apply.log"
check_grep 'the plugin block carries runtime_user' '"runtime_user": "'"$RUN_USER"'"' "$P/etc/nomad.d/50-box.json"
check_grep 'the plugin block carries the allowlist' '"driver.allowlist": "box"' "$P/etc/nomad.d/50-box.json"
check_grep 'the plugin block names the live plugin directory' '"plugin_dir": "'"$P"'/opt/box/current/plugins"' "$P/etc/nomad.d/50-box.json"
check_grep 'the plugin block names the live host volume plugin directory' '"host_volume_plugin_dir": "'"$P"'/opt/box/current/host_volume_plugins"' "$P/etc/nomad.d/50-box.json"
if [[ $(stat -c '%a' "$P/etc/nomad.d/nomad.env") == 600 ]]; then
	ok 'the secrets file is 0600'
else
	bad "the secrets file is $(stat -c '%a' "$P/etc/nomad.d/nomad.env"), not 0600"
fi

# The volume plugin takes no volume parameters: the node's three settings live in
# one file, and the plugin rejects an unknown key, so the file must hold exactly
# those three.
if python3 - "$P/etc/box/volume.json" "$LIMACTL" "$P/var/lib/box/lima" "$RUN_USER" <<'PY'
import json, sys

path, limactl, lima_home, user = sys.argv[1:]
with open(path, encoding="utf-8") as f:
    doc = json.load(f)
assert set(doc) == {"limactl", "lima_home", "runtime_user"}, sorted(doc)
assert doc["limactl"] == limactl, doc["limactl"]
assert doc["lima_home"] == lima_home, doc["lima_home"]
assert doc["runtime_user"] == user, doc["runtime_user"]
PY
then
	ok 'the box-volume settings file holds exactly limactl, lima_home and runtime_user'
else
	bad "the box-volume settings file is not the three settings the plugin reads ($P/etc/box/volume.json)"
fi

# The Mac's daemon: the same configuration, and a plist that launchd can read.
# plutil is not on Linux, so the plist is parsed as a property list with python.
if python3 - "$P/Library/LaunchDaemons/com.hashicorp.nomad.plist" "$P" "$RUN_USER" <<'PY'
import plistlib, sys

path, prefix, user = sys.argv[1:]
with open(path, "rb") as f:
    doc = plistlib.load(f)
assert doc["Label"] == "com.hashicorp.nomad", doc["Label"]
args = doc["ProgramArguments"]
assert args[1] == "agent" and args[2] == "-config", args
assert args[3] == prefix + "/etc/nomad.d", args[3]
assert doc["UserName"] == user, doc["UserName"]
assert doc["RunAtLoad"] is True, doc["RunAtLoad"]
assert doc["KeepAlive"] == {"SuccessfulExit": False}, doc["KeepAlive"]
assert doc["AbandonProcessGroup"] is True, doc["AbandonProcessGroup"]
assert doc["StandardOutPath"] and doc["StandardErrorPath"], "no log paths"
assert "EnvironmentVariables" in doc, "no environment"
PY
then
	ok 'the launchd plist is a valid property list with the daemon launchd needs'
else
	bad "the launchd plist is not the daemon this kit describes ($P/Library/LaunchDaemons/com.hashicorp.nomad.plist)"
fi

printf -- '-- 5b. a second run changes nothing\n'
if play "$TESTS_DIR/play-node.yml" >"$TEST_ROOT/apply2.log" 2>&1; then
	if grep -qE 'changed=0 .*failed=0' "$TEST_ROOT/apply2.log"; then
		ok 'the second run reports changed=0'
	else
		bad 'the second run changed something'
		grep -E 'changed=[1-9]' "$TEST_ROOT/apply2.log" | head -10
	fi
else
	bad 'the second run failed (tests/.test/apply2.log)'
	tail -20 "$TEST_ROOT/apply2.log"
fi

# The coordinator, into its own prefix: the CLI is a release and a symlink, and
# the rollback below switches it the same way.
printf -- '-- 5c. the coordinator role\n'
export NOMAD_TEST_PREFIX=$TEST_ROOT/coordinator
C=$NOMAD_TEST_PREFIX
mkdir -p -- "$C"
if play "$TESTS_DIR/play-coordinator.yml" >"$TEST_ROOT/coordinator.log" 2>&1; then
	ok 'the coordinator role installs the CLI'
else
	bad 'the coordinator role failed (tests/.test/coordinator.log)'
	tail -30 "$TEST_ROOT/coordinator.log"
fi
check "release $V1 holds the CLI" test -x "$C/opt/box/releases/$V1/bin/box"
check 'the CLI on PATH is the live release' test "$C/usr/local/bin/box" -ef "$C/opt/box/current/bin/box"
check 'the skill is installed for the Prime Agent' test -f "$C/home/.agents/skills/box/SKILL.md"
check 'the skill installed is the SKILL.md of the repository' \
	cmp -s "$C/home/.agents/skills/box/SKILL.md" "$REPO/claude-plugin/skills/box/SKILL.md"
if [[ $claude_mode == plugin ]]; then
	if claude_plugin plugin marketplace list --json 2>/dev/null | grep -q '"nomad-driver-lima"'; then
		ok 'Claude Code has the marketplace of this repository'
	else
		bad 'Claude Code does not have the marketplace of this repository'
	fi
	if claude_plugin plugin list --json 2>/dev/null | grep -q '"box@nomad-driver-lima"'; then
		ok 'Claude Code has the box plugin'
	else
		bad 'Claude Code does not have the box plugin'
	fi
	plugin_skill=''
	for candidate in "$C"/home/.claude-config/plugins/cache/nomad-driver-lima/box/*/skills/box/SKILL.md; do
		if [[ -f $candidate ]]; then
			plugin_skill=$candidate
			break
		fi
	done
	check 'the plugin carries the SKILL.md of the repository' \
		cmp -s "${plugin_skill:-/nonexistent}" "$REPO/claude-plugin/skills/box/SKILL.md"
	check 'the skill is not copied beside the plugin' test ! -e "$C/home/.claude/skills/box"
else
	check 'the skill is copied for Claude Code' test -f "$C/home/.claude/skills/box/SKILL.md"
fi
if [[ $(stat -c '%a' "$C/home/.config/box/env") == 600 ]]; then
	ok 'the environment file of the coordinator is 0600'
else
	bad "the environment file of the coordinator is $(stat -c '%a' "$C/home/.config/box/env" 2>/dev/null), not 0600"
fi
if play "$TESTS_DIR/play-coordinator.yml" >"$TEST_ROOT/coordinator2.log" 2>&1; then
	if grep -qE 'changed=0 .*failed=0' "$TEST_ROOT/coordinator2.log"; then
		ok 'a second coordinator run reports changed=0'
	else
		bad 'a second coordinator run changed something'
		grep -E 'changed=[1-9]' "$TEST_ROOT/coordinator2.log" | head -5
	fi
else
	bad 'the second coordinator run failed (tests/.test/coordinator2.log)'
	tail -20 "$TEST_ROOT/coordinator2.log"
fi

# The other way the role can give Claude Code the skill: a copy of the same file.
# The two ways are exclusive, so this run also takes the plugin off.
printf -- '-- 5d. the skill as a file instead of the plugin\n'
if play "$TESTS_DIR/play-coordinator.yml" -e box_claude_skill_mode=copy >"$TEST_ROOT/coordinator-copy.log" 2>&1; then
	ok 'the coordinator installs the skill as a file'
else
	bad 'the coordinator failed with box_claude_skill_mode=copy (tests/.test/coordinator-copy.log)'
	tail -30 "$TEST_ROOT/coordinator-copy.log"
fi
check 'the skill is copied for Claude Code' test -f "$C/home/.claude/skills/box/SKILL.md"
check 'the copied skill is the SKILL.md of the repository' \
	cmp -s "$C/home/.claude/skills/box/SKILL.md" "$REPO/claude-plugin/skills/box/SKILL.md"
if [[ $claude_mode == plugin ]]; then
	if claude_plugin plugin list --json 2>/dev/null | grep -q '"box@nomad-driver-lima"'; then
		bad 'the box plugin is still installed beside the copied skill'
	else
		ok 'the box plugin is gone once the skill is a file'
	fi
	if play "$TESTS_DIR/play-coordinator.yml" -e box_claude_skill_mode=copy >"$TEST_ROOT/coordinator-copy2.log" 2>&1; then
		if grep -qE 'changed=0 .*failed=0' "$TEST_ROOT/coordinator-copy2.log"; then
			ok 'a second copy-mode run reports changed=0'
		else
			bad 'a second copy-mode run changed something'
			grep -E 'changed=[1-9]' "$TEST_ROOT/coordinator-copy2.log" | head -5
		fi
	else
		bad 'the second copy-mode run failed (tests/.test/coordinator-copy2.log)'
		tail -20 "$TEST_ROOT/coordinator-copy2.log"
	fi
	# Back to the default: the plugin run below (release $V2) installs it again.
fi

# ------------------------------------------------------------ 6. rollback
printf -- '-- 6. a rollback between two installed releases\n'
export NOMAD_TEST_PREFIX=$TEST_ROOT/node
P=$NOMAD_TEST_PREFIX
config_sum=$(sha256sum "$P/etc/nomad.d/10-base.json" | cut -d' ' -f1)
unit_sum=$(sha256sum "$P/etc/systemd/system/nomad.service" | cut -d' ' -f1)
settings_sum=$(sha256sum "$P/etc/box/volume.json" | cut -d' ' -f1)
# The three places a rollback must never touch, marked so a loss is visible.
for marker in "$P/var/lib/box/lima/keep-me" "$P/var/lib/nomad-volumes/keep-me" "$P/var/lib/nomad/keep-me"; do
	mkdir -p -- "${marker%/*}"
	printf '%s\n' "$marker" >"$marker"
done

use_release "$V2"
if play "$TESTS_DIR/play-node.yml" >"$TEST_ROOT/apply-v2.log" 2>&1; then
	ok "release $V2 installs next to $V1"
else
	bad "release $V2 failed to install (tests/.test/apply-v2.log)"
	tail -20 "$TEST_ROOT/apply-v2.log"
fi
check "the live symlink now points at $V2" test "$P/opt/box/current" -ef "$P/opt/box/releases/$V2"
check "the $V1 release is still on the node" test -x "$P/opt/box/releases/$V1/plugins/box"

export NOMAD_TEST_PREFIX=$TEST_ROOT/coordinator
C=$NOMAD_TEST_PREFIX
if play "$TESTS_DIR/play-coordinator.yml" >"$TEST_ROOT/coordinator-v2.log" 2>&1; then
	ok "release $V2 of the CLI installs next to $V1"
else
	bad "release $V2 of the CLI failed to install (tests/.test/coordinator-v2.log)"
	tail -20 "$TEST_ROOT/coordinator-v2.log"
fi
check "the coordinator's live symlink points at $V2" test "$C/opt/box/current" -ef "$C/opt/box/releases/$V2"

use_release "$V1"
export NOMAD_TEST_PREFIX=$TEST_ROOT/node
P=$NOMAD_TEST_PREFIX
if play rollback.yml --limit node-01 -e "box_version=$V1" >"$TEST_ROOT/rollback.log" 2>&1; then
	ok 'rollback.yml switches the live release of the node'
else
	bad 'rollback.yml failed for the node (tests/.test/rollback.log)'
	tail -30 "$TEST_ROOT/rollback.log"
fi
export NOMAD_TEST_PREFIX=$TEST_ROOT/coordinator
C=$NOMAD_TEST_PREFIX
if play rollback.yml --limit coordinator-01 -e "box_version=$V1" >"$TEST_ROOT/rollback-cli.log" 2>&1; then
	ok 'rollback.yml switches the live release of the coordinator'
else
	bad 'rollback.yml failed for the coordinator (tests/.test/rollback-cli.log)'
	tail -30 "$TEST_ROOT/rollback-cli.log"
fi
check "the coordinator's live symlink points at $V1 again" test "$C/opt/box/current" -ef "$C/opt/box/releases/$V1"
check "the CLI on PATH is the $V1 artifact" \
	bash -c "cmp -s '$C/usr/local/bin/box' '$DIST_ROOT/$V1/box_linux_amd64'"
check "the $V2 release of the CLI is still on the coordinator" test -x "$C/opt/box/releases/$V2/bin/box"

export NOMAD_TEST_PREFIX=$TEST_ROOT/node
P=$NOMAD_TEST_PREFIX
check "the live symlink points at $V1 again" test "$P/opt/box/current" -ef "$P/opt/box/releases/$V1"
check_grep 'the rollback reports what it does not touch' 'untouched on purpose' "$TEST_ROOT/rollback.log"
if [[ $(sha256sum "$P/etc/nomad.d/10-base.json" | cut -d' ' -f1) == "$config_sum" ]]; then
	ok 'the client configuration is the same after the rollback'
else
	bad 'the rollback rewrote the client configuration'
fi
if [[ $(sha256sum "$P/etc/systemd/system/nomad.service" | cut -d' ' -f1) == "$unit_sum" ]]; then
	ok 'the unit is the same after the rollback'
else
	bad 'the rollback rewrote the unit'
fi
if [[ $(sha256sum "$P/etc/box/volume.json" | cut -d' ' -f1) == "$settings_sum" ]]; then
	ok 'the box-volume settings are the same after the rollback'
else
	bad 'the rollback rewrote the box-volume settings'
fi
for marker in "$P/var/lib/box/lima/keep-me" "$P/var/lib/nomad-volumes/keep-me" "$P/var/lib/nomad/keep-me"; do
	check "the rollback kept ${marker#$P}" test -f "$marker"
done
# The plugin behind the path is the one of the release now live.
check "the live driver is the $V1 artifact" \
	bash -c "cmp -s '$P/opt/box/current/plugins/box' '$DIST_ROOT/$V1/box-driver_linux_amd64'"
check "the live volume plugin is the $V1 artifact" \
	bash -c "cmp -s '$P/opt/box/current/host_volume_plugins/box-volume' '$DIST_ROOT/$V1/box-volume_linux_amd64'"

if play rollback.yml --limit node-01 -e "box_version=9.9.9" >"$TEST_ROOT/rollback-missing.log" 2>&1; then
	bad 'a rollback to a release that is not on the node was accepted'
else
	if grep -qE 'is not on this node' "$TEST_ROOT/rollback-missing.log"; then
		ok 'a rollback to a release that is not on the node is refused, and names the releases that are'
	else
		bad 'the refusal does not say which releases are on the node'
	fi
fi

# ------------------------------------------------------------- 7. the ACL play
printf -- '-- 7. the ACL play against an agent with ACLs\n'
acl_dir=$TEST_ROOT/acl
mkdir -p -- "$acl_dir/etc"
cat >"$acl_dir/etc/10-acl.json" <<EOF
{
  "acl": {"enabled": true},
  "ports": {"http": $ACL_PORT, "rpc": $((ACL_PORT + 1)), "serf": $((ACL_PORT + 2))}
}
EOF
if ss -lnt 2>/dev/null | grep -qE ":($ACL_PORT|$((ACL_PORT + 1))|$((ACL_PORT + 2))) "; then
	die "port $ACL_PORT/$(($ACL_PORT + 1))/$(($ACL_PORT + 2)) is in use; stop what holds it (the checks use their own ports)"
fi
"$NOMAD" agent -dev -config "$acl_dir/etc" >"$acl_dir/agent.log" 2>&1 &
acl_pid=$!
printf '   agent pid %s\n' "$acl_pid"
export NOMAD_ADDR=http://127.0.0.1:$ACL_PORT
acl_token=''
for _ in $(seq 1 30); do
	if ! kill -0 "$acl_pid" 2>/dev/null; then break; fi
	if acl_token=$(nomad acl bootstrap -json 2>/dev/null | python3 -c 'import json,sys; print(json.load(sys.stdin)["SecretID"])' 2>/dev/null) && [[ -n $acl_token ]]; then
		break
	fi
	sleep 1
done
if [[ -n $acl_token ]]; then
	ok 'the ACL agent bootstrapped a management token'
else
	bad 'the ACL agent did not bootstrap (tests/.test/acl/agent.log)'
	tail -10 "$acl_dir/agent.log"
fi

if [[ -n $acl_token ]]; then
	if play_acl -e "box_acl_addr=$NOMAD_ADDR" -e "box_acl_management_token=$acl_token" \
		-e 'box_acl_workload_jobs=["dev1"]' >"$TEST_ROOT/acl.log" 2>&1; then
		ok 'acl.yml applies the policies'
	else
		bad 'acl.yml failed (tests/.test/acl.log)'
		tail -30 "$TEST_ROOT/acl.log"
	fi
	if NOMAD_TOKEN=$acl_token nomad acl policy list 2>/dev/null | grep -qE '^box-operator\b'; then
		ok 'the cluster has the box-operator policy'
	else
		bad 'the cluster has no box-operator policy'
	fi
	if NOMAD_TOKEN=$acl_token nomad acl policy info box-workload-dev1 2>/dev/null | grep -q 'JobID *= *box-dev1'; then
		ok 'the workload policy is attached to the box job it belongs to'
	else
		bad 'the workload policy is not attached to a job'
	fi
	if play_acl -e "box_acl_addr=$NOMAD_ADDR" -e "box_acl_management_token=$acl_token" \
		-e 'box_acl_workload_jobs=["dev1"]' >"$TEST_ROOT/acl2.log" 2>&1; then
		if grep -qE 'changed=0 .*failed=0' "$TEST_ROOT/acl2.log"; then
			ok 'a second acl.yml run changes nothing'
		else
			bad 'a second acl.yml run changed something'
			grep -E 'changed=[1-9]' "$TEST_ROOT/acl2.log" | head -5
		fi
	else
		bad 'the second acl.yml run failed (tests/.test/acl2.log)'
		tail -20 "$TEST_ROOT/acl2.log"
	fi
	if play_acl -e "box_acl_addr=$NOMAD_ADDR" -e 'box_acl_management_token=not-a-management-token' \
		>"$TEST_ROOT/acl-badtoken.log" 2>&1; then
		bad 'acl.yml accepted a token that may not write policies'
	else
		if grep -qE 'not a management token|HTTP 403' "$TEST_ROOT/acl-badtoken.log"; then
			ok 'acl.yml refuses a token that may not write policies'
		else
			bad 'the refusal does not say the token is not a management token'
		fi
	fi
fi

kill -TERM "$acl_pid" 2>/dev/null
for _ in $(seq 1 20); do
	kill -0 "$acl_pid" 2>/dev/null || break
	sleep 1
done
kill -KILL "$acl_pid" 2>/dev/null
unset NOMAD_ADDR

# ------------------------------------------------------------- 8. a real agent
printf -- '-- 8. a real non-root Nomad agent on the rendered configuration\n'
export NOMAD_TEST_PREFIX=$TEST_ROOT/node
if ss -lnt 2>/dev/null | grep -qE ":($HTTP_PORT|54647|54648) "; then
	die "port $HTTP_PORT/54647/54648 is in use; stop what holds it (the checks use their own ports)"
fi

agent_log=$TEST_ROOT/agent.log
"$P/usr/local/bin/nomad" agent -dev -config "$P/etc/nomad.d" >"$agent_log" 2>&1 &
agent_pid=$!
printf '   agent pid %s\n' "$agent_pid"

export NOMAD_ADDR=http://127.0.0.1:$HTTP_PORT
# `nomad node status <arg>` wants a node ID, not a name, so the ID is taken from
# the list (a -dev agent registers exactly one node).
ready=false
node_id=''
for _ in $(seq 1 30); do
	if ! kill -0 "$agent_pid" 2>/dev/null; then break; fi
	node_id=$(nomad node status -quiet 2>/dev/null | head -1)
	if [[ -n $node_id ]] && nomad node status -verbose "$node_id" 2>/dev/null | grep -q '^Status * = *ready'; then
		ready=true
		break
	fi
	sleep 2
done

if [[ $ready == true ]]; then
	ok 'the agent started on the rendered configuration and registered the node'
else
	bad 'the agent did not register the node'
	tail -20 "$agent_log"
fi

if [[ $ready == true ]]; then
	node_status=$TEST_ROOT/node-status.txt
	nomad node status -verbose "$node_id" >"$node_status" 2>&1
	check_grep 'the node registered under the configured name' "^Name * = $NODE_NAME\$" "$node_status"
	check_grep 'the node runs in the configured datacenter' '^DC * = dc-test$' "$node_status"
	check_grep 'the node carries the configured metadata' 'box_test *= *yes' "$node_status"
	check_grep 'the box driver is detected' '^box +true' "$node_status"
	check_grep 'the box driver is healthy' '^box +true +true +Healthy' "$node_status"
	check_grep 'the box driver reports the configured runtime user' "driver\.box\.runtime_user *= $RUN_USER" "$node_status"
	check_grep 'the box driver reports lima' 'driver\.box\.lima_version *= 2\.' "$node_status"
	check_grep 'the box-volume host volume plugin is fingerprinted' 'plugins\.host_volume\.box-volume\.version *= 0\.' "$node_status"
fi

kill -TERM "$agent_pid" 2>/dev/null
for _ in $(seq 1 20); do
	kill -0 "$agent_pid" 2>/dev/null || break
	sleep 1
done
if kill -0 "$agent_pid" 2>/dev/null; then
	kill -KILL "$agent_pid" 2>/dev/null
	bad 'the agent had to be killed'
else
	ok 'the agent stopped on SIGTERM'
fi
unset NOMAD_ADDR

# ------------------------------------------------------------- 9. uninstall
printf -- '-- 9. uninstall keeps the boxes and the node state\n'
export NOMAD_TEST_PREFIX=$TEST_ROOT/uninstall
prepare_prefix "$NOMAD_TEST_PREFIX"
U=$NOMAD_TEST_PREFIX
use_release "$V1"
if play "$TESTS_DIR/play-node.yml" >"$TEST_ROOT/uninstall-apply.log" 2>&1; then
	ok 'the node installs before the uninstall'
else
	bad 'the node did not install before the uninstall (tests/.test/uninstall-apply.log)'
	tail -20 "$TEST_ROOT/uninstall-apply.log"
fi
mkdir -p -- "$U/var/lib/box/lima" "$U/var/lib/nomad"
printf 'the boxes\n' >"$U/var/lib/box/lima/keep-me"
printf 'the node state\n' >"$U/var/lib/nomad/keep-me"

if play uninstall.yml --limit node-01 >"$TEST_ROOT/uninstall-refused.log" 2>&1; then
	bad 'uninstall.yml ran without a confirmation'
else
	if grep -q 'box_uninstall_confirm' "$TEST_ROOT/uninstall-refused.log"; then
		ok 'uninstall.yml refuses to run without box_uninstall_confirm=true'
	else
		bad 'the refusal does not name box_uninstall_confirm'
	fi
fi

if play uninstall.yml --limit node-01 -e "box_uninstall_confirm=true" >"$TEST_ROOT/uninstall.log" 2>&1; then
	ok 'uninstall.yml removes the box client'
else
	bad 'uninstall.yml failed (tests/.test/uninstall.log)'
	tail -30 "$TEST_ROOT/uninstall.log"
fi
check 'the release directory is gone' test ! -e "$U/opt/box"
check 'the box-volume settings are gone' test ! -e "$U/etc/box/volume.json"
check 'the client configuration is gone' test ! -e "$U/etc/nomad.d/10-base.json"
check 'the unit is gone' test ! -e "$U/etc/systemd/system/nomad.service"
check 'the Lima home is still there' test -f "$U/var/lib/box/lima/keep-me"
check 'the client data directory is still there' test -f "$U/var/lib/nomad/keep-me"

export NOMAD_TEST_PREFIX=$TEST_ROOT/coordinator
C=$NOMAD_TEST_PREFIX
mkdir -p -- "$C/home/.config/box"
printf 'NOMAD_ADDR=http://nomad.example:4646\n' >"$C/home/.config/box/env"
if play uninstall.yml --limit coordinator-01 -e "box_uninstall_confirm=true" >"$TEST_ROOT/uninstall-cli.log" 2>&1; then
	ok 'uninstall.yml removes the CLI of the coordinator'
else
	bad 'uninstall.yml failed for the coordinator (tests/.test/uninstall-cli.log)'
	tail -30 "$TEST_ROOT/uninstall-cli.log"
fi
check 'the CLI is gone' test ! -e "$C/usr/local/bin/box"
check 'the releases of the coordinator are gone' test ! -e "$C/opt/box"
check 'the skill is gone from the Prime Agent' test ! -e "$C/home/.agents/skills/box"
check 'the copied skill of Claude Code is gone' test ! -e "$C/home/.claude/skills/box"
if [[ $claude_mode == plugin ]]; then
	if claude_plugin plugin list --json 2>/dev/null | grep -q '"box@nomad-driver-lima"'; then
		bad 'uninstall left the box plugin in Claude Code'
	else
		ok 'uninstall took the box plugin off Claude Code'
	fi
	if claude_plugin plugin marketplace list --json 2>/dev/null | grep -q '"nomad-driver-lima"'; then
		bad 'uninstall left the marketplace of this repository in Claude Code'
	else
		ok 'uninstall removed the marketplace of this repository'
	fi
fi
check 'the environment file of the coordinator is kept' test -f "$C/home/.config/box/env"

# --------------------------------------------------------------------- summary
printf '\n%d passed, %d failed\n' "$passed" "$failed"
if ((failed > 0)); then
	printf 'logs: %s\n' "$TEST_ROOT"
	exit 1
fi
printf 'logs: %s\n' "$TEST_ROOT"
exit 0
