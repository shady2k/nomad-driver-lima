#!/usr/bin/env bash
#
# deploy/accept.sh - acceptance run of the box stack against the live cluster.
#
# The script runs the whole path an operator cares about, with the `box` CLI only
# (plus `nomad node status -verbose` for the disk-usage attributes and the
# restart hook you pass in). Every step prints PASS or FAIL with the expected and
# the actual value; the script exits non-zero when any step fails.
#
#   ./deploy/accept.sh --node node-01 --model <provider>/<model> \
#       --restart-cmd 'ssh node-01 sudo systemctl restart nomad'
#
#   ./deploy/accept.sh --dry-run          # print the steps, change nothing
#
# What it checks, in order:
#   1  box capacity           the node can run boxes: driver, volume plugin, room
#   2  box images             the 1.0.x base image exists for the node's arch
#   3  box create            1 vCPU / 2048 MiB / 10 GiB box on that node
#   4  box run uname         the guest is the node's architecture
#   5  box run df             the guest root filesystem is the 10 GiB disk
#   6  box cp both ways      a file in, a file out, contents compared
#   7  box session new/send/wait/events   a prime session answers "pong"
#   8  nomad node status -verbose         the driver reports the disk usage
#   9  restart the Nomad client          same allocation, same VM, same session
#  10  box rm                the job and the disk are gone
#
# The environment is the same as for `box` itself: NOMAD_ADDR, NOMAD_TOKEN,
# NOMAD_CACERT (see deploy/README.md, or the env file the coordinator role
# writes). python3 is needed to read the --json output.
set -euo pipefail

readonly PROG=${0##*/}

die() {
	printf '%s: %s\n' "$PROG" "$*" >&2
	exit 2
}

usage() {
	cat <<EOF
usage: $PROG [options]

Run the acceptance checks against the live cluster with the box CLI.

  --node NAME         node to pin the box to (default: the first node
                      \`box capacity\` reports as ready)
  --image VERSION     base image version (default: the newest 1.0.x for the
                      node's architecture)
  --arch amd64|arm64  architecture (default: the node's)
  --box ID            box id (default: accept-<epoch>)
  --executor NAME     session executor (default: prime)
  --model MODEL       model as <provider>/<model>, for the session step
                      (or set BOX_ACCEPT_MODEL)
  --restart-cmd CMD   shell command that restarts the Nomad client on that node,
                      for example 'ssh node-01 sudo systemctl restart nomad'
                      (without it, step 9 is skipped)
  --nomad-bin PATH    nomad CLI for \`nomad node status -verbose\` (default: nomad)
  --timeout DUR       how long to wait for the box to run (default: 15m)
  --session-timeout DUR  how long to wait for the session's turn (default: 10m)
  --restart-timeout DUR  how long to wait for the node to come back (default: 6m)
  --skip-session      skip the session steps (1-3 of them are reported SKIP)
  --keep              keep the box at the end instead of deleting it
  --dry-run           print the steps and the commands, run nothing
  -h, --help          this text

Exit status: 0 every step passed, 1 a step failed, 2 usage.
EOF
}

node=''
image=''
arch=''
box_id="accept-$(date +%s)"
executor='prime'
model=${BOX_ACCEPT_MODEL:-}
restart_cmd=''
nomad_bin='nomad'
timeout='15m'
session_timeout='10m'
restart_timeout='6m'
skip_session=false
keep=false
dry_run=false

while (($# > 0)); do
	case $1 in
	--node) (($# >= 2)) || die "--node needs a value"; node=$2; shift 2 ;;
	--image) (($# >= 2)) || die "--image needs a value"; image=$2; shift 2 ;;
	--arch) (($# >= 2)) || die "--arch needs a value"; arch=$2; shift 2 ;;
	--box) (($# >= 2)) || die "--box needs a value"; box_id=$2; shift 2 ;;
	--executor) (($# >= 2)) || die "--executor needs a value"; executor=$2; shift 2 ;;
	--model) (($# >= 2)) || die "--model needs a value"; model=$2; shift 2 ;;
	--restart-cmd) (($# >= 2)) || die "--restart-cmd needs a value"; restart_cmd=$2; shift 2 ;;
	--nomad-bin) (($# >= 2)) || die "--nomad-bin needs a value"; nomad_bin=$2; shift 2 ;;
	--timeout) (($# >= 2)) || die "--timeout needs a value"; timeout=$2; shift 2 ;;
	--session-timeout) (($# >= 2)) || die "--session-timeout needs a value"; session_timeout=$2; shift 2 ;;
	--restart-timeout) (($# >= 2)) || die "--restart-timeout needs a value"; restart_timeout=$2; shift 2 ;;
	--skip-session) skip_session=true; shift ;;
	--keep) keep=true; shift ;;
	--dry-run) dry_run=true; shift ;;
	-h | --help) usage; exit 0 ;;
	*) die "unknown argument: $1 (see --help)" ;;
	esac
done

if [[ -z $model && $skip_session == false ]] && ! $dry_run; then
	die "the session step needs --model <provider>/<model> (or BOX_ACCEPT_MODEL); pass --skip-session to leave it out"
fi

# ---------------------------------------------------------------- reporting
STEP_N=0
STEP_TITLE=''
PASSES=0
FAILURES=0
SKIPS=0
FAILED_STEPS=()

step() {
	STEP_N=$((STEP_N + 1))
	STEP_TITLE=$1
	printf '\n== step %d: %s\n' "$STEP_N" "$STEP_TITLE"
}

pass() {
	printf 'PASS  step %2d: %s%s\n' "$STEP_N" "$STEP_TITLE" "${1:+ ($1)}"
	PASSES=$((PASSES + 1))
}

fail() {
	printf 'FAIL  step %2d: %s\n        expected: %s\n        actual:   %s\n' \
		"$STEP_N" "$STEP_TITLE" "$1" "$2"
	FAILURES=$((FAILURES + 1))
	FAILED_STEPS+=("$STEP_N $STEP_TITLE")
}

skip() {
	printf 'SKIP  step %2d: %s (%s)\n' "$STEP_N" "$STEP_TITLE" "$1"
	SKIPS=$((SKIPS + 1))
}

# would_run prints the command a step would run, in a dry run.
would_run() {
	local out='' arg
	for arg in "$@"; do
		if [[ $arg == *' '* || $arg == *'$'* || $arg == *'/'* ]]; then
			out+=" '$arg'"
		else
			out+=" $arg"
		fi
	done
	printf 'DRY   ran:%s\n' "$out"
}

# boxq runs the box CLI and returns its output; the caller checks the status.
boxq() {
	local out
	if ! out=$("box" "$@" 2>&1); then
		printf '%s\n' "$out"
		return 1
	fi
	printf '%s\n' "$out"
}

# jsonq reads one JSON document on stdin and prints the value of each Python
# expression, with d bound to the document. Empty results print nothing.
jsonq() {
	python3 -c '
import json, sys
try:
    d = json.load(sys.stdin)
except ValueError as exc:
    sys.stderr.write("cannot read the JSON output: %s\n" % exc)
    sys.exit(1)
for expr in sys.argv[1:]:
    try:
        value = eval(expr, {"d": d, "len": len, "str": str, "int": int, "any": any, "all": all})
    except Exception as exc:  # noqa: BLE001 - the expression is ours
        sys.stderr.write("cannot evaluate %s: %s\n" % (expr, exc))
        sys.exit(1)
    if value is None:
        value = ""
    print(value)
' "$@"
}

require_python() {
	command -v python3 >/dev/null 2>&1 || die "python3 is required to read the box --json output"
}

require_box() {
	command -v box >/dev/null 2>&1 || die "the box CLI is not in PATH; install it with deploy/ansible/site.yml (the box_coordinators group)"
}

# seconds turns a duration the CLI accepts (30s, 5m, 1h) into seconds, for the
# waits this script keeps itself.
seconds() {
	local value=${1%[smh]} unit=${1: -1}
	case $unit in
	m) printf '%d' "$((value * 60))" ;;
	h) printf '%d' "$((value * 3600))" ;;
	*) printf '%d' "$((value))" ;;
	esac
}

note_box_kept() {
	printf '\nthe box %s was kept for inspection:\n' "$box_id"
	printf '  box status %s\n  box logs %s\n  box rm %s --yes\n' "$box_id" "$box_id" "$box_id"
}

require_python
require_box

printf 'box acceptance run: box %s, node %s, image %s, model %s\n' \
	"$box_id" "${node:-<auto>}" "${image:-<1.0.x>}" "${model:-<none>}"
$dry_run && printf '(dry run: nothing is created, changed or deleted)\n'

# ------------------------------------------------------------------ state the
# steps share
node_name=''
capacity_unreadable=false
node_arch=''
session_id=''
turn=0
alloc_before=''
boot_id_before=''
marker='box-accept-marker'

# -------------------------------------------------------------------- step 1
step 'box capacity lists a node that can run boxes'
if $dry_run; then
	would_run box capacity --json
else
	capacity='{"nodes": []}'
	if ! capacity_out=$(boxq capacity --json); then
		capacity='{"nodes": []}'
		capacity_unreadable=true
		fail 'a JSON capacity answer' "box capacity failed: $(printf '%s' "$capacity_out" | tr '\n' ' ' | cut -c1-200)"
	else
		capacity=$capacity_out
	fi
	nodes=$(printf '%s' "$capacity" | jsonq 'chr(10).join(n["name"] for n in (d.get("nodes") or []))' 2>/dev/null || true)
	if [[ -n $node ]]; then
		# The name goes into the expression inside single quotes: node names are
		# letters, digits, dashes and dots, so nothing needs escaping here (a
		# %q-quoted name is not a Python string and broke the step once).
		picked=$(printf '%s' "$capacity" | jsonq "chr(10).join(n['name'] for n in (d.get('nodes') or []) if n['name'] == '$node')" 2>/dev/null || true)
		[[ -n $picked ]] || picked=''
	else
		picked=$(printf '%s' "$capacity" | jsonq "chr(10).join(n['name'] for n in (d.get('nodes') or []) if (n.get('driver') or {}).get('installed') and n.get('eligible') and (n.get('volume_plugins') or []) )" 2>/dev/null || true)
		picked=${picked%%$'\n'*}
	fi
	if $capacity_unreadable; then
		: # step 1 already reported why the answer could not be read
	elif [[ -z $picked ]]; then
		fail "a ready node${node:+ named $node}" "the nodes are: ${nodes:-none reported}"
	else
		node_name=$picked
		detail=$(printf '%s' "$capacity" | jsonq \
			"next((str(n.get('driver',{}).get('version','?')) + ' lima ' + str(n.get('driver',{}).get('lima_version','?')) + ', ' + str(len(n.get('volume_plugins') or [])) + ' volume plugin(s), ' + str(n.get('disk_mib',{}).get('free',0)) + ' MiB free disk') for n in (d.get('nodes') or []) if n['name'] == '$node_name')" 2>/dev/null || true)
		node_arch=$(printf '%s' "$capacity" | jsonq "next((n.get('arch','') for n in (d.get('nodes') or []) if n['name'] == '$node_name'), '')" 2>/dev/null || true)
		plugin_ok=$(printf '%s' "$capacity" | jsonq "next(('ok' if any((p.get('id') == 'box-volume') for p in (n.get('volume_plugins') or [])) else 'missing box-volume') for n in (d.get('nodes') or []) if n['name'] == '$node_name')" 2>/dev/null || true)
		driver_ok=$(printf '%s' "$capacity" | jsonq "next(('ok' if (n.get('driver') or {}).get('installed') else 'driver not installed') for n in (d.get('nodes') or []) if n['name'] == '$node_name')" 2>/dev/null || true)
		if [[ $plugin_ok == ok && $driver_ok == ok ]]; then
			pass "$node_name: $detail"
		else
			fail 'driver installed and box-volume plugin present' "$driver_ok, $plugin_ok"
		fi
	fi
fi

# -------------------------------------------------------------------- step 2
step 'box images lists the 1.0.x base image for the node'
if $dry_run; then
	would_run box images --json
else
	images=$(boxq images --json) || {
		fail 'a JSON image list' "box images failed: $images"
		images='{"images": []}'
	}
	wanted_arch=${arch:-$node_arch}
	[[ -n $wanted_arch ]] || wanted_arch='amd64'
	if [[ -n $image ]]; then
		found=$(printf '%s' "$images" | jsonq "chr(10).join(i['version'] + ' ' + i['arch'] for i in (d.get('images') or []) if i['version'] == '$image' and i['arch'] == '$wanted_arch')" 2>/dev/null || true)
		[[ -n $found ]] || found='missing'
	else
		found=$(printf '%s' "$images" | jsonq "chr(10).join(i['version'] + ' ' + i['arch'] for i in (d.get('images') or []) if i['version'].startswith('1.0.') and i['arch'] == '$wanted_arch')" 2>/dev/null || true)
	fi
	if [[ -z $found ]]; then
		fail "a 1.0.x image for $wanted_arch" "the index offers: $(printf '%s' "$images" | jsonq "chr(10).join(i['version'] + ' ' + i['arch'] for i in (d.get('images') or []))" 2>/dev/null || echo none)"
	elif [[ $found == missing ]]; then
		fail "image $image for $wanted_arch" "that version is not published"
	else
		# The newest matching version, by version sort.
		image=$(printf '%s\n' "$found" | awk '{print $1}' | sort -V | tail -1)
		arch=$wanted_arch
		pass "image $image for $arch"
	fi
fi

# -------------------------------------------------------------------- step 3
step "box create makes a 1 vCPU / 2048 MiB / 10 GiB box on ${node_name:-$node}"
if $dry_run; then
	would_run box create "$box_id" --image "${image:-1.0.x}" --cpus 1 --memory-mib 2048 --disk-gib 10 --arch "${arch:-amd64}" --node "${node:-NODE}" --timeout "$timeout" --json
else
	set +e
	created=$(boxq create "$box_id" --image "$image" --cpus 1 --memory-mib 2048 --disk-gib 10 --arch "$arch" --node "$node_name" --timeout "$timeout" --json)
	rc=$?
	set -e
	if ((rc != 0)); then
		fail "a running box named $box_id" "box create exited $rc: $(printf '%s' "$created" | tr '\n' ' ')"
		printf '\nif the box exists it was kept: box status %s / box rm %s --yes\n' "$box_id" "$box_id"
		printf '\n%d step(s) failed, %d passed, %d skipped\n' "$FAILURES" "$PASSES" "$SKIPS"
		exit 1
	fi
	state=$(printf '%s' "$created" | jsonq "d['box']['state']" 2>/dev/null || true)
	spec=$(boxq status "$box_id" --json) || spec='{}'
	got=$(printf '%s' "$spec" | jsonq "'%s vCPU, %s MiB, %s GiB on %s' % (d['spec']['cpus'], d['spec']['memory_mib'], d['spec']['disk_gib'], (d.get('box') or {}).get('node','?'))" 2>/dev/null || true)
	if [[ $state == running && $got == '1 vCPU, 2048 MiB, 10 GiB on '"$node_name" ]]; then
		pass "$got, state running"
	else
		fail "a running box: 1 vCPU, 2048 MiB, 10 GiB on $node_name" "state $state, $got"
		note_box_kept
	fi
fi

# -------------------------------------------------------------------- step 4
step 'box run uname -m reports the node architecture'
if $dry_run; then
	would_run box run "$box_id" -- uname -m
else
	uname_out=$(boxq run "$box_id" -- uname -m) || true
	want_uname='x86_64'
	[[ ${arch:-$node_arch} == arm64 ]] && want_uname='aarch64'
	if [[ $uname_out == "$want_uname" ]]; then
		pass "$uname_out"
	else
		fail "$want_uname" "${uname_out:-no output}"
	fi
fi

# -------------------------------------------------------------------- step 5
step 'box run df -h / shows the 10 GiB disk'
if $dry_run; then
	would_run box run "$box_id" -- df -h /
else
	df_out=$(boxq run "$box_id" -- df -h /) || true
	size=$(printf '%s\n' "$df_out" | awk 'NR == 2 {print $2}' | tr -d ' ')
	avail=$(printf '%s\n' "$df_out" | awk 'NR == 2 {print $4}' | tr -d ' ')
	gib=$(printf '%s' "$size" | awk '{n=$0; sub(/G$/,"",n); printf "%d", n}')
	# The box image keeps its EFI, /boot and BIOS partitions at the front of the
	# disk (about 1 GiB), and ext4 metadata takes a few hundred MiB of the rest,
	# so a 10 GiB box shows about 8.7G usable on / (measured: a 9.0 GiB
	# filesystem). An image that was never grown shows 6.8G, which is what this
	# range still catches.
	if [[ -n $size && $gib =~ ^[0-9]+$ ]] && ((gib >= 8 && gib <= 11)); then
		pass "size $size, available $avail"
	else
		fail 'the 10 GiB disk of the box as / (about 8.7G usable)' "${size:-no df output} (whole output: $(printf '%s' "$df_out" | tr '\n' ' '))"
	fi
fi

# -------------------------------------------------------------------- step 6
step 'box cp carries a file in and a file out'
if $dry_run; then
	would_run 'box cp <local file> '"$box_id"':/tmp/accept-in.txt'
	would_run box run "$box_id" -- cat /tmp/accept-in.txt
	# shellcheck disable=SC2016 # the $1 is expanded in the guest, not here
	would_run box run "$box_id" -- sh -c 'printf %s "$1" > /tmp/accept-out.txt' sh "$marker"-out
	would_run 'box cp '"$box_id"':/tmp/accept-out.txt <local file>'
else
	tmpdir=$(mktemp -d)
	# shellcheck disable=SC2064 # the path is fixed here on purpose
	trap "rm -rf -- '$tmpdir'" EXIT
	in_marker="$marker-in-$(date +%s)"
	out_marker="$marker-out-$(date +%s)"
	printf '%s\n' "$in_marker" >"$tmpdir/in.txt"
	cp_in=$(boxq cp "$tmpdir/in.txt" "$box_id:/tmp/accept-in.txt") || true
	read_back=$(boxq run "$box_id" -- cat /tmp/accept-in.txt) || true
	# shellcheck disable=SC2016 # the $1 is expanded in the guest, not here
	write_out=$(boxq run "$box_id" -- sh -c 'printf "%s\n" "$1" > /tmp/accept-out.txt' sh "$out_marker") || true
	cp_out=$(boxq cp "$box_id:/tmp/accept-out.txt" "$tmpdir/out.txt") || true
	read_local=''
	[[ -f $tmpdir/out.txt ]] && read_local=$(cat "$tmpdir/out.txt")
	if [[ $read_back == "$in_marker" && $read_local == "$out_marker" ]]; then
		pass 'the file in came back unchanged and the file out matches'
	else
		fail "in: '$in_marker', out: '$out_marker'" \
			"in: '${read_back:-nothing}' (cp: ${cp_in:-no answer}), out: '${read_local:-nothing}' (cp: ${cp_out:-no answer}, run: ${write_out:-no answer})"
	fi
fi

# -------------------------------------------------------------------- step 7
step "box session new ($executor) answers a prompt with pong"
if $dry_run; then
	would_run box session new "$box_id" --executor "$executor" --model "${model:-<provider>/<model>}" --title accept --json
	would_run box send "$box_id" '<session>' 'reply with the word pong'
	would_run box wait "$box_id" '<session>' --timeout "$session_timeout" --json
	would_run box events "$box_id" '<session>' --json
elif $skip_session || [[ -z $model ]]; then
	skip '--skip-session'
else
	new_session=$(boxq session new "$box_id" --executor "$executor" --model "$model" --title accept --json) || true
	session_id=$(printf '%s' "$new_session" | jsonq "d.get('session','')" 2>/dev/null || true)
	if [[ -z $session_id ]]; then
		fail 'a session id' "${new_session:-no answer}"
	else
		sent=$(boxq send "$box_id" "$session_id" 'reply with the word pong' --json) || true
		turn=$(printf '%s' "$sent" | jsonq "d.get('turn',0)" 2>/dev/null || echo 0)
		waited=$(boxq wait "$box_id" "$session_id" --timeout "$session_timeout" --json) || true
		text=$(printf '%s' "$waited" | jsonq "d.get('text','')" 2>/dev/null || true)
		state=$(printf '%s' "$waited" | jsonq "d.get('state','')" 2>/dev/null || true)
		events=$(boxq events "$box_id" "$session_id" --json) || true
		events_n=$(printf '%s\n' "$events" | grep -c . || true)
		# The contract's finished turn state is "done" (docs/boxctl-contract.md):
		# "finished" is not a value boxd ever returns.
		if [[ $state == 'done' && ${text,,} == *pong* && $events_n -gt 0 ]]; then
			pass "session $session_id turn $turn finished, $events_n event(s), answer: $(printf '%s' "$text" | tr '\n' ' ' | cut -c1-60)"
		else
			fail 'a done turn whose answer contains pong, and at least one event' \
				"state '$state', answer '$(printf '%s' "$text" | tr '\n' ' ' | cut -c1-120)', $events_n event(s); wait: $(printf '%s' "$waited" | tr '\n' ' ' | cut -c1-160)"
		fi
	fi
fi

# -------------------------------------------------------------------- step 8
step 'nomad node status -verbose reports the disk usage of the box'
if $dry_run; then
	would_run "$nomad_bin" node status -verbose "${node_name:-$node}"
	would_run "grep driver.box.disk. of that output"
else
	# `nomad node status -verbose` takes a node *id*: a name fails with "Invalid
	# UUID". The list gives the id of the node this run picked.
	node_id=$(NOMAD_ADDR=${NOMAD_ADDR:-} "$nomad_bin" node status -json 2>/dev/null | python3 -c '
import json, sys
name = sys.argv[1]
try:
    nodes = json.load(sys.stdin)
except ValueError:
    sys.exit(1)
for node in nodes:
    if node.get("Name") == name:
        print(node.get("ID", ""))
        break
' "$node_name" 2>/dev/null || true)
	[[ -n $node_id ]] || node_id=$node_name
	node_status=$("$nomad_bin" node status -verbose "$node_id" 2>&1) || node_status=''

	used=$(printf '%s\n' "$node_status" | awk -v key="driver.box.disk.$box_id.used_bytes" '$1 == key {print $3}' | head -1)
	total=$(printf '%s\n' "$node_status" | awk '$1 == "driver.box.disk.used_bytes" {print $3}' | head -1)
	if [[ -n $used && -n $total ]] && ((used > 0)); then
		pass "box $box_id uses $used bytes, all boxes on the node use $total bytes"
	else
		fail "driver.box.disk.$box_id.used_bytes and driver.box.disk.used_bytes above 0" \
			"$(printf '%s\n' "$node_status" | grep -c 'driver.box.disk.' || true) disk attribute(s): $(printf '%s\n' "$node_status" | grep 'driver.box.disk.' | head -4 | tr '\n' ' ')"
	fi
fi

# -------------------------------------------------------------------- step 9
step 'restarting the Nomad client keeps the allocation, the VM and the session'
if [[ -z $restart_cmd ]]; then
	skip 'no --restart-cmd was given'
elif $dry_run; then
	would_run box status "$box_id" --json
	would_run box run "$box_id" -- cat /proc/sys/kernel/random/boot_id
	would_run "$restart_cmd"
	would_run 'wait for the box to report running again, then compare the allocation, boot id, marker file and session'
	would_run box status "$box_id" --json
else
	before=$(boxq status "$box_id" --json) || before='{}'
	alloc_before=$(printf '%s' "$before" | jsonq "((d.get('allocation') or {}).get('id',''))" 2>/dev/null || true)
	boot_id_before=$(boxq run "$box_id" -- cat /proc/sys/kernel/random/boot_id) || true
	printf 'restarting the Nomad client: %s\n' "$restart_cmd"
	set +e
	restart_out=$(sh -c "$restart_cmd" 2>&1)
	restart_rc=$?
	set -e
	# Wait for the node to come back and the box to report running again.
	after='{}'
	deadline=$((SECONDS + $(seconds "$restart_timeout")))
	while ((SECONDS < deadline)); do
		if after_json=$(boxq status "$box_id" --json 2>/dev/null); then
			after=$after_json
			after_state=$(printf '%s' "$after" | jsonq "d['box']['state']" 2>/dev/null || true)
			after_alloc_state=$(printf '%s' "$after" | jsonq "((d.get('allocation') or {}).get('client_status',''))" 2>/dev/null || true)
			[[ $after_state == running && $after_alloc_state == running ]] && break
		fi
		sleep 5
	done
	alloc_after=$(printf '%s' "$after" | jsonq "((d.get('allocation') or {}).get('id',''))" 2>/dev/null || true)
	# The allocation is restored before the restarted client can exec into it: a
	# command in the guest answers "No path to node" for a few seconds after the
	# state says running (measured: about 10 s). Wait for the guest, then compare.
	boot_id_after=''
	for _ in $(seq 1 36); do
		if boot_id_after=$(boxq run "$box_id" -- cat /proc/sys/kernel/random/boot_id 2>/dev/null); then
			break
		fi
		boot_id_after=''
		sleep 5
	done
	marker_after=$(boxq run "$box_id" -- cat /tmp/accept-in.txt) || true
	# The polls above can answer before the guest is reachable again, and the
	# session list comes from the guest: read the status once more now.
	if after_json=$(boxq status "$box_id" --json 2>/dev/null); then
		after=$after_json
	fi
	session_after=$(printf '%s' "$after" | jsonq "chr(10).join(s['id'] for s in (d.get('sessions') or []))" 2>/dev/null || true)
	events_after=''
	if [[ -n $session_id ]]; then
		events_after=$(boxq events "$box_id" "$session_id" --json) || true
	fi
	problems=''
	((restart_rc == 0)) || problems+="the restart command exited $restart_rc: $(printf '%s' "$restart_out" | tr '\n' ' ' | cut -c1-100); "
	[[ -n $alloc_before && $alloc_after == "$alloc_before" ]] || problems+="allocation was '$alloc_before', now '$alloc_after'; "
	[[ -n $boot_id_before && $boot_id_after == "$boot_id_before" ]] || problems+="the VM boot id was '$boot_id_before', now '$boot_id_after' (the VM was restarted); "
	[[ $marker_after == "$in_marker" ]] || problems+="the marker file reads '${marker_after:-nothing}' instead of '$in_marker'; "
	if [[ -n $session_id ]]; then
		printf '%s\n' "$session_after" | grep -qx -- "$session_id" || problems+="session $session_id is gone from the box; "
		[[ -n $events_after ]] || problems+="the events of session $session_id are gone; "
	fi
	if [[ -z $problems ]]; then
		pass "allocation $alloc_after, boot id $(printf '%s' "$boot_id_before" | cut -c1-8), marker file and session $session_id survived"
	else
		fail 'the same allocation, the same VM, the same disk and the same session' "$problems"
	fi
fi

# ------------------------------------------------------------------- step 10
step 'box rm deletes the job and the disk'
if $dry_run; then
	would_run box rm "$box_id" --yes
	would_run box status "$box_id"
elif $keep; then
	skip '--keep'
	note_box_kept
else
	removed=$(boxq rm "$box_id" --yes) || true
	printf '%s\n' "$removed" | sed 's/^/        /'
	set +e
	# Not through boxq: the exit code (3 = not found) is what this step proves,
	# and boxq reports every failure as 1.
	after_status=$("box" status "$box_id" 2>&1)
	after_rc=$?
	set -e
	listed=$(boxq ls --json 2>/dev/null | jsonq "chr(10).join(b['id'] for b in (d.get('boxes') or []))" 2>/dev/null || true)
	if [[ $after_rc -eq 3 ]] && ! printf '%s\n' "$listed" | grep -qx -- "$box_id"; then
		pass 'box status answers not found (exit 3) and box ls does not list it'
	else
		fail 'exit 3 from box status and no entry in box ls' \
			"exit $after_rc: $(printf '%s' "$after_status" | tr '\n' ' ' | cut -c1-120); listed: ${listed:-none}"
	fi
fi

# ----------------------------------------------------------------- summary
printf '\n%d passed, %d failed, %d skipped (box %s)\n' "$PASSES" "$FAILURES" "$SKIPS" "$box_id"
if ((FAILURES > 0)); then
	printf 'failed steps:\n'
	printf '  %s\n' "${FAILED_STEPS[@]}"
	printf 'send these lines back with the output above and the box status of the node\n'
	exit 1
fi
exit 0
