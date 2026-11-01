# Read an existing request without resubmitting it

Caller0.1.38 adds a read-only view of the durable official acpx0.21.0 journal.
It runs an ephemeral helper through the existing ownership-checked native Nomad
exec transport. No new guest asset, image, daemon, driver schema or model request
is needed. The official package's pinned journal reader handles retained segments;
unsupported versions fail closed instead of guessing a parser or creating a session.

```sh
nomad-workers --config <bound-target-config> request-transcript \
  --session <worker-session-uuid> --request <request-uuid> --limit 50
nomad-workers --config <bound-target-config> request-transcript \
  --session <worker-session-uuid> --request <request-uuid> \
  --cursor '<returned-transcript-cursor>' --limit 50
```

Each call returns one JSON object with `entries`, `cursor`, `has_more`, a terminal
summary and history flags. The cursor belongs only to this session/record/request
and is independent of the live-watch cursor. Repeating a page is deterministic;
no cursor/request/lease record is rewritten. Changed history or a foreign cursor
is an explicit error. Follow `has_more`; terminal metadata on a page does not mean
all pages have been displayed. `history_complete` means the whole turn is present
in retained history, not that this one page contains it all.

Visible user/assistant text and tool start/end/status are projected. Raw tool
arguments/output, environment/config, provider error bodies and hidden thoughts
are omitted. Credential-shaped text is filtered after joining logical ACP text
chunks and before splitting/pagination. This is bounded diagnostic redaction,
not protection against code in the same VM deliberately encoding/exfiltrating a
secret. No secret values are read to implement the filter.

A still-growing text block is reported as `tail_buffered` until a real logical
boundary or terminal marker. Retention gaps, missing history and bounds are
reported rather than presented as a complete empty answer. The command reads at
most an 8MiB opened journal snapshot/32768 events with a five-second scan deadline.
Pages contain at most100 entries and262144 content bytes (defaults50/65536), plus
bounded metadata. Oversized or unavailable history is not a reason to retry the
model prompt; preserve it for an approved diagnostic inspection.

The guest must be accessible under the original target/attempt/owner binding.
A stopped VM cannot be read by alloc exec; use already exported responses/results
when available. This feature does not retrospectively promise that transcripts
were included in old exports.

## Watch lifetime

Live watch uses one private official `sessions watch` child. The caller holds its
native stdin connection open; EOF/disconnect terminates and reaps that child only.
It does not signal a queue owner, runtime daemon, shared session, another
watcher or model turn. Local transport cleanup signals only its own Popen handle. A lost transport is
reported as unknown; absence of a response is not proof of remote cleanup.
A terminal watch returns a cached terminal summary (without provider error bodies) and a `request-transcript`
next action immediately, without starting another observer. Live cursor updates
retain their existing atomic behavior; transcript pages never update that cursor.

## Prompt files

Use quoted heredocs so command-looking prompt text remains literal:

```sh
cat > prompt.txt <<'PROMPT'
Inspect the literal text `example()` and $(example) in the supplied source.
Do not execute these strings in the caller shell.
PROMPT
nomad-workers --config <bound-target-config> submit \
  --session <worker-session-uuid> --request <persisted-request-uuid> \
  --prompt-file prompt.txt
```

Or use a literal direct file write. Do not embed prompt text into an unquoted
`python3 <<EOF` block: the shell expands backticks and substitutions first.

## Verification and distribution

Unit/Node fixtures cover independent/repeated pagination, terminal summary,
changed history, generation/attempt mismatch, split/quoted redaction and quiet
observer disconnect without signalling a separate primary process. An opt-in
fixture sets `NOMAD_WORKERS_TEST_ACPX_PACKAGE` to an already cached official0.21.0
package and reads synthetic retained journals through its real reader, with no
CLI/model invocation and byte-for-byte unchanged inputs.

Ship the entire caller archive, including `scripts/request_observer_guest.mjs`
next to `request_transcript.py`. Operator copy-only-Python staging must also copy
that MJS dependency. Preserve0.1.36 routing and ready-target receipt handling,
current owner/history and cache. No installed target/config mutation, VM restart,
image bake or schema5/secret-profile rollout belongs to this change. Existing
Claude sessions load new instructions only on their next normal resume/start.

New observer lifetime handling applies to newly started CLI commands only. This
source release does not signal, migrate or classify existing watch processes.
