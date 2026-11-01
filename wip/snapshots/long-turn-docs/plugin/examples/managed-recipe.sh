#!/usr/bin/env bash
# Source/inline this helper INSIDE an owned managed operation, never on the host.
# No arguments, URL, credentials or environment are echoed. Native runner redacts.
worker_step() {
  local label="$1" rc; shift
  printf '@@worker-step-start %s\n' "$label"
  if "$@"; then rc=0; else rc=$?; fi
  printf '@@worker-step-end %s %s\n' "$label" "$rc"
  return "$rc"
}
_worker_download() {
  local url="$1" output="$2" pid bytes rc
  [[ "$output" == /* && "$output" != / ]] || return 64
  : > "$output" || return 1
  curl -fSL --connect-timeout 20 --retry 0 --progress-meter --output "$output" "$url" &
  pid=$!
  while kill -0 "$pid" 2>/dev/null; do
    bytes=$(stat -c %s -- "$output" 2>/dev/null) || bytes=0
    printf '@@worker-download %s unknown\n' "$bytes"
    sleep 5
  done
  if wait "$pid"; then rc=0; else rc=$?; fi
  bytes=$(stat -c %s -- "$output" 2>/dev/null) || bytes=0
  printf '@@worker-download-final %s unknown\n' "$bytes"
  return "$rc"
}
worker_download() {
  local label="$1"; shift
  worker_step "$label" _worker_download "$@"
}
# Example recipe body (versions/checksums come from the project, not this helper):
# worker_download download-go "$APPROVED_GO_URL" /tmp/go.tgz
# worker_step verify-go sha256sum --check /tmp/go.tgz.sha256
# worker_step install-go sudo tar -C /usr/local -xzf /tmp/go.tgz
