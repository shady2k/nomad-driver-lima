#!/usr/bin/env bash
# deploy/acl/apply.sh -- put the box ACL policies on a cluster.
#
# Usage:
#   deploy/acl/apply.sh operator  [--token-name NAME] [--no-token] [--namespace NS]
#   deploy/acl/apply.sh workloads [BOX ...] [--all] [--namespace NS]
#   deploy/acl/apply.sh reap      [--namespace NS]
#   deploy/acl/apply.sh check     [--token SECRET] [--write-probe] [--namespace NS]
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
#   check       run the permission probes with a token: one PASS or FAIL per
#               probe, exit 1 if one fails. Without --token it uses NOMAD_TOKEN.
#
# Environment: NOMAD_ADDR (and NOMAD_TOKEN for operator and workloads: both need
# a management token), NOMAD (the nomad binary, default `nomad`), NS (default
# namespace, default `default`).
#
# Nothing here prints a secret except the token of `operator`, once, on stdout.

set -euo pipefail

DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
NOMAD=${NOMAD:-nomad}
PROG=${0##*/}
die() { printf '%s: %s\n' "$PROG" "$*" >&2; exit 2; }
log() { printf '%s: %s\n' "$PROG" "$*" >&2; }
need() { command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"; }

usage() { sed -n '2,26p' "$0" | sed 's/^# \{0,1\}//'; }

NS=${NS:-default}
token_name=box-operator
with_token=true
all_boxes=false
box_ids=()
token=${NOMAD_TOKEN:-}
write_probe=false

command=${1:-}
shift || true
case $command in
  "" | -h | --help | help) usage; exit 0 ;;
  operator | workloads | reap | check) ;;
  *) die "unknown subcommand '$command' (want operator, workloads, reap or check; --help shows the usage)" ;;
esac

while (($# > 0)); do
  case $1 in
    --token-name) (($# >= 2)) || die "--token-name needs a name"; token_name=$2; shift 2 ;;
    --no-token) with_token=false; shift ;;
    --namespace) (($# >= 2)) || die "--namespace needs a namespace"; NS=$2; shift 2 ;;
    --all) all_boxes=true; shift ;;
    --token) (($# >= 2)) || die "--token needs the secret"; token=$2; shift 2 ;;
    --write-probe) write_probe=true; shift ;;
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
  # per job and carries no ID, while /v1/jobs is the job list.
  curl -sS --max-time 30 -H "X-Nomad-Token: ${NOMAD_TOKEN:-}" \
    "$NOMAD_ADDR/v1/jobs?namespace=$NS" | python3 -c '
import json, sys

jobs = json.load(sys.stdin) or []
for job in jobs:
    job_id = job.get("ID") or job.get("id") or ""
    if not job_id.startswith("box-"):
        continue
    # The job list does not always carry the meta block, so the prefix is the
    # filter (box-<id> is what `box create` writes) and the marker is checked
    # when it is there.
    meta = job.get("Meta") or job.get("meta") or {}
    if meta and str(meta.get("box")).lower() != "true":
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
    -description "box reap: delete the disk of a box that stopped 24 hours ago" \
    box-reap "$policy"
  log "register the job with deploy/reap.sh, and check the attachment with:"
  log "  nomad acl policy info box-reap"
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

cmd_check() {
  need curl
  [[ -n ${NOMAD_ADDR:-} ]] || die "NOMAD_ADDR is not set"
  [[ -n $token ]] || die "check needs a token: --token SECRET or NOMAD_TOKEN"
  local node variable_body

  printf 'checking the box-operator permissions at %s, namespace %s\n' "$NOMAD_ADDR" "$NS"

  expect "box ls: list the jobs" GET "/v1/jobs?namespace=$NS" 200
  expect "box capacity: list the nodes" GET "/v1/nodes" 200
  expect "box ls: list the host volumes" GET "/v1/volumes?type=host&namespace=$NS" 200
  expect "box executors show: read the executors variable" GET "/v1/var/nomad/jobs/box-executors?namespace=$NS" "200 404"

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

  if ((fails > 0)); then
    printf '\n%d probe(s) failed\n' "$fails"
    return 1
  fi
  printf '\nall probes passed\n'
}

case $command in
  operator) cmd_operator ;;
  workloads) cmd_workloads ;;
  reap) cmd_reap ;;
  check) cmd_check ;;
esac
