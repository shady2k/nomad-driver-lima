#!/bin/bash
# real-smoke.sh: manual end-to-end check of boxd/boxctl against the REAL
# prime-agent on this host (not part of `go test`).
#
# It costs two tiny provider requests ("reply with the word pong" twice) and
# about a minute, most of it prime-agent's cold model-catalog start. Run it by
# hand when the executor adapter, the resume path or the contract changes:
#
#   MODEL=<provider>/<model> internal/boxd/testdata/real-smoke.sh
#
# MODEL is required: the smoke sends two real prompts, so it needs a provider and
# a model your prime-agent configuration can reach, and it is skipped without it.
#
# What it proves: a session is created, one turn runs, the events are the
# contract events, the prime root session id (the transcript file name) is
# stored, killing the executor mid-life is recovered by "-r <root id>" on the
# next send (executor_restart), a boxd restart keeps the session resumable, and
# the host's own prime config is not modified (the secrets directory is copied
# into the state dir, which PRIME_AGENT_CODING_AGENT_DIR then points at).
set -u
REPO=$(cd "$(dirname "$0")/../../.." && pwd)
MODEL=${MODEL:-}
# The model is not baked in: the smoke needs one provider/model your own
# prime-agent configuration can reach (see --provider in
# deploy/executors-from-prime.sh). Without it there is nothing to send.
if [ -z "$MODEL" ]; then
	echo "SKIP: set MODEL=<provider>/<model> to run the real-model smoke" >&2
	exit 0
fi
W=${W:-/tmp/boxd-smoke}
rm -rf "$W"; mkdir -p "$W/secrets/prime" "$W/run"
for f in auth.json settings.json models.json telemetry.json; do
	[ -f "$HOME/.prime/agent/$f" ] && cp "$HOME/.prime/agent/$f" "$W/secrets/prime/"
done
cp "$HOME/.prime/agent/settings.json" "$W/settings.json.bak" 2>/dev/null

cd "$REPO" || exit 1
go build -o "$W/boxd" ./cmd/boxd || exit 1
ln -sf "$W/boxd" "$W/boxctl"

SOCK="$W/run/boxd.sock"
"$W/boxd" -socket "$SOCK" -state-dir "$W/state" -secrets-dir "$W/secrets" >"$W/journal.log" 2>"$W/boxd.err" &
BOXDPID=$!
trap 'kill -TERM $BOXDPID 2>/dev/null' EXIT
for _ in $(seq 1 100); do [ -S "$SOCK" ] && break; sleep 0.1; done

BC="$W/boxctl --socket $SOCK"
jget() { python3 -c 'import json,sys; print(json.load(sys.stdin).get(sys.argv[1],""))' "$1"; }

echo "=== info ==="; $BC info
echo "=== new session ==="
SID=$($BC session new --executor prime --model "$MODEL" --title smoke | jget session)
echo "session=$SID"
echo "=== send + wait ==="
printf 'reply with the word pong' | $BC send "$SID"
$BC wait "$SID" --timeout 240
echo "=== events ==="
$BC events "$SID" --limit 50
EPID=$($BC session get "$SID" | jget pid)
ROOT=$($BC session get "$SID" | jget executor_session)
echo "executor pid=$EPID root session id=$ROOT"
echo "=== kill -9 the executor, then resume on the next send ==="
kill -9 "$EPID"
sleep 1
$BC session get "$SID"
printf 'reply with the word pong again' | $BC send "$SID"
$BC wait "$SID" --turn 2 --timeout 240
$BC events "$SID" --since 0 --limit 100
echo "=== close ==="
$BC session close "$SID"
echo "=== boxd restart keeps the session listed ==="
kill -TERM "$BOXDPID"; wait "$BOXDPID" 2>/dev/null
"$W/boxd" -socket "$SOCK" -state-dir "$W/state" -secrets-dir "$W/secrets" >>"$W/journal.log" 2>>"$W/boxd.err" &
BOXDPID=$!
for _ in $(seq 1 100); do [ -S "$SOCK" ] && break; sleep 0.1; done
$BC session ls
echo "=== stop ==="
kill -TERM "$BOXDPID"; wait "$BOXDPID" 2>/dev/null
echo "journal events: $(wc -l < "$W/journal.log")"
if diff -q "$W/settings.json.bak" "$HOME/.prime/agent/settings.json" >/dev/null 2>&1; then
	echo "host prime settings.json unchanged"
else
	echo "WARNING: host prime settings.json changed"
fi
echo "DONE (state in $W)"
