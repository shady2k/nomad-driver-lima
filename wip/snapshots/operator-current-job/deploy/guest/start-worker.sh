#!/bin/sh
# Guest-only Nomad main task. No companion daemon, prompt, automatic retry or purge.
set -eu
umask 077
export PATH=/usr/local/bin:/usr/bin:/bin
export PI_OFFLINE=1
export PRIME_AGENT_CODING_AGENT_DIR=/var/lib/nomad-workers/provider
export PRIME_AGENT_DAEMON_SOCKET=/var/tmp/nomad-workers/prime.sock
export PRIME_AGENT_KERNEL_PYTHON=/opt/worker/prime/prime-agent-runtime/.venv/bin/python
test -f /opt/nomad-workers/runtime.ready
cd /workspace/repo
printf '%s\n' 'Runtime ready; awaiting external provider configuration.'
while [ ! -f "$PRIME_AGENT_CODING_AGENT_DIR/provider.ready" ]; do sleep 1; done
test -s "$PRIME_AGENT_CODING_AGENT_DIR/models.json"
exec /usr/local/bin/prime-agent --mode daemon --daemon-socket "$PRIME_AGENT_DAEMON_SOCKET"
