#!/usr/bin/env bash
#
# The Mac fixtures: the opt-in user profile and the launchd lifecycle of both
# scopes, in isolated prefixes, without launchd, without a VM and without Nomad.
#
#   export BOX_MAC_FIXTURE=$(mktemp -d "$HOME/bx.XXXXXX")
#   export BOX_MAC_USER_LIFECYCLE_FIXTURE=$(mktemp -d "$HOME/bx.XXXXXX")
#   export BOX_MAC_SYSTEM_LIFECYCLE_FIXTURE=$(mktemp -d "$HOME/bx.XXXXXX")
#   export BOX_MAC_VERSION=0.0.0-fixture
#   export BOX_MAC_DIST=$PWD/dist/0.0.0-fixture       # deploy/build.sh --version 0.0.0-fixture
#   ./deploy/ansible/tests/test-macos-user.sh
#
# The three prefixes have to be throwaway directories inside the login home (the
# profile refuses anything else), and this script does not remove them: the caller
# knows whether it wants to look at them. Nothing here calls the launchd of the
# machine: every launchctl the plays run is the recorder of the fixture, which
# writes its argv to commands.jsonl, and the play refuses to run unless the
# recorder is the first launchctl on PATH.
#
# What it proves: the two roles install a user-scope client whose every path is
# under the login home, twice in a row with the second run changing nothing; and
# reload, restart and uninstall speak to gui/<uid> for the agent and system for the
# daemon, keep the box disks, and remove the plist. assert-macos-fixture.py holds
# the assertions over the files.
#
# Exit status: 0 every check passed, 1 something failed, 2 a variable or a tool is
# missing.
set -euo pipefail

readonly PROG=${0##*/}

die() {
	printf '%s: %s\n' "$PROG" "$*" >&2
	exit 2
}

for variable in BOX_MAC_FIXTURE BOX_MAC_USER_LIFECYCLE_FIXTURE BOX_MAC_SYSTEM_LIFECYCLE_FIXTURE BOX_MAC_VERSION BOX_MAC_DIST; do
	[[ -n ${!variable:-} ]] || die "$variable is not set; see the header of this script"
done
command -v ansible-playbook >/dev/null 2>&1 || die 'ansible-playbook is not in PATH'
command -v python3 >/dev/null 2>&1 || die 'python3 is not in PATH'
[[ -d $BOX_MAC_DIST ]] || die "$BOX_MAC_DIST is not a release: run deploy/build.sh --version $BOX_MAC_VERSION"

root=$(cd -- "$(dirname -- "$0")/.." && pwd)
export ANSIBLE_ROLES_PATH=$root/roles

# The inventories are generated here rather than kept in the tree: the group is one
# local host with no escalation, and the prefixes come from the environment.
inventory=$BOX_MAC_FIXTURE/inventory.ini
if [[ ! -f $inventory ]]; then
	printf '[box_nodes_mac]\nlocalhost ansible_connection=local ansible_become=false\n' >"$inventory"
fi

play() {
	(cd -- "$root" && ansible-playbook -i "$inventory" "$@")
}

printf '== the Mac fixtures (%s)\n' "${BOX_MAC_VERSION}"
printf '   node %s\n   user lifecycle %s\n   system lifecycle %s\n\n' \
	"$BOX_MAC_FIXTURE" "$BOX_MAC_USER_LIFECYCLE_FIXTURE" "$BOX_MAC_SYSTEM_LIFECYCLE_FIXTURE"

printf -- '-- the user profile, into its prefix\n'
play tests/play-macos-user.yml

printf -- '-- the same run again: nothing changes\n'
play tests/play-macos-user.yml | tee "$BOX_MAC_FIXTURE/repeat.log"
grep -Eq 'changed=0 .*failed=0' "$BOX_MAC_FIXTURE/repeat.log" ||
	die 'the second run of the user profile changed something'

printf -- '-- the lifecycle of the user agent (gui/<uid>)\n'
BOX_MAC_FIXTURE=$BOX_MAC_USER_LIFECYCLE_FIXTURE play tests/play-launchd-fixture.yml

printf -- '-- the lifecycle of the system daemon (system)\n'
BOX_MAC_FIXTURE=$BOX_MAC_SYSTEM_LIFECYCLE_FIXTURE play tests/play-launchd-fixture.yml \
	-e nomad_launchd_scope=system

printf -- '-- the files the fixtures left behind\n'
python3 "$root/tests/assert-macos-fixture.py" \
	--node "$BOX_MAC_FIXTURE" \
	--version "$BOX_MAC_VERSION" \
	--user-lifecycle "$BOX_MAC_USER_LIFECYCLE_FIXTURE" \
	--system-lifecycle "$BOX_MAC_SYSTEM_LIFECYCLE_FIXTURE"

printf '\npass; the prefixes are kept for you to look at\n'
