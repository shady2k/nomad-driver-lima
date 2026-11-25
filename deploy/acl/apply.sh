#!/usr/bin/env bash
# deploy/acl/apply.sh -- put the box ACL policies on a cluster.
#
# Usage:
#   deploy/acl/apply.sh operator  [--token-name NAME] [--no-token] [--namespace NS]
#   deploy/acl/apply.sh workloads [BOX ...] [--all] [--namespace NS]
#   deploy/acl/apply.sh reap      [--namespace NS]
#   deploy/acl/apply.sh clock     [--token ACCESSOR] [--namespace NS]
#   deploy/acl/apply.sh check     [--token SECRET] [--write-probe] [--clock-probe] [--namespace NS]
#
#   operator    apply box-operator.policy.hcl, and (unless --no-token) create the
#               ACL token the coordinator uses. The new token is printed once.
#   workloads   attach box-workload.policy.hcl to box jobs, one policy per job
#               (box-workload-<id>), so their workload identities can read
#               nomad/jobs/box-executors. Without arguments it lists the box jobs
#               it found and changes nothing.
#   reap        apply box-reap.policy.hcl to the periodic job reap-boxes
#               (deploy/reap-boxes.nomad.hcl), which then covers every child it
#               dispatches. Register the job with deploy/reap.sh.
#   clock       apply box-clock.policy.hcl, the stopped-box clock of every box.
#               With --token ACCESSOR also add it to that token's policies,
#               keeping the ones the token already has. It is for a CLI token
#               that is not given the whole box-operator policy.
#   check       run the permission probes with a token: one PASS or FAIL per
#               probe, exit 1 if one fails. Without --token it uses NOMAD_TOKEN.
#               --clock-probe expects a token with box-clock instead of
#               box-operator: the clock probes must pass and the
#               executor-credential probes must be refused. --write-probe adds
#               the writes (in --clock-probe mode, the clock write and delete).
#
# Environment: NOMAD_ADDR, NOMAD_TOKEN (a management token for operator,
# workloads, reap and clock), NOMAD (the nomad binary, default `nomad`), NS
# (default namespace, default `default`).
#
# Nothing here prints a secret except the token of `operator`, once, on stdout.

set -euo pipefail

DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
NOMAD=${NOMAD:-nomad}
PROG=${0##*/}
die() { printf '%s: %s\n' "$PROG" "$*" >&2; exit 2; }
log() { printf '%s: %s\n' "$PROG" "$*" >&2; }
need() { command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"; }

usage() { sed -n '2,35p' "$0" | sed 's/^# \{0,1\}//'; }

NS=${NS:-default}
token_name=box-operator
with_token=true
all_boxes=false
box_ids=()
token=${NOMAD_TOKEN:-}
token_accessor=
write_probe=false
clock_probe=false

command=${1:-}
shift || true
case $command in
  "" | -h | --help | help) usage; exit 0 ;;
  operator | workloads | reap | clock | check) ;;
  *) die "unknown subcommand '$command' (want operator, workloads, reap, clock or check; --help shows the usage)" ;;
esac

while (($# > 0)); do
  case $1 in
    --token-name) (($# >= 2)) || die "--token-name needs a name"; token_name=$2; shift 2 ;;
    --no-token) with_token=false; shift ;;
    --namespace) (($# >= 2)) || die "--namespace needs a namespace"; NS=$2; shift 2 ;;
    --all) all_boxes=true; shift ;;
    --token) (($# >= 2)) || die "--token needs a value"; token=$2; token_accessor=$2; shift 2 ;;
    --write-probe) write_probe=true; shift ;;
    --clock-probe) clock_probe=true; shift ;;
    -h | --help) usage; exit 0 ;;
    -*) die "unknown option '$1' (--help shows the usage)" ;;
    *) box_ids+=("$1"); shift ;;
  esac
done

# policy_for_namespace prints a policy file whose namespace block is NS. The
# files ship with "default", which is what a box is created in.
policy_for_namespace() {
  local file=$1 tmp
  if [[ $NS == default ]]; then
    printf '%s\n' "$file"
    return
  fi
  tmp=$(mktemp "${TMPDIR:-/tmp}/box-acl-XXXXXX.hcl")
  sed "s/^namespace \"default\"/namespace \"$NS\"/" "$file" >"$tmp"
  printf '%s\n' "$tmp"
}

cleanup_files=()
cleanup() {
  local file
  if ((${#cleanup_files[@]})); then
    for file in "${cleanup_files[@]}"; do rm -rf "$file"; done
  fi
}
trap cleanup EXIT INT TERM

# --- operator ----------------------------------------------------------------
cmd_operator() {
  need "$NOMAD"
  local policy token_json
  policy=$(policy_for_namespace "$DIR/box-operator.policy.hcl")
  [[ $policy == "$DIR/box-operator.policy.hcl" ]] || cleanup_files+=("$policy")

  log "applying the box-operator policy to namespace $NS"
  "$NOMAD" acl policy apply -description "box: the coordinator CLI" "box-operator" "$policy"

  if ! $with_token; then
    log "no token created (--no-token); create one with:"
    printf '  nomad acl token create -name %s -policy box-operator\n' "$token_name" >&2
    return 0
  fi

  need python3
  log "creating the ACL token $token_name"
  token_json=$("$NOMAD" acl token create -name "$token_name" -policy box-operator -json)
  python3 - "$token_json" <<'PY'
import json, sys

doc = json.loads(sys.argv[1])
secret = doc.get("SecretID") or doc.get("secret_id")
if not secret:
    raise SystemExit("nomad acl token create returned no SecretID")
print(secret)
PY
  printf '\nstore it in the coordinator environment, not in git:\n' >&2
  printf '  install -m 0600 /dev/null ~/.config/box/env\n' >&2
  printf "  printf 'NOMAD_TOKEN=%%s\\\\n' '<the token above>' >> ~/.config/box/env\n" >&2
  printf 'the token is not shown again; delete it with: nomad acl token delete <accessor id>\n' >&2
}

# --- workloads ---------------------------------------------------------------
box_jobs() {
  # The HTTP API, not `nomad job status -json`: that prints one status document
  # per job and carries no ID, while /v1/jobs is the job list. meta=true asks for
  # the meta block: without it a job whose meta says it is not a box (the publish
  # job box-store, which runs while a publish is going on) would be taken for a
  # box by its prefix and handed the executor credentials.
  curl -sS --max-time 30 -H "X-Nomad-Token: ${NOMAD_TOKEN:-}" \
    "$NOMAD_ADDR/v1/jobs?namespace=$NS&meta=true" | python3 -c '
import json, sys

jobs = json.load(sys.stdin) or []
for job in jobs:
    job_id = job.get("ID") or job.get("id") or ""
    if not job_id.startswith("box-"):
        continue
    # meta=true carries the meta block, so the marker is what decides: a box is
    # meta.box = "true", and a bake or a publish is not a box.
    meta = job.get("Meta") or job.get("meta") or {}
    if str(meta.get("box")).lower() != "true":
        continue
    print(job_id)
'
}

cmd_workloads() {
  need "$NOMAD"
  need curl
  need python3
  [[ -n ${NOMAD_ADDR:-} ]] || die "NOMAD_ADDR is not set"
  local policy job id name
  policy=$(policy_for_namespace "$DIR/box-workload.policy.hcl")
  [[ $policy == "$DIR/box-workload.policy.hcl" ]] || cleanup_files+=("$policy")

  if $all_boxes; then
    while IFS= read -r job; do
      [[ -n $job ]] && box_ids+=("${job#box-}")
    done < <(box_jobs)
  fi
  if ((${#box_ids[@]} == 0)); then
    log "no box id given. The box jobs on this cluster:"
    box_jobs | sed 's/^/  /' >&2 || true
    printf '\nrun again with the box ids, or with --all:\n  %s workloads --all\n' "$0" >&2
    return 2
  fi
  for id in "${box_ids[@]}"; do
    id=${id#box-}
    name="box-workload-$id"
    log "attaching $name to the box job box-$id"
    "$NOMAD" acl policy apply -namespace "$NS" -job "box-$id" \
      -description "box $id: read the executors variable" "$name" "$policy"
  done
  log "a box job created later needs its own attachment: run this command with its id"
}

# --- reap --------------------------------------------------------------------
# The reap pass is a task of the periodic job reap-boxes, and Nomad resolves a
# dispatched child's policies by its parent job id, so one attachment to the
# parent covers every reap-boxes/periodic-<time> child.
cmd_reap() {
  need "$NOMAD"
  local policy
  policy=$(policy_for_namespace "$DIR/box-reap.policy.hcl")
  [[ $policy == "$DIR/box-reap.policy.hcl" ]] || cleanup_files+=("$policy")

  log "applying the box-reap policy to the job reap-boxes in namespace $NS"
  "$NOMAD" acl policy apply -namespace "$NS" -job reap-boxes \
    -description "box reap: delete the disk of a box whose term ran out" \
    box-reap "$policy"
  log "register the job with deploy/reap.sh, and check the attachment with:"
  log "  nomad acl policy info box-reap"
}

# --- clock -------------------------------------------------------------------
# The clock is the one rule a CLI token needs when it cannot carry all of
# box-operator: the stopped-box variable of every box. A token that already has
# policies keeps them -- `nomad acl token update -policy` replaces the whole
# list, so the list is read and every existing name is passed back.
cmd_clock() {
  need "$NOMAD"
  local policy
  policy=$(policy_for_namespace "$DIR/box-clock.policy.hcl")
  [[ $policy == "$DIR/box-clock.policy.hcl" ]] || cleanup_files+=("$policy")

  log "applying the box-clock policy to namespace $NS"
  "$NOMAD" acl policy apply -description "box: the stopped-box clock of every box" box-clock "$policy"

  # Only an explicit --token is an accessor: NOMAD_TOKEN is the management token
  # this command authenticates with, never the token to change.
  if [[ -z $token_accessor ]]; then
    log "no token given (--token ACCESSOR); add the policy to one with:"
    log "  nomad acl token update -policy <its other policies> -policy box-clock <accessor>"
    return 0
  fi

  need python3
  local token_info token_type policies=() name have_clock=false key value
  # `nomad acl token info` has no -json output to rely on: read its stable
  # key = value lines. The Secret ID line holds <none>, never the secret.
  token_info=$("$NOMAD" acl token info "$token_accessor")
  while IFS='=' read -r key value; do
    case $key in
      type) token_type=$value ;;
      policy) policies+=("$value") ;;
    esac
  done < <(printf '%s\n' "$token_info" | python3 -c '
import re, sys

doc = sys.stdin.read()
match = re.search(r"^Type\s*=\s*(\S+)", doc, re.M)
print("type=" + (match.group(1) if match else ""))
policies = re.search(r"^Policies\s*=\s*\[(.*?)\]", doc, re.M)
if policies:
    for name in policies.group(1).split():
        print("policy=" + name)
')
  if [[ $token_type == management ]]; then
    log "the token $token_accessor is a management token; it needs no policy, so nothing was changed"
    return 0
  fi
  for name in "${policies[@]}"; do
    [[ $name == box-clock ]] && have_clock=true
  done
  if $have_clock; then
    log "the token $token_accessor already carries box-clock; its other policies are unchanged"
    return 0
  fi

  log "adding box-clock to the token $token_accessor and keeping its other policies (${#policies[@]})"
  local args=(acl token update)
  for name in "${policies[@]}"; do args+=(-policy "$name"); done
  args+=(-policy box-clock "$token_accessor")
  # The update prints the whole token, and stdout is reserved for the operator
  # token this script may print.
  "$NOMAD" "${args[@]}" >/dev/null
  log "the token $token_accessor now carries box-clock; check it with:"
  log "  $0 check --token <the token's secret> --clock-probe --write-probe"
}

# --- check -------------------------------------------------------------------
fails=0
pass() { printf 'PASS %s\n' "$*"; }
fail() { printf 'FAIL %s\n' "$*"; fails=$((fails + 1)); }

# api METHOD PATH [BODY] -> the HTTP status on stdout
api() {
  local method=$1 path=$2 body=${3:-}
  local args=(-sS -o /dev/null -w '%{http_code}' --max-time 30 -X "$method" -H "X-Nomad-Token: $token" "$NOMAD_ADDR$path")
  [[ -n $body ]] && args+=(-H 'Content-Type: application/json' --data-binary "$body")
  curl "${args[@]}" || true
}

# expect WHAT METHOD PATH EXPECTED [BODY]
expect() {
  local what=$1 method=$2 path=$3 want=$4 body=${5:-} got
  got=$(api "$method" "$path" "$body")
  case " $want " in
    *" $got "*) pass "$what ($got)" ;;
    *) fail "$what: got HTTP $got, want $want" ;;
  esac
}

# check_summary ends a check run: 1 if a probe failed, 0 if every probe passed.
check_summary() {
  if ((fails > 0)); then
    printf '\n%d probe(s) failed\n' "$fails"
    return 1
  fi
  printf '\nall probes passed\n'
}

# check_clock probes a token that carries box-clock instead of box-operator: the
# clock must work, and the executor credentials must stay out of reach. The probe
# id has an underscore, which no box id may have, so it never names a real box.
check_clock() {
  local probe_box="nomad/jobs/box-_acl-probe"
  printf 'checking the box-clock permissions at %s, namespace %s\n' "$NOMAD_ADDR" "$NS"

  expect "box ls, box status, box reap --dry-run: read the clock of a box" GET \
    "/v1/var/$probe_box/stopped?namespace=$NS" "200 404"

  if $write_probe; then
    expect "a reap pass: set the clock of a box" PUT "/v1/var/$probe_box/stopped?namespace=$NS" 200 \
      '{"Items":{"stopped_at":"2026-01-01T00:00:00Z","state":"stopped"}}'
    expect "box ls, box status: read the clock that was written" GET \
      "/v1/var/$probe_box/stopped?namespace=$NS" 200
    expect "box create, box start, box rm: delete the clock of a box" DELETE \
      "/v1/var/$probe_box/stopped?namespace=$NS" "200 204"
  fi

  # The policy is the clock and nothing else: the executor credentials belong to
  # box-operator, and box-clock must not hand them to a narrower token.
  expect "denied: read the executor credentials" GET "/v1/var/nomad/jobs/box-executors?namespace=$NS" 403
  expect "denied: write the executor credentials" PUT "/v1/var/nomad/jobs/box-executors?namespace=$NS" 403 \
    '{"Items":{"k":"v"}}'
}

cmd_check() {
  need curl
  [[ -n ${NOMAD_ADDR:-} ]] || die "NOMAD_ADDR is not set"
  [[ -n $token ]] || die "check needs a token: --token SECRET or NOMAD_TOKEN"
  local node variable_body

  if $clock_probe; then
    check_clock
    check_summary
    return
  fi

  printf 'checking the box-operator permissions at %s, namespace %s\n' "$NOMAD_ADDR" "$NS"

  expect "box ls: list the jobs" GET "/v1/jobs?namespace=$NS" 200
  expect "box capacity: list the nodes" GET "/v1/nodes" 200
  expect "box ls: list the host volumes" GET "/v1/volumes?type=host&namespace=$NS" 200
  expect "box executors show: read the executors variable" GET "/v1/var/nomad/jobs/box-executors?namespace=$NS" "200 404"
  # A box's description and its clock are variables below nomad/jobs/box-<id>.
  # `box create` deletes the clock before it creates anything, so a token that
  # may touch the description but not the clock refuses every new box. The probe
  # id has an underscore, which no box id may have, so it never names a real box.
  local probe_box="nomad/jobs/box-_acl-probe"
  expect "box ls, box start: read the description of a box" GET "/v1/var/$probe_box/spec?namespace=$NS" "200 404"
  expect "box create, box rm: read the clock of a box" GET "/v1/var/$probe_box/stopped?namespace=$NS" "200 404"
  expect "box ls, box status: read the stopped-box term" GET "/v1/var/nomad/jobs/box-reap?namespace=$NS" "200 404"

  node=$(curl -sS --max-time 30 -H "X-Nomad-Token: $token" "$NOMAD_ADDR/v1/nodes" \
    | python3 -c 'import json,sys
nodes = json.load(sys.stdin) or []
print(nodes[0]["ID"] if nodes else "")' 2>/dev/null || true)
  if [[ -n $node ]]; then
    expect "box capacity: read one node" GET "/v1/node/$node" 200
    expect "box capacity: read the node's allocations" GET "/v1/node/$node/allocations" 200
  else
    printf 'SKIP box capacity: there is no node to read\n'
  fi

  # Denied on purpose: the operator runs the CLI, nothing else.
  expect "denied: write an ACL policy" PUT "/v1/acl/policy/acl-probe" 403 \
    '{"Name":"acl-probe","Rules":"node { policy = \"read\" }"}'
  expect "denied: another variable path" PUT "/v1/var/nomad/jobs/other-job?namespace=$NS" 403 '{"Items":{"k":"v"}}'
  expect "denied: another namespace" GET "/v1/jobs?namespace=agent-workers" 403
  if [[ -n $node ]]; then
    expect "denied: node write (drain)" PUT "/v1/node/$node/drain" 403 '{"Drain":true}'
    expect "denied: node write (eligibility)" PUT "/v1/node/$node/eligibility" 403 '{"Eligibility":"ineligible"}'
  fi

  if $write_probe; then
    # The description and the clock of a box nobody can create (see probe_box):
    # written, read and deleted again, as create, start and rm do.
    local kind name
    for kind in spec stopped; do
      name=description
      [[ $kind == stopped ]] && name=clock
      expect "box create, box start: write the $name of a box" PUT "/v1/var/$probe_box/$kind?namespace=$NS" 200 \
        '{"Items":{"acl-probe":"1"}}'
      expect "box rm, box create: delete the $name of a box" DELETE "/v1/var/$probe_box/$kind?namespace=$NS" "200 204"
    done

    # Writes the variable and puts it back (or deletes it if it did not exist).
    # This is the probe that proves `box executors set` works; run it only on a
    # cluster you may touch.
    local tmp status var_path action
    tmp=$(mktemp -d "${TMPDIR:-/tmp}/box-acl-check-XXXXXX")
    cleanup_files+=("$tmp")
    var_path="/v1/var/nomad/jobs/box-executors?namespace=$NS"
    status=$(curl -sS -o "$tmp/var.json" -w '%{http_code}' --max-time 30 \
      -H "X-Nomad-Token: $token" "$NOMAD_ADDR$var_path" || true)
    printf '%s' "$status" >"$tmp/status"
    action=$(python3 - "$tmp" <<'PY'
import json, os, sys

tmp = sys.argv[1]
status = open(os.path.join(tmp, "status")).read().strip()
doc = {}
if status == "200":
    doc = json.load(open(os.path.join(tmp, "var.json")))
items = dict(doc.get("Items") or {})
probe = dict(items)
probe["acl-probe"] = "1"
json.dump({"Items": probe, "Namespace": doc.get("Namespace") or ""}, open(os.path.join(tmp, "probe.json"), "w"))
if status == "200":
    json.dump({"Items": items, "Namespace": doc.get("Namespace") or ""}, open(os.path.join(tmp, "restore.json"), "w"))
    print("put")
else:
    print("delete")
PY
)
    variable_body=$(cat "$tmp/probe.json")
    expect "box executors set: write the executors variable" PUT "$var_path" 200 "$variable_body"
    case $action in
      put)
        got=$(api PUT "$var_path" "$(cat "$tmp/restore.json")")
        if [[ $got == 200 ]]; then
          pass "restored the executors variable as it was"
        else
          fail "restoring the executors variable: got HTTP $got"
        fi
        ;;
      delete)
        got=$(api DELETE "$var_path")
        case $got in
          200 | 204) pass "removed the probe variable again" ;;
          *) fail "removing the probe variable: got HTTP $got" ;;
        esac
        ;;
    esac
  fi

  check_summary
}

case $command in
  operator) cmd_operator ;;
  workloads) cmd_workloads ;;
  reap) cmd_reap ;;
  clock) cmd_clock ;;
  check) cmd_check ;;
esac
