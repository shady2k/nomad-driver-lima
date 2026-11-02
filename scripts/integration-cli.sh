#!/usr/bin/env bash
# Integration harness for the `box` CLI (phase 4).
#
# It starts its own Nomad server and client on ports of its own (the phase 2
# suite uses 14646-14658 and may run at the same time), with its own data
# directory, its own LIMA_HOME and its own host volume directory, builds the
# two plugins and the CLI, and runs integration/cli_test.go against real Lima
# VMs on this host.
#
# Everything this run creates (VMs, volumes, image cache, Nomad data) is removed
# at the end unless KEEP=1. Nothing here talks to any other cluster.
#
# Requirements: limactl 2.x, qemu with /dev/kvm, python3, curl.
#
# Environment:
#   BOX_CLI_WORK       work directory (default <repo>/.integration-cli)
#   BOX_CLI_KEEP=1     keep the work directory after the run
#   BOX_TEST_IMAGE     Ubuntu minimal cloud image (default /tmp/box-images/ubuntu.img,
#                      downloaded when missing)
#   BOX_TEST_LIMACTL   limactl binary (default: PATH, then a nix store lima 2.x)
#   BOX_TEST_NOMAD     nomad binary (default: work dir, then a nix store zip, then download)
#   GO                 go binary (default: go)

set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
WORK=${BOX_CLI_WORK:-$ROOT/.integration-cli}
KEEP=${BOX_CLI_KEEP:-0}
GO=${GO:-go}
# The unprivileged user the box driver runs Lima as: the client of this run runs
# as the current user, so that is the user (a root client names its own).
RUNTIME_USER=${BOX_TEST_RUNTIME_USER:-$(id -un)}

# Own ports: the phase 2 suite owns 14646-14658.
SERVER_HTTP=24646
SERVER_RPC=24647
SERVER_SERF=24648
CLIENT_HTTP=24656
CLIENT_RPC=24657
CLIENT_SERF=24658
ADDR=http://127.0.0.1:$SERVER_HTTP

IMAGE_URL=${BOX_TEST_IMAGE_URL:-https://cloud-images.ubuntu.com/minimal/releases/noble/release/ubuntu-24.04-minimal-cloudimg-amd64.img}

log() { printf '\n==> %s\n' "$*" >&2; }
die() { printf 'integration-cli: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"; }

find_limactl() {
  if [[ -n "${BOX_TEST_LIMACTL:-}" ]]; then printf '%s\n' "$BOX_TEST_LIMACTL"; return; fi
  if command -v limactl >/dev/null 2>&1; then command -v limactl; return; fi
  local found
  found=$(ls -d /nix/store/*-lima-2*/bin/limactl 2>/dev/null | head -1 || true)
  [[ -n $found ]] && { printf '%s\n' "$found"; return; }
  die "limactl not found: install lima 2.x or set BOX_TEST_LIMACTL"
}

extract_nomad() {
  local zip=$1 dest=$2
  mkdir -p "$dest"
  python3 - "$zip" "$dest/nomad" <<'PY'
import os, sys, zipfile
zip_path, out = sys.argv[1], sys.argv[2]
with zipfile.ZipFile(zip_path) as z:
    member = next(n for n in z.namelist() if os.path.basename(n) == "nomad")
    with open(out, "wb") as f:
        f.write(z.read(member))
os.chmod(out, 0o755)
PY
}

find_nomad() {
  if [[ -n "${BOX_TEST_NOMAD:-}" ]]; then printf '%s\n' "$BOX_TEST_NOMAD"; return; fi
  if [[ -x $WORK/nomad/nomad ]]; then printf '%s\n' "$WORK/nomad/nomad"; return; fi
  if command -v nomad >/dev/null 2>&1; then command -v nomad; return; fi
  local found
  found=$(ls -d /nix/store/*nomad-official-2.0.7/bin/nomad 2>/dev/null | head -1 || true)
  [[ -n $found ]] && { printf '%s\n' "$found"; return; }
  local zip
  zip=$(ls /nix/store/*nomad_2.0.7_linux_amd64.zip 2>/dev/null | head -1 || true)
  if [[ -z $zip ]]; then
    log "downloading Nomad 2.0.7"
    zip=$WORK/nomad.zip
    curl -fsSL --retry 3 -o "$zip" https://releases.hashicorp.com/nomad/2.0.7/nomad_2.0.7_linux_amd64.zip
  fi
  extract_nomad "$zip" "$WORK/nomad"
  printf '%s\n' "$WORK/nomad/nomad"
}

fetch_image() {
  if [[ -n "${BOX_TEST_IMAGE:-}" && -s "${BOX_TEST_IMAGE}" ]]; then
    printf '%s\n' "$BOX_TEST_IMAGE"
    return
  fi
  if [[ -s /tmp/box-images/ubuntu.img ]]; then printf '%s\n' /tmp/box-images/ubuntu.img; return; fi
  local image=$WORK/images/ubuntu-24.04-minimal.img
  if [[ ! -s $image ]]; then
    log "downloading the Ubuntu 24.04 minimal cloud image (about 250 MB)"
    mkdir -p "$WORK/images"
    curl -fL --retry 3 -o "$image.part" "$IMAGE_URL"
    mv "$image.part" "$image"
  fi
  printf '%s\n' "$image"
}

write_server_config() {
  cat > "$WORK/server.hcl" <<EOF
data_dir   = "$WORK/data/server"
bind_addr  = "127.0.0.1"
log_level  = "INFO"

advertise {
  http = "127.0.0.1"
  rpc  = "127.0.0.1"
  serf = "127.0.0.1"
}

ports {
  http = $SERVER_HTTP
  rpc  = $SERVER_RPC
  serf = $SERVER_SERF
}

server {
  enabled          = true
  bootstrap_expect = 1
}

client {
  enabled = false
}
EOF
}

write_client_config() {
  mkdir -p "$WORK/data/client"
  sed -e "s|__DATA_DIR__|$WORK/data/client|g" \
      -e "s|__PLUGIN_DIR__|$WORK/bin/plugins|g" \
      -e "s|__HTTP_PORT__|$CLIENT_HTTP|g" \
      -e "s|__RPC_PORT__|$CLIENT_RPC|g" \
      -e "s|__SERF_PORT__|$CLIENT_SERF|g" \
      -e "s|__SERVER_RPC_PORT__|$SERVER_RPC|g" \
      -e "s|__VOLUMES_DIR__|$WORK/volumes|g" \
      -e "s|__DHV_PLUGIN_DIR__|$WORK/dhv|g" \
      -e "s|__LIMACTL__|$LIMACTL|g" \
      -e "s|__LIMA_HOME__|$LIMA_HOME|g" \
      -e "s|__IMAGE_CACHE__|$WORK/images/cache|g" \
      -e "s|__RUNTIME_USER__|$RUNTIME_USER|g" \
      "$ROOT/examples/client.hcl" > "$WORK/client.hcl"
}

write_start_scripts() {
  cat > "$WORK/scripts/start-server.sh" <<EOF
#!/usr/bin/env bash
set -euo pipefail
echo \$\$ > "$WORK/server.pid"
exec "$NOMAD" agent -config "$WORK/server.hcl" >> "$WORK/server.log" 2>&1
EOF
  cat > "$WORK/scripts/start-client.sh" <<EOF
#!/usr/bin/env bash
set -euo pipefail
echo \$\$ > "$WORK/client.pid"
exec "$NOMAD" agent -config "$WORK/client.hcl" >> "$WORK/client.log" 2>&1
EOF
  chmod +x "$WORK/scripts/start-server.sh" "$WORK/scripts/start-client.sh"
}

node_is_ready() {
  curl -sf --max-time 2 "$ADDR/v1/nodes" 2>/dev/null | python3 -c '
import json, sys
try:
    nodes = json.load(sys.stdin)
except Exception:
    sys.exit(1)
sys.exit(0 if any(n.get("Status") == "ready" for n in nodes) else 1)
'
}

wait_for_node_ready() {
  local deadline=$((SECONDS + 180))
  while (( SECONDS < deadline )); do
    if node_is_ready; then return 0; fi
    sleep 1
  done
  die "the Nomad node did not become ready; see $WORK/client.log"
}

cleanup() {
  local status=$?
  set +e
  log "cleaning up"
  if [[ -x $NOMAD ]]; then
    # Both boxes of the suite: it1 (the scenarios) and it2 (reconciliation and
    # rm -> create with the same id).
    for box in box-it box-it2; do
      NOMAD_ADDR=$ADDR "$NOMAD" job stop -purge "$box" >/dev/null 2>&1
      NOMAD_ADDR=$ADDR "$NOMAD" volume delete -type=host "$box" >/dev/null 2>&1
    done
  fi
  for pidfile in "$WORK/client.pid" "$WORK/server.pid"; do
    if [[ -f $pidfile ]]; then
      kill -9 "$(cat "$pidfile")" 2>/dev/null
    fi
  done
  sleep 1
  if [[ -n ${LIMACTL:-} && -d $LIMA_HOME ]]; then
    log "deleting Lima instances created by this run"
    for dir in "$LIMA_HOME"/*/; do
      local name
      name=$(basename "$dir")
      case $name in _*) continue ;; esac
      LIMA_HOME=$LIMA_HOME "$LIMACTL" delete -f "$name" >/dev/null 2>&1
    done
  fi
  for unit in $(systemctl --user list-units --all --plain --no-legend 'box-*.scope' 2>/dev/null | awk '{print $1}'); do
    systemctl --user stop "$unit" >/dev/null 2>&1
    systemctl --user reset-failed "$unit" >/dev/null 2>&1
  done
  if [[ $KEEP == 1 ]]; then
    log "kept $WORK (BOX_CLI_KEEP=1)"
  else
    rm -rf "$WORK"
  fi
  exit $status
}

main() {
  need python3
  need curl
  LIMACTL=$(find_limactl)
  NOMAD=$(find_nomad)
  LIMA_HOME=$WORK/lima-home
  mkdir -p "$WORK/bin/plugins" "$WORK/dhv" "$WORK/scripts" "$WORK/volumes" "$LIMA_HOME" "$WORK/data/server" "$WORK/data/client"

  trap cleanup EXIT

  # A leftover agent from an earlier run must never be joined: check the ports
  # instead of killing by pattern.
  if curl -sf -o /dev/null --max-time 1 "$ADDR/v1/status/leader"; then
    die "another agent is already answering on $ADDR; stop it first"
  fi

  log "limactl: $LIMACTL"
  log "nomad:   $NOMAD ($("$NOMAD" version 2>/dev/null | head -1))"
  log "work:    $WORK"

  IMAGE=$(fetch_image)
  log "image:   $IMAGE"
  [[ -x $NOMAD ]] || chmod +x "$NOMAD"

  # Nomad matches an external plugin to its block by executable name.
  log "building the box driver, the box-volume plugin and the box CLI"
  ( cd "$ROOT" && $GO build -o "$WORK/bin/plugins/box" ./cmd/box-driver \
                 && $GO build -o "$WORK/dhv/box-volume" ./cmd/box-volume \
                 && $GO build -o "$WORK/bin/box" ./cmd/box )

  # box-volume takes limactl, lima_home and runtime_user from a settings file the
  # node writes, not from volume parameters (which it now refuses). Nomad gives a
  # host volume plugin no environment, so the file next to the plugin binary is
  # how this node is configured; see docs/design-decisions.md for the format.
  printf '{"limactl": "%s", "lima_home": "%s", "runtime_user": "%s"}\n' \
      "$LIMACTL" "$LIMA_HOME" "$RUNTIME_USER" > "$WORK/dhv/box-volume.json"

  # The real boxd (phase 3) is installed into the box by one scenario when it
  # builds; the session scenarios themselves use a fake boxctl. The fake ACP
  # agent stands in for prime-agent, so a session really runs without a model
  # provider. Both binaries are static: the guest has no NixOS dynamic linker.
  BOXD_BIN=""
  if ( cd "$ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $GO build -o "$WORK/bin/boxd" ./cmd/boxd ) 2>"$WORK/boxd-build.log"; then
    BOXD_BIN=$WORK/bin/boxd
  else
    log "boxd did not build (see $WORK/boxd-build.log); the real-boxd scenario is skipped"
  fi
  FAKEACP_BIN=""
  if ( cd "$ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $GO build -o "$WORK/bin/prime-agent" ./internal/boxd/fakeacp ) 2>"$WORK/fakeacp-build.log"; then
    FAKEACP_BIN=$WORK/bin/prime-agent
  else
    log "fakeacp did not build (see $WORK/fakeacp-build.log); the real-boxd scenario is skipped"
  fi

  write_server_config
  write_client_config
  write_start_scripts

  log "starting the Nomad server on 127.0.0.1:$SERVER_HTTP"
  "$WORK/scripts/start-server.sh" &
  sleep 2
  log "starting the Nomad client on 127.0.0.1:$CLIENT_HTTP"
  "$WORK/scripts/start-client.sh" &
  wait_for_node_ready
  log "node is ready"

  log "running the CLI integration tests"
  # The test runs in its own process group, so cleanup takes the whole group
  # down; a surviving test binary would keep talking to this cluster.
  setsid env \
    NOMAD_ADDR=$ADDR \
    BOX_CLI=$WORK/bin/box \
    BOX_TEST_NOMAD=$NOMAD \
    BOX_TEST_NOMAD_ADDR=$ADDR \
    BOX_TEST_WORK=$WORK \
    BOX_TEST_LIMACTL=$LIMACTL \
    BOX_TEST_LIMA_HOME=$LIMA_HOME \
    BOX_TEST_IMAGE=$IMAGE \
    BOX_TEST_REPO=$ROOT \
    BOX_TEST_BOXD=$BOXD_BIN \
    BOX_TEST_FAKEACP=$FAKEACP_BIN \
    BOX_TEST_RUNTIME_USER=$RUNTIME_USER \
    BOX_RUNTIME_USER=$RUNTIME_USER \
    $GO test -tags integration_cli -count=1 -timeout 60m -v ./integration/... &
  local test_pid=$!
  local rc=0
  wait $test_pid || rc=$?
  return $rc
}

main "$@"
