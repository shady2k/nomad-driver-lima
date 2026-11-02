#!/usr/bin/env bash
# Integration harness for the box driver.
#
# It builds the two plugin executables, starts a local Nomad server and client
# bound to 127.0.0.1 (a real server+client, NOT -dev: dev mode never restores
# allocations), configures the box driver and the box-volume plugin from
# examples/client.hcl, and runs the integration suite against real Lima VMs on
# this host.
#
# Requirements: limactl 2.x, qemu with /dev/kvm, python3, curl, and ~30 GB of
# free disk. Nothing here talks to any Nomad cluster other than the local one.
#
# Usage: scripts/integration.sh [--root]
#
#   (no argument)  the full suite, with the Nomad server and client running as the
#                  current user, on ports 14646-14658.
#   --root         the root mode: the server and the client run as root through
#                  `sudo -n`, the driver gets runtime_user=<current user>, and the
#                  root-mode scenarios of integration/root_test.go run: create,
#                  run, client restart, stop/start, rm, plus the ownership of
#                  everything Lima touches. Ports 44646-44658, its own work
#                  directory, and its own cleanup (mounts are unmounted before the
#                  tree is removed).
#
# Environment:
#   BOX_TEST_WORK       work directory (default <repo>/.integration, or
#                       <repo>/.integration-root with --root)
#   BOX_TEST_KEEP=1     keep the work directory after the run
#   BOX_TEST_LIMACTL    limactl binary (default: PATH, then a nix store lima 2.x)
#   BOX_TEST_NOMAD      nomad binary (default: work dir, then a nix store zip, then download)
#   BOX_TEST_IMAGE      box base image, for example the 1.0.1 amd64 qcow2
#                       (downloaded when missing)
#   BOX_TEST_RUNTIME_USER  the unprivileged user the driver runs Lima as
#                       (default: the current user; --root needs a non-root user)
#   GO                  go binary (default: go)
#
# Everything this run creates (VMs, volumes, image cache, Nomad data) is removed
# at the end unless BOX_TEST_KEEP=1.

set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
KEEP=${BOX_TEST_KEEP:-0}
GO=${GO:-go}

log() { printf '\n==> %s\n' "$*" >&2; }
die() { printf 'integration: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"; }

ROOT_MODE=0
for arg in "$@"; do
  case $arg in
    --root) ROOT_MODE=1 ;;
    -h|--help) sed -n '2,40p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *) die "unknown argument: $arg (the only option is --root)" ;;
  esac
done

# The user every Lima process of this run runs as. In the plain mode that is the
# user the agents run as; in the root mode it is the user the root client drops
# to, so it must not be root.
RUNTIME_USER=${BOX_TEST_RUNTIME_USER:-$(id -un)}
[[ -n $RUNTIME_USER ]] || die "cannot determine the runtime user; set BOX_TEST_RUNTIME_USER"

if [[ $ROOT_MODE == 1 ]]; then
  [[ $RUNTIME_USER != root ]] || die "--root needs a non-root BOX_TEST_RUNTIME_USER (the driver runs Lima as that user)"
  WORK=${BOX_TEST_WORK:-$ROOT/.integration-root}
  SERVER_HTTP=44646
  SERVER_RPC=44650
  SERVER_SERF=44651
  CLIENT_HTTP=44656
  CLIENT_RPC=44657
  CLIENT_SERF=44658
else
  WORK=${BOX_TEST_WORK:-$ROOT/.integration}
  SERVER_HTTP=14646
  SERVER_RPC=14650
  SERVER_SERF=14651
  CLIENT_HTTP=14656
  CLIENT_RPC=14657
  CLIENT_SERF=14658
fi
ADDR=http://127.0.0.1:$SERVER_HTTP

IMAGE_URL=${BOX_TEST_IMAGE_URL:-https://cloud-images.ubuntu.com/minimal/releases/noble/release/ubuntu-24.04-minimal-cloudimg-amd64.img}

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
  local image=$WORK/images/ubuntu-24.04-minimal.img
  if [[ ! -s $image ]]; then
    log "downloading the Ubuntu 24.04 minimal cloud image (about 250 MB)"
    mkdir -p "$WORK/images"
    curl -fL --retry 3 -o "$image.part" "$IMAGE_URL"
    mv "$image.part" "$image"
  fi
  printf '%s\n' "$image"
}

write_agent_config() {
  local template=$1 outfile=$2 data_dir=$3 http=$4 rpc=$5 serf=$6
  mkdir -p "$data_dir"
  sed -e "s|__DATA_DIR__|$data_dir|g" \
      -e "s|__PLUGIN_DIR__|$WORK/bin|g" \
      -e "s|__HTTP_PORT__|$http|g" \
      -e "s|__RPC_PORT__|$rpc|g" \
      -e "s|__SERF_PORT__|$serf|g" \
      -e "s|__SERVER_RPC_PORT__|$SERVER_RPC|g" \
      -e "s|__VOLUMES_DIR__|$WORK/volumes|g" \
      -e "s|__DHV_PLUGIN_DIR__|$WORK/dhv|g" \
      -e "s|__LIMACTL__|$LIMACTL|g" \
      -e "s|__LIMA_HOME__|$LIMA_HOME|g" \
      -e "s|__RUNTIME_USER__|$RUNTIME_USER|g" \
      -e "s|__IMAGE_CACHE__|$WORK/images/cache|g" \
      "$template" > "$outfile"
}

write_server_config() {
  local data_dir=$1
  mkdir -p "$data_dir"
  cat > "$WORK/server.hcl" <<EOF
data_dir   = "$data_dir"
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

# agent_prefix is what a start script execs: the agent itself, or sudo with the
# PATH of this shell. The root client needs the PATH because the driver builds the
# PATH of its own children from it (lima and qemu live outside /usr/bin here), and
# `env` makes sure sudo passes it on.
agent_prefix() {
  if [[ $ROOT_MODE == 1 ]]; then
    printf 'sudo -n env PATH=%s' "$PATH"
  else
    printf ''
  fi
}

# write_start_scripts writes the two launchers. The pid file must hold the pid of
# the *agent*, not of sudo: cleanup and the tests stop an agent by that pid, and a
# sudo pid would leave a root agent behind (measured: sudo forks, the agent is its
# child). In the root mode the pid is therefore written by a shell that sudo runs
# and that then execs the agent, so the recorded pid is the agent's own.
write_start_scripts() {
  local prefix
  prefix=$(agent_prefix)
  if [[ $ROOT_MODE == 1 ]]; then
    cat > "$WORK/scripts/start-server.sh" <<EOF
#!/usr/bin/env bash
set -euo pipefail
exec $prefix sh -c 'echo \$\$ > "$WORK/server.pid"; exec "\$0" agent -config "$WORK/server.hcl"' "$NOMAD" >> "$WORK/server.log" 2>&1
EOF
    cat > "$WORK/scripts/start-client.sh" <<EOF
#!/usr/bin/env bash
set -euo pipefail
exec $prefix sh -c 'echo \$\$ > "$WORK/client.pid"; exec "\$0" agent -config "$WORK/client.hcl"' "$NOMAD" >> "$WORK/client.log" 2>&1
EOF
  else
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
  fi
  chmod +x "$WORK/scripts/start-server.sh" "$WORK/scripts/start-client.sh"
}

nomad_cli() { NOMAD_ADDR=$ADDR "$NOMAD" "$@"; }

wait_for_node_ready() {
  local deadline=$((SECONDS + 120))
  while (( SECONDS < deadline )); do
    if nomad_cli node status 2>/dev/null | grep -qE '[[:space:]]ready[[:space:]]*$'; then
      return 0
    fi
    sleep 1
  done
  die "the Nomad node did not become ready; see $WORK/client.log"
}

# kill_agent stops one agent by its pid file. The pid file holds the pid of the
# start script, which execs the agent (through sudo in the root mode), so it is
# the agent's own pid; a root agent can only be signalled as root.
kill_agent() {
  local pidfile=$1 pid
  [[ -f $pidfile ]] || return 0
  pid=$(cat "$pidfile")
  if [[ $ROOT_MODE == 1 ]]; then
    sudo -n kill -9 "$pid" 2>/dev/null
  else
    kill -9 "$pid" 2>/dev/null
  fi
}

# unmount_work unmounts what the agents mounted below the work directory. A Nomad
# client bind-mounts the alloc directory of every allocation, and a mounted tree
# cannot be removed: the tree has to be unmounted deepest first, and only then
# removed.
unmount_work() {
  local mounts mountpoint
  mounts=$(mount | sed -n "s|.* on \(.*\) type .*|\1|p" | grep -F "$WORK" | sort -r || true)
  [[ -n $mounts ]] || return 0
  log "unmounting the mounts of this run"
  while read -r mountpoint; do
    [[ -n $mountpoint ]] || continue
    if [[ $ROOT_MODE == 1 ]]; then
      sudo -n umount "$mountpoint" 2>/dev/null || sudo -n umount -l "$mountpoint" 2>/dev/null
    else
      umount "$mountpoint" 2>/dev/null || umount -l "$mountpoint" 2>/dev/null
    fi
  done <<< "$mounts"
  sleep 1
  if mount | grep -qF "$WORK"; then
    log "warning: a mount below $WORK survived the unmount:"
    mount | grep -F "$WORK" >&2
  fi
}

cleanup() {
  local status=$?
  set +e
  if [[ -x $NOMAD ]]; then
    log "cleaning up Nomad state"
    nomad_cli job stop -purge box >/dev/null 2>&1
    nomad_cli volume delete -type=host boxdisk >/dev/null 2>&1
  fi
  if [[ -n ${TEST_PID:-} ]]; then
    kill -9 -"$TEST_PID" 2>/dev/null
  fi
  kill_agent "$WORK/client.pid"
  kill_agent "$WORK/server.pid"
  sleep 2
  unmount_work
  if [[ -n ${LIMACTL:-} && -d $LIMA_HOME ]]; then
    log "deleting Lima instances created by this run"
    for dir in "$LIMA_HOME"/*/; do
      local name
      name=$(basename "$dir")
      case $name in _*) continue ;; esac
      LIMA_HOME=$LIMA_HOME "$LIMACTL" delete -f "$name" >/dev/null 2>&1
    done
  fi
  # Remove the transient memory scopes the driver created for the boxes.
  for unit in $(systemctl --user list-units --all --plain --no-legend 'box-*.scope' 2>/dev/null | awk '{print $1}'); do
    systemctl --user stop "$unit" >/dev/null 2>&1
    systemctl --user reset-failed "$unit" >/dev/null 2>&1
  done
  # `limactl delete -f` above stops the hostagent and the qemu process of each
  # instance, so no pattern-based kill is needed here.
  if [[ $KEEP == 1 ]]; then
    log "kept $WORK (BOX_TEST_KEEP=1)"
  elif [[ $ROOT_MODE == 1 ]]; then
    # The agents created their data directory as root.
    sudo -n rm -rf "$WORK"
  else
    rm -rf "$WORK"
  fi
  exit $status
}

main() {
  need python3
  need curl
  if [[ $ROOT_MODE == 1 ]]; then
    need sudo
    sudo -n true 2>/dev/null || die "--root needs passwordless sudo (sudo -n true failed)"
  fi
  # find_nomad downloads into $WORK, so $WORK must exist before it runs (a
  # previous run's cleanup removes it, and curl then fails with a write error).
  mkdir -p "$WORK"
  LIMACTL=$(find_limactl)
  NOMAD=$(find_nomad)
  LIMA_HOME=$WORK/lima-home

  trap cleanup EXIT

  # A leftover agent from an earlier run must never be joined as if it were this
  # run's cluster: it would carry the old volumes and allocations. This harness
  # never kills by pattern (a pattern can match unrelated processes, including
  # the shell that runs it); it only checks that the port is free and, at the
  # end, stops the two agents it started through their pid files.
  for _ in $(seq 1 20); do
    curl -sf -o /dev/null --max-time 1 "$ADDR/v1/status/leader" || break
    sleep 0.5
  done
  if curl -sf -o /dev/null --max-time 1 "$ADDR/v1/status/leader"; then
    die "another agent is still answering on $ADDR; stop it first"
  fi

  mkdir -p "$WORK/bin" "$WORK/dhv" "$WORK/scripts" "$WORK/volumes" "$LIMA_HOME" "$WORK/data/server" "$WORK/data/client"
  # A nomad binary from a read-only prefix (a nix store, /opt) is executable
  # already; only a downloaded copy needs the bit.
  [[ -x $NOMAD ]] || chmod +x "$NOMAD"

  log "limactl: $LIMACTL"
  log "nomad:   $NOMAD ($("$NOMAD" version 2>/dev/null | head -1))"
  log "work:    $WORK"
  log "mode:    $([[ $ROOT_MODE == 1 ]] && echo "root client, runtime user $RUNTIME_USER" || echo "current user")"

  IMAGE=$(fetch_image)
  log "image:   $IMAGE"

  # Nomad matches an external plugin to its configuration block by executable
  # name, so the driver binary is installed as "box" (the plugin name) and the
  # volume plugin as "box-volume" (its plugin_id).
  log "building the box driver and the box-volume plugin"
  # Driver plugins and host volume plugins live in different directories: Nomad
  # runs *every* executable in host_volume_plugin_dir as a volume plugin.
  ( cd "$ROOT" && $GO build -o "$WORK/bin/box" ./cmd/box-driver \
                 && $GO build -o "$WORK/dhv/box-volume" ./cmd/box-volume )

  # box-volume reads which limactl to run, which Lima home to use and which user
  # owns the volumes from a settings file the node writes: Nomad gives a host
  # volume plugin no environment at all, so a file next to the plugin binary is
  # how this node is configured (see docs/design-decisions.md for the format).
  log "writing the box-volume settings file"
  printf '{"limactl": "%s", "lima_home": "%s", "runtime_user": "%s"}\n' \
      "$LIMACTL" "$LIMA_HOME" "$RUNTIME_USER" > "$WORK/dhv/box-volume.json"

  write_server_config "$WORK/data/server"
  write_agent_config "$ROOT/examples/client.hcl" "$WORK/client.hcl" "$WORK/data/client" \
      "$CLIENT_HTTP" "$CLIENT_RPC" "$CLIENT_SERF"
  write_start_scripts

  log "starting the Nomad server on 127.0.0.1:$SERVER_HTTP"
  "$WORK/scripts/start-server.sh" &
  sleep 2
  log "starting the Nomad client on 127.0.0.1:$CLIENT_HTTP"
  "$WORK/scripts/start-client.sh" &
  wait_for_node_ready
  log "node is ready"

  log "running the integration tests"
  # The test runs in its own process group so that cleanup can take the whole
  # group down: a surviving test binary would keep talking to this cluster.
  # The root mode runs its own scenarios only: the full suite resizes a box to
  # 4 vCPU and kills the client as the test user, neither of which belongs to a
  # root run on a shared host.
  local test_args=(-tags integration -count=1 -timeout 120m -v ./integration/...)
  if [[ $ROOT_MODE == 1 ]]; then
    test_args+=(-run TestBoxRootLifecycle)
  fi
  setsid env \
  BOX_TEST_NOMAD=$NOMAD \
  BOX_TEST_NOMAD_ADDR=$ADDR \
  BOX_TEST_WORK=$WORK \
  BOX_TEST_LIMACTL=$LIMACTL \
  BOX_TEST_LIMA_HOME=$LIMA_HOME \
  BOX_TEST_IMAGE=$IMAGE \
  BOX_TEST_EXAMPLES=$ROOT/examples \
  BOX_TEST_START_CLIENT=$WORK/scripts/start-client.sh \
  BOX_TEST_CLIENT_PID=$WORK/client.pid \
  BOX_TEST_NOMAD_LOG=$WORK/client.log \
  BOX_TEST_RUNTIME_USER=$RUNTIME_USER \
  BOX_TEST_ROOT_MODE=$ROOT_MODE \
  $GO test "${test_args[@]}" &
  TEST_PID=$!
  TEST_RC=0
  wait $TEST_PID || TEST_RC=$?
  return $TEST_RC
}

TEST_PID=""
main "$@"
