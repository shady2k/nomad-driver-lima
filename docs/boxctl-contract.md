# The boxctl contract: the `box` CLI on the host, `boxd` in the guest

This is the interface between the two halves of a box: the `box` CLI on a
coordinator, which talks to Nomad, and `boxd`, the daemon inside the guest that
runs the agent sessions. It is the only way the CLI reaches into a box.

## Transport

The CLI never talks to a box's network. There is no SSH key, no published port
and no address to find. Every guest call is one `nomad alloc exec`:

```
nomad alloc exec -task box <alloc> /usr/local/bin/boxctl <args...>
```

(the driver does the same through `limactl shell` when it needs the guest
itself). Large payloads (prompt text, files) go over **stdin**.

`boxctl` prints exactly one JSON document to **stdout** and exits 0. On failure
it prints

```json
{"error":{"code":"<snake_case>","message":"<plain words>"}}
```

and exits 1. Diagnostics go to stderr only.

`boxctl` is a client of `boxd`, which listens on the unix socket
`/run/boxd/boxd.sock` inside the guest. The protocol on that socket is boxd's
own business; `--socket PATH` (or `BOXD_SOCKET`) points boxctl somewhere else,
which is how the tests run it. If boxd is down, every command answers
`{"error":{"code":"boxd_unavailable",...}}`.

## Layout in the guest

| path | what it is |
| --- | --- |
| `/usr/local/bin/boxd` | the daemon; `boxd.service` runs it |
| `/usr/local/bin/boxctl` | the same binary, by `argv[0]` or the `boxctl` subcommand |
| `/var/lib/boxd/` | durable state on the box disk: sessions and their transcripts; survives a restart of the VM and a reallocation |
| `/var/lib/boxd/sessions/<id>/events.jsonl` | one session's event feed |
| `/var/lib/boxd/homes/<executor>/` | the executor's `HOME`, on the box disk: caches, downloaded runtimes, virtual environments, logs. Programs run from there (the guest's `/run` is a small tmpfs mounted `noexec`) and it survives a reboot. No credential goes there |
| `/etc/box/local/` | files the driver copied from the task's `local/` directory (not secrets) |
| `/run/box/secrets/` | the task's `secrets/` directory, on the guest's tmpfs, root-only, mode 0700: one directory per executor, `/run/box/secrets/<executor>/` |
| `/run/boxd/` | everything boxd derives for this boot, also a tmpfs: the executors' mutable copy of their secrets (`executors/<executor>/`) and their `XDG_CONFIG_HOME` (`executors/<executor>-config/`) |

The two tmpfs paths are the reason a box's credentials never reach the box disk:
the driver writes them after boot, boxd reads them, and a reboot removes them.

The driver streams `journalctl -f -o cat -u boxd` into the Nomad task log, so
`nomad alloc logs` (and `box logs`) shows the guest's activity. boxd writes every
event to that journal as one line for a person, and to the session's
`events.jsonl` as the structured event:

```
2026-11-04 12:36:07 UTC session s-9123db turn 1: started: You are a worker in a Linux VM...
2026-11-04 12:36:09 UTC session s-9123db turn 1: tool: Run go test ./...
2026-11-04 12:38:10 UTC session s-9123db turn 1: still working: running for 2m3s; last, 2m1s ago, tool: Run go test ./...
2026-11-04 12:39:02 UTC session s-9123db turn 1: finished: All 214 tests pass.
```

A running turn that has been silent for two minutes writes a `still working`
line with its last event. The driver adds its own lines to the task's stdout, between whole lines of the
journal, so the one log shows both:
every command run in the box through `alloc exec` (`box run`, `box cp`, the
CLI's `boxctl` calls), when it started, every two minutes while it runs, and
its exit code, with the command cut to 200 characters.

## Commands

Every command prints one JSON document.

| command | stdin | result |
| --- | --- | --- |
| `boxctl info` | – | `{"boxd_version","executors":[ExecutorInfo],"sessions":N,"mem_total_mib","mem_free_mib","cpus"}` |
| `boxctl session new --executor prime\|opencode\|claude --model <provider>/<model> [--cwd DIR] [--title T]` | – | `{"session":"<id>"}` |
| `boxctl session ls` | – | `{"sessions":[Session]}` |
| `boxctl session get <id>` | – | `Session` |
| `boxctl send <id>` | prompt text | `{"session","turn":N}` — returns immediately; `busy` if a turn is already running |
| `boxctl wait <id> [--turn N] [--timeout SEC]` | – | `Turn` when the turn has finished; on timeout the same `Turn` with `"state":"running"` and exit 0 |
| `boxctl events <id> [--since SEQ] [--limit N]` | – | `{"events":[Event],"next":SEQ}` |
| `boxctl cancel <id>` | – | `Turn`, with `"state":"cancelled"` once the executor acknowledged it |
| `boxctl session close <id>` | – | `{"session","state":"closed"}` — stops the executor process and keeps the history |
| `boxctl reload` | – | re-reads the executor credentials without touching the sessions: it answers `{"configured":[...],"restarted":[...],"deferred":[...]}` — idle sessions restart their executor now, a session in a turn defers to the end of that turn |

## Objects

`ExecutorInfo`: `{"name","version","available","configured","reason"}`.
`available` says the binary is installed. `configured` says the box holds
credentials and configuration for it in `/run/box/secrets/<name>/`. `configured`
is omitted when the binary is missing, because `available` already says
everything. `reason` is the sentence a person reads when something is missing.

`Session`: `{"id","executor","model","cwd","title","state","created_at","last_turn","executor_session","pid"}`
with `state` one of `idle`, `running`, `starting`, `dead`, `closed`.
`executor_session` is the executor's own transcript id, the one a resumed
session reopens.

`Turn`: `{"session","turn","state","started_at","ended_at","stop_reason","text","error"}`
with `state` one of `running`, `done`, `cancelled`, `error`. `text` is the final
assistant text of the turn.

`Event`: `{"seq","ts","session","turn","kind","summary","data"}`
with `kind` one of `turn_start`, `message`, `tool`, `tool_result`, `turn_end`,
`error`, `executor_exit`, `executor_restart`, and `summary` one line in plain
words. Sequence numbers are per session and monotonic, which is what makes
`--since` a resumable cursor.

Session ids are short and stable, generated by boxd (`s-7f3a2c`).

Error codes: `boxd_unavailable`, `busy`, `not_found`, `invalid_argument`,
`session_closed`, `executor_error`, `internal`, `timeout`.

## Executors

An executor is one agent product, driven over the Agent Client Protocol. Three
are known: `prime` (prime-agent), `opencode` and `claude` (claude-code-acp).
Each one runs as one process per session, with its own home below `/run/boxd`,
its own `--provider`/`--model` and its own transcript.

`boxctl session new` names the executor and a model as `<provider>/<model>`: the
provider is whatever the operator configured, never a fixed one.

## The box as a Nomad job

The CLI owns these conventions; the driver supports them.

- job name `box-<box-id>`, group and task `box`,
  `meta { box = "true", box_id, image, arch }`
- dynamic host volume `box-<box-id>` (plugin `box-volume`), mounted as the
  volume `disk`
- the executor credentials are one Nomad variable, `nomad/jobs/box-executors`,
  rendered into the task's `secrets/` directory with one template per file; an
  item whose name ends in `.tmpl` is template text that reads its secret from
  Vault, and the task then carries a `vault` block (`change_mode = "noop"`,
  `disable_file = true`) with the role the CLI was given

`examples/box.nomad.hcl` and `examples/box.volume.hcl` show the shape, and
`docs/architecture.md` explains why it is this shape.
