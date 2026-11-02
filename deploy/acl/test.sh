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

log() { printf '\n==> %s\n' "$*" >&2; }
die() { printf 'acl-test: %s\n' "$*" >&2; exit 1; }
fails=0
pass() { printf 'PASS %s\n' "$*"; }
fail() { printf 'FAIL %s\n' "$*"; fails=$((fails + 1)); }

find_nomad() {
  if [[ -n ${BOX_TEST_NOMAD:-} ]]; then printf '%s\n' "$BOX_TEST_NOMAD"; return; fi
  if command -v nomad >/dev/null 2>&1; then command -v nomad; return; fi
  local found
  found=$(ls -d /nix/store/*nomad*/bin/nomad 2>/dev/null | head -1 || true)
  [[ -n $found ]] && { printf '%s\n' "$found"; return; }
  die "nomad not found: set BOX_TEST_NOMAD"
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
    echo $$ >"$WORK/agent.pid"
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

  if ((fails > 0)); then
    printf '\n%d check(s) failed\n' "$fails"
    return 1
  fi
  printf '\nall ACL checks passed\n'
}

main "$@"
