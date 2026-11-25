#!/usr/bin/env bash
# deploy/acl/test.sh -- prove the box ACL policies against a real Nomad agent.
#
# It starts a throwaway `nomad agent -dev -acl-enabled` on ports of its own
# (24666-24668), bootstraps ACLs, applies the policies with deploy/acl/apply.sh
# and probes them: what the `box` CLI needs is allowed, what it does not need is
# denied, and the box-workload policy is attached to a box job. Nothing here
# talks to any other cluster, and the agent is stopped by PID at the end.
#
# Requirements: the nomad binary (BOX_TEST_NOMAD, PATH, or a nix store copy),
# curl, python3.
#
# Environment:
#   BOX_TEST_NOMAD   nomad binary
#   KEEP=1           keep the work directory
#
# Exit code 0 means every check passed.

set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
WORK=${BOX_ACL_WORK:-$(mktemp -d "${TMPDIR:-/tmp}/box-acl-test.XXXXXX")}
KEEP=${KEEP:-0}
HTTP=24666
RPC=24667
SERF=24668
ADDR=http://127.0.0.1:$HTTP
# The test agent's namespace: the policies are written for it.
NS=default

log() { printf '\n==> %s\n' "$*" >&2; }
die() { printf 'acl-test: %s\n' "$*" >&2; exit 1; }
fails=0
pass() { printf 'PASS %s\n' "$*"; }
fail() { printf 'FAIL %s\n' "$*"; fails=$((fails + 1)); }

# api METHOD PATH [BODY] -> the HTTP status on stdout, with the token in $token
api() {
  local method=$1 path=$2 body=${3:-}
  local args=(-sS -o /dev/null -w '%{http_code}' --max-time 30 -X "$method" -H "X-Nomad-Token: $token" "$ADDR$path")
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

find_nomad() {
  if [[ -n ${BOX_TEST_NOMAD:-} ]]; then printf '%s\n' "$BOX_TEST_NOMAD"; return; fi
  if command -v nomad >/dev/null 2>&1; then command -v nomad; return; fi
  local found
  found=$(ls -d /nix/store/*nomad*/bin/nomad 2>/dev/null | head -1 || true)
  [[ -n $found ]] && { printf '%s\n' "$found"; return; }
  die "nomad not found: set BOX_TEST_NOMAD"
}

# probe_job NAME writes the job file of a batch task whose template renders the
# image store's url into its secrets, and which prints that file. The job's own id
# is what decides whether Nomad grants the read, so the same job body probes a
# publish (box-store) and a box (box-dev1).
probe_job() {
  local name=$1
  cat >"$WORK/probe-$name.nomad.hcl" <<EOF
job "$name" {
  type = "batch"

  group "publish" {
    task "box" {
      driver = "raw_exec"

      # One attempt: a task that cannot render the variable says so at once
      # instead of restarting into the same refusal.
      restart {
        attempts = 0
      }

      config {
        command = "/bin/sh"
        args    = ["-c", "cat \${NOMAD_SECRETS_DIR}/store"]
      }

      template {
        destination = "secrets/store"
        data        = <<EOT
{{- with nomadVar "nomad/jobs/box-store" -}}{{- (index . "url").Value -}}{{- end -}}
EOT
      }
    }
  }
}
EOF
}

# probe_store_reads runs the probe job NAME, and reports whether its task read the
# store: the task ends complete and prints the url its template rendered.
probe_store_reads() {
  local name=$1 alloc deadline log
  probe_start "$name" || return 1
  deadline=$((SECONDS + 120))
  while :; do
    alloc=$(alloc_of_job "$name")
    if [[ -n $alloc ]] && [[ $(alloc_client_status "$alloc") == complete ]]; then
      log=$("$NOMAD" alloc logs "$alloc" 2>/dev/null || true)
      [[ $log == *https://store.example.com* ]]
      return
    fi
    if ((SECONDS >= deadline)); then
      printf 'the probe job %s did not read the store in 120s\n' "$name" >&2
      return 1
    fi
    sleep 1
  done
}

# probe_store_denied runs the probe job NAME, and reports whether its identity was
# refused the store. Nomad does not fail such a task at once: the template that
# cannot read the variable blocks it with an event that names the variable as
# missing, and retries. A variable that exists (the publish probe just read it)
# can only be "missing" for an identity that was refused it, and the task prints
# nothing.
probe_store_denied() {
  local name=$1 alloc deadline message log
  probe_start "$name" || return 1
  deadline=$((SECONDS + 120))
  while :; do
    alloc=$(alloc_of_job "$name")
    if [[ -n $alloc ]]; then
      if [[ $(alloc_client_status "$alloc") == complete ]]; then
        # A task that ends complete rendered the template, so it read the store:
        # the answer is in, and the template's message cannot say otherwise.
        # Waiting for that message would wait out the whole timeout.
        return 1
      fi
      message=$(alloc_template_message "$alloc")
      if [[ $message == *nomad/jobs/box-store* ]]; then
        log=$("$NOMAD" alloc logs "$alloc" 2>/dev/null || true)
        [[ $log != *https://store.example.com* ]]
        return
      fi
    fi
    if ((SECONDS >= deadline)); then
      printf 'the probe job %s did not report the store in 120s\n' "$name" >&2
      return 1
    fi
    sleep 1
  done
}

# probe_start registers the probe job NAME; false when Nomad refuses the job.
probe_start() {
  local name=$1
  if "$NOMAD" job run -detach "$WORK/probe-$name.nomad.hcl" >"$WORK/probe-$name.log" 2>&1; then
    return 0
  fi
  tail -5 "$WORK/probe-$name.log" >&2
  return 1
}

# alloc_of_job JOB prints the id of the job's newest allocation, once there is one.
alloc_of_job() {
  curl -sS --max-time 30 -H "X-Nomad-Token: $mgmt" "$ADDR/v1/job/$1/allocations" | python3 -c '
import json, sys

newest = None
for alloc in json.load(sys.stdin) or []:
    if newest is None or (alloc.get("CreateIndex") or 0) >= (newest.get("CreateIndex") or 0):
        newest = alloc
if newest:
    print(newest["ID"])
'
}

# alloc_client_status ALLOC prints the client status of an allocation.
alloc_client_status() {
  curl -sS --max-time 30 -H "X-Nomad-Token: $mgmt" "$ADDR/v1/allocation/$1" | python3 -c '
import json, sys
print(json.load(sys.stdin).get("ClientStatus", ""))
'
}

# alloc_template_message ALLOC prints the message of the task's last template event.
alloc_template_message() {
  curl -sS --max-time 30 -H "X-Nomad-Token: $mgmt" "$ADDR/v1/allocation/$1" | python3 -c '
import json, sys

doc = json.load(sys.stdin)
for state in (doc.get("TaskStates") or {}).values():
    for event in reversed(state.get("Events") or []):
        if event.get("Type") == "Template":
            print(event.get("DisplayMessage") or "")
            raise SystemExit
'
}

cleanup() {
  local status=$?
  set +e
  if [[ -f $WORK/agent.pid ]]; then
    kill -9 "$(cat "$WORK/agent.pid")" 2>/dev/null
  fi
  if [[ $KEEP == 1 ]]; then
    log "kept $WORK (KEEP=1)"
  else
    rm -rf "$WORK"
  fi
  exit $status
}
trap cleanup EXIT

main() {
  command -v curl >/dev/null 2>&1 || die "curl is required"
  command -v python3 >/dev/null 2>&1 || die "python3 is required"
  NOMAD=$(find_nomad)
  mkdir -p "$WORK/data"

  if curl -sf -o /dev/null --max-time 1 "$ADDR/v1/status/leader"; then
    die "another agent already answers on $ADDR; stop it first"
  fi

  cat >"$WORK/agent.hcl" <<EOF
data_dir = "$WORK/data"
bind_addr = "127.0.0.1"
log_level = "WARN"

ports {
  http = $HTTP
  rpc  = $RPC
  serf = $SERF
}

acl {
  enabled = true
}
EOF

  log "starting nomad agent -dev -acl-enabled on $ADDR"
  (
    # $BASHPID, not $$: in a subshell $$ is the script, and the agent would
    # outlive the test on its port.
    echo "$BASHPID" >"$WORK/agent.pid"
    exec "$NOMAD" agent -dev -config "$WORK/agent.hcl" >>"$WORK/agent.log" 2>&1
  ) &
  local deadline=$((SECONDS + 60))
  until curl -sf -o /dev/null --max-time 1 "$ADDR/v1/status/leader"; do
    ((SECONDS < deadline)) || die "the agent did not start; see $WORK/agent.log"
    sleep 1
  done

  log "bootstrapping ACLs"
  local bootstrap mgmt
  bootstrap=$("$NOMAD" acl bootstrap -address="$ADDR" -json)
  mgmt=$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])["SecretID"])' "$bootstrap")
  [[ -n $mgmt ]] || die "acl bootstrap returned no SecretID"

  export NOMAD_ADDR=$ADDR
  export NOMAD_TOKEN=$mgmt
  export NOMAD=$NOMAD

  log "applying the box-operator policy and creating its token"
  local out token
  out=$("$ROOT/deploy/acl/apply.sh" operator 2>"$WORK/apply.log") || {
    cat "$WORK/apply.log" >&2
    die "apply.sh operator failed"
  }
  token=$(printf '%s\n' "$out" | tail -1)
  [[ -n $token ]] || die "apply.sh printed no token"

  if "$NOMAD" acl policy info box-operator >/dev/null 2>&1; then
    pass "box-operator policy is on the cluster"
  else
    fail "box-operator policy is missing"
  fi

  log "probing the operator permissions"
  if "$ROOT/deploy/acl/apply.sh" check --token "$token" --write-probe; then
    pass "every operator probe behaved as specified"
  else
    fail "an operator probe behaved differently than specified"
  fi

  # `box create` submits the box job and `box rm` deregisters it: that is the
  # one write the probes above cannot do without leaving a job behind.
  log "registering a job with the operator token"
  cat >"$WORK/box-test.nomad.hcl" <<'EOF'
job "box-test" {
  type = "service"

  meta {
    box    = "true"
    box_id = "test"
  }

  # Nothing matches: the job is registered and never placed, which is all this
  # test needs.
  constraint {
    attribute = "${attr.kernel.name}"
    value     = "no-such-kernel"
  }

  group "box" {
    task "box" {
      driver = "box"
      config {
        box_id = "test"
      }
    }
  }
}
EOF
  if "$NOMAD" job run -detach "$WORK/box-test.nomad.hcl" >"$WORK/job-run.log" 2>&1; then
    pass "the operator can submit a job (box create, start, stop, resize)"
  else
    fail "the operator cannot submit a job: $(tail -3 "$WORK/job-run.log")"
  fi
  if "$NOMAD" job status box-test >/dev/null 2>&1; then
    pass "the operator can read a job (box status, box ls)"
  else
    fail "the operator cannot read the job it registered"
  fi

  log "attaching the box-workload policy to box-test"
  if "$ROOT/deploy/acl/apply.sh" workloads box-test >"$WORK/workloads.log" 2>&1; then
    pass "apply.sh workloads applied to box-test"
  else
    tail -5 "$WORK/workloads.log" >&2
    fail "apply.sh workloads failed"
  fi
  local info
  info=$(curl -sS --max-time 30 -H "X-Nomad-Token: $mgmt" "$ADDR/v1/acl/policy/box-workload-test" 2>/dev/null || true)
  if python3 -c '
import json, sys
doc = json.loads(sys.argv[1])
acl = doc.get("JobACL") or {}
sys.exit(0 if acl.get("JobID") == "box-test" and acl.get("Namespace") == "default" else 1)
' "$info" 2>/dev/null; then
    pass "box-workload-test is attached to the job box-test in the default namespace"
  else
    fail "box-workload-test is not attached to box-test: $info"
  fi

  # `--all` finds the same box job by its meta marker.
  if "$ROOT/deploy/acl/apply.sh" workloads --all >"$WORK/workloads-all.log" 2>&1; then
    pass "apply.sh workloads --all finds the box job by meta.box"
  else
    tail -5 "$WORK/workloads-all.log" >&2
    fail "apply.sh workloads --all failed"
  fi

  if "$NOMAD" job stop -purge box-test >/dev/null 2>&1; then
    pass "the operator can deregister a job (box rm)"
  else
    fail "the operator cannot deregister a job"
  fi

  # The cluster's stopped-box term is a variable the operator sets and the pass
  # reads: write it with the operator token, so the reap probe below has one to
  # read and a write of it to be refused.
  log "writing the stopped-box term with the operator token"
  expect "operator: write the stopped-box term" PUT "/v1/var/nomad/jobs/box-reap?namespace=$NS" 200 \
    '{"Items":{"stopped_term":"48h"}}'
  expect "operator: read the stopped-box term" GET "/v1/var/nomad/jobs/box-reap?namespace=$NS" 200

  log "applying the box-reap policy to the job reap-boxes"
  if "$ROOT/deploy/acl/apply.sh" reap >"$WORK/reap.log" 2>&1; then
    pass "apply.sh reap applied the policy"
  else
    tail -5 "$WORK/reap.log" >&2
    fail "apply.sh reap failed"
  fi
  local reap_info
  reap_info=$(curl -sS --max-time 30 -H "X-Nomad-Token: $mgmt" "$ADDR/v1/acl/policy/box-reap" 2>/dev/null || true)
  if python3 -c '
import json, sys
doc = json.loads(sys.argv[1])
acl = doc.get("JobACL") or {}
sys.exit(0 if acl.get("JobID") == "reap-boxes" and not acl.get("Group") and not acl.get("Task") else 1)
' "$reap_info" 2>/dev/null; then
    pass "the box-reap policy is attached to the whole job reap-boxes, so every child gets it"
  else
    fail "the box-reap policy is not attached to the job reap-boxes: $reap_info"
  fi

  # The pass runs as the job's workload identity, but its policy is an ordinary
  # set of rules: a token that carries it proves the same grant on the HTTP API.
  log "probing the permissions of the box-reap policy"
  local reap_token reap_accessor operator_token reap_token_json
  operator_token=$token
  reap_token_json=$("$NOMAD" acl token create -name box-reap-probe -policy box-reap -json)
  reap_token=$(python3 -c 'import json,sys; print(json.load(sys.stdin)["SecretID"])' <<<"$reap_token_json")
  reap_accessor=$(python3 -c 'import json,sys; print(json.load(sys.stdin)["AccessorID"])' <<<"$reap_token_json")
  token=$reap_token
  expect "reap: list the jobs" GET "/v1/jobs?namespace=$NS" 200
  expect "reap: list the allocations" GET "/v1/allocations?namespace=$NS" 200
  expect "reap: read a node" GET "/v1/nodes" 200
  expect "reap: list the host volumes" GET "/v1/volumes?type=host&namespace=$NS" 200
  expect "reap: write the clock of a box" PUT "/v1/var/nomad/jobs/box-reap-probe/stopped?namespace=$NS" 200 \
    '{"Items":{"stopped_at":"2026-01-01T00:00:00Z","state":"stopped"}}'
  expect "reap: read the clock of a box" GET "/v1/var/nomad/jobs/box-reap-probe/stopped?namespace=$NS" 200
  expect "reap: delete the clock of a box" DELETE "/v1/var/nomad/jobs/box-reap-probe/stopped?namespace=$NS" "200 204"
  expect "reap: delete the description of a box" DELETE "/v1/var/nomad/jobs/box-reap-probe/spec?namespace=$NS" "200 204 404"
  expect "reap: read the stopped-box term" GET "/v1/var/nomad/jobs/box-reap?namespace=$NS" 200
  expect "denied: reap writes the stopped-box term" PUT "/v1/var/nomad/jobs/box-reap?namespace=$NS" 403 \
    '{"Items":{"stopped_term":"1m"}}'
  expect "reap: purge a job that is already over" DELETE "/v1/job/box-reap-probe?purge=true&namespace=$NS" "200 404"
  expect "denied: write the executor credentials" PUT "/v1/var/nomad/jobs/box-executors?namespace=$NS" 403 \
    '{"Items":{"k":"v"}}'
  expect "denied: read the executor credentials" GET "/v1/var/nomad/jobs/box-executors?namespace=$NS" 403
  expect "denied: write another job's clock" PUT "/v1/var/nomad/jobs/other-job/stopped?namespace=$NS" 403 \
    '{"Items":{"stopped_at":"2026-01-01T00:00:00Z"}}'
  token=$operator_token
  "$NOMAD" acl token delete -accessor "$reap_accessor" >/dev/null 2>&1 || true
  expect "operator: delete the stopped-box term" DELETE "/v1/var/nomad/jobs/box-reap?namespace=$NS" "200 204"

  # The clock policy is for a CLI token that cannot carry all of box-operator.
  # It is one variable rule, so `apply.sh clock` must apply it, and the token
  # must gain it without losing the policies it already has.
  log "applying the box-clock policy"
  if "$ROOT/deploy/acl/apply.sh" clock >"$WORK/clock.log" 2>&1; then
    pass "apply.sh clock applied the policy"
  else
    tail -5 "$WORK/clock.log" >&2
    fail "apply.sh clock failed"
  fi
  if "$NOMAD" acl policy info box-clock >/dev/null 2>&1; then
    pass "box-clock policy is on the cluster"
  else
    fail "box-clock policy is missing"
  fi

  log "attaching box-clock to a token that already has box-reap"
  local clock_json clock_accessor clock_token clock_info
  clock_json=$("$NOMAD" acl token create -name box-clock-probe -policy box-reap -json)
  clock_accessor=$(python3 -c 'import json,sys; print(json.load(sys.stdin)["AccessorID"])' <<<"$clock_json")
  clock_token=$(python3 -c 'import json,sys; print(json.load(sys.stdin)["SecretID"])' <<<"$clock_json")
  if "$ROOT/deploy/acl/apply.sh" clock --token "$clock_accessor" >"$WORK/clock-token.log" 2>&1; then
    pass "apply.sh clock --token attached the policy"
  else
    tail -5 "$WORK/clock-token.log" >&2
    fail "apply.sh clock --token failed"
  fi
  clock_info=$(curl -sS --max-time 30 -H "X-Nomad-Token: $mgmt" "$ADDR/v1/acl/token/$clock_accessor")
  if python3 -c '
import json, sys
policies = sorted(json.loads(sys.argv[1]).get("Policies") or [])
sys.exit(0 if policies == ["box-clock", "box-reap"] else 1)
' "$clock_info"; then
    pass "the token kept box-reap and gained box-clock"
  else
    fail "the token's policies are not box-clock and box-reap: $clock_info"
  fi

  # A second run must not duplicate the policy: updating a token replaces its
  # policy list, so the mode has to read the list it already has.
  if "$ROOT/deploy/acl/apply.sh" clock --token "$clock_accessor" >"$WORK/clock-token2.log" 2>&1; then
    clock_info=$(curl -sS --max-time 30 -H "X-Nomad-Token: $mgmt" "$ADDR/v1/acl/token/$clock_accessor")
    if python3 -c '
import json, sys
policies = sorted(json.loads(sys.argv[1]).get("Policies") or [])
sys.exit(0 if policies == ["box-clock", "box-reap"] else 1)
' "$clock_info"; then
      pass "a second apply.sh clock changed nothing on the token"
    else
      fail "a second apply.sh clock changed the token's policies: $clock_info"
    fi
  else
    tail -5 "$WORK/clock-token2.log" >&2
    fail "the second apply.sh clock --token failed"
  fi

  log "probing a token with the box-clock policy"
  if "$ROOT/deploy/acl/apply.sh" check --token "$clock_token" --clock-probe --write-probe; then
    pass "the box-clock probes behaved as specified"
  else
    fail "a box-clock probe behaved differently than specified"
  fi
  "$NOMAD" acl token delete "$clock_accessor" >/dev/null 2>&1 || true

  # --- the publish job's own identity -----------------------------------------
  # Every publish is the job box-store, which is the name of the variable the task
  # reads its store from. Nomad gives a workload identity read and list on
  # nomad/jobs/<its own job id> (acl.matchingVariablesCapabilitySet), so the
  # publish job reads that variable with no ACL policy at all -- that is the whole
  # reason the job has one name for every version. A box (box-<id>) is not named
  # after the variable and cannot read it, whichever policy is attached to it.
  #
  # These probes are real tasks of this agent, and the task's own template is what
  # decides: it renders the store's url into its secrets, and the task prints it.
  log "probing the publish job's identity against the image store variable"
  # The operator writes the store the publish reads: that is its own grant, not
  # the management token's. $NOMAD and apply.sh keep NOMAD_TOKEN=$mgmt.
  token=$operator_token
  expect "operator: write the image store the publish job reads" PUT "/v1/var/nomad/jobs/box-store?namespace=$NS" 200 \
    '{"Items":{"url":"https://store.example.com"}}'

  probe_job box-store
  if probe_store_reads box-store; then
    pass "the publish job box-store reads nomad/jobs/box-store with no ACL policy"
  else
    # The design rests on this one fact and the code has no fallback: a cluster
    # that does not grant it needs the plan's alternative (the job box-publish
    # with one policy of its own), not a publish that renders an empty store.
    tail -20 "$WORK/agent.log" >&2 || true
    die "the job box-store cannot read nomad/jobs/box-store without a policy: the publish design does not hold on this Nomad; see $WORK/agent.log"
  fi

  # The publish job's id starts with box-, so `workloads --all` must tell it from
  # a box by meta.box or it would attach the executor credentials to an upload.
  # A real box job is registered beside it, so the run has one box to apply and
  # still creates no policy for the store.
  log "checking apply.sh workloads --all beside the publish job box-store"
  if "$NOMAD" job run -detach "$WORK/box-test.nomad.hcl" >"$WORK/box-test-store.log" 2>&1 \
    && "$ROOT/deploy/acl/apply.sh" workloads --all >"$WORK/workloads-box-store.log" 2>&1; then
    pass "apply.sh workloads --all ran beside the publish job box-store"
  else
    tail -5 "$WORK/box-test-store.log" >&2 || true
    tail -5 "$WORK/workloads-box-store.log" >&2 || true
    fail "apply.sh workloads --all failed beside the publish job box-store"
  fi
  if [[ $(curl -sS -o /dev/null -w '%{http_code}' --max-time 30 -H "X-Nomad-Token: $mgmt" \
    "$ADDR/v1/acl/policy/box-workload-store") == 404 ]]; then
    pass "box-workload-store is not created for the publish job box-store"
  else
    fail "box-workload-store was created for the publish job box-store"
  fi
  "$NOMAD" job stop -purge box-test >/dev/null 2>&1 || true

  probe_job box-dev1
  if probe_store_denied box-dev1; then
    pass "a box job (box-dev1) cannot read nomad/jobs/box-store"
  else
    fail "the box job box-dev1 read the image store"
  fi

  log "attaching the box-workload policy to box-dev1"
  if "$ROOT/deploy/acl/apply.sh" workloads box-dev1 >"$WORK/probe-workload.log" 2>&1; then
    pass "apply.sh workloads attached the policy to box-dev1"
  else
    tail -5 "$WORK/probe-workload.log" >&2
    fail "apply.sh workloads box-dev1 failed"
  fi
  "$NOMAD" job stop -purge box-dev1 >/dev/null 2>&1 || true
  if probe_store_denied box-dev1; then
    pass "the box-workload policy does not hand the image store to a box"
  else
    fail "a box with the box-workload policy read the image store"
  fi

  "$NOMAD" job stop -purge box-store >/dev/null 2>&1 || true
  "$NOMAD" job stop -purge box-dev1 >/dev/null 2>&1 || true
  # The probe tasks hold their secrets mount until the client unmounts it, and
  # $WORK holds the agent's data dir: a moment here keeps the last rm quiet.
  sleep 2
  token=$operator_token
  expect "operator: delete the image store" DELETE "/v1/var/nomad/jobs/box-store?namespace=$NS" "200 204"

  if ((fails > 0)); then
    printf '\n%d check(s) failed\n' "$fails"
    return 1
  fi
  printf '\nall ACL checks passed\n'
}

main "$@"
