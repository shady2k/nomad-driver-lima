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

`boxctl` prints exactly one JSON document to **stdout** and exits 0. One command
is the exception: `boxctl seal` streams one document per line, its steps first
(below). On failure it prints

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
| `/opt/box/` | the platform bundle, a read-only mount the node made before the boot: `bin/boxd`, `bin/boxctl`, `bin/<executor>`, `tools/<executor>/`, `addons/`, `manifest.json`. An image of the older shape has no such mount and carries boxd itself |
| `/opt/box/bin/boxd` | the daemon in a box with a bundle; `boxd.service` runs it |
| `/opt/box/bin/boxctl` | the same binary, by `argv[0]` or the `boxctl` subcommand |
| `/usr/local/bin/boxd` | the daemon in an image that carries its own boxd (no bundle) |
| `/usr/local/bin/boxctl` | that same binary |
| `/var/lib/boxd/` | durable state on the box disk: sessions and their transcripts; survives a restart of the VM and a reallocation |
| `/var/lib/boxd/sessions/<id>/events.jsonl` | one session's event feed |
| `/var/lib/boxd/homes/<executor>/` | the executor's `HOME`, on the box disk: caches, downloaded runtimes, virtual environments, logs. Programs run from there (the guest's `/run` is a small tmpfs mounted `noexec`) and it survives a reboot. No credential goes there |
| `/etc/box/local/` | files the driver copied from the task's `local/` directory (not secrets) |
| `/etc/box/image.json` | the record of the image this box is, written by `boxctl seal` (or by `images/build.sh` for a built image): `schema`, `version`, `name`, `arch`, `parent`, `platform`, `recipe`, `sealed_at` |
| `/etc/box/recipe.sh` | the recipe a seal wrote: the script `box bake --rebuild` replays on a newer base image |
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
its exit code, with the command cut to 200 characters. The CLI's boxctl calls
that only read state (`info`, `session ls|get`, `wait`, `events`, `prepare
get|wait`) are not listed: the CLI repeats them while it waits, and what they
report is in boxd's own lines.

Besides sessions, the journal says what helps to diagnose a box: what boxd
starts with (its version, the executors and whether they have credentials,
whether the task files are mounted, and which platform bundle it runs from --
`platform 1.2.0 (boxd 1.2.0)` or `no bundle mounted at /opt/box`), every line an
executor writes to stderr
(at most 40 a minute, all of them in its `executor.log`), the log files of
prime's background workers, how an executor exited and its last lines, the
guest kernel's lines about memory and the disk (out of memory, I/O errors,
ext4 errors, hung tasks), when memory or a disk is low, when the guest has
waited on its disk for a minute, and a process that has used a whole core for
ten minutes (`guest: systemd-logind (pid 851) has used a whole core for 10m`),
with a line when it stops using it. The last of those is a measurement and never
a verdict: a long compile holds a core for ten minutes too, and the line is there
so that a box that lost a core for four hours does not lose it silently. A tool
call names what it runs (the code of a Python cell, the command of a shell) and
its result says its exit code, its time and the beginning and the end of its
output. The driver adds to the task events when the kernel OOM-killed something
in the box's cgroup.

## Commands

Every command prints one JSON document.

### `boxctl prepare start|get|wait|cancel|skip`

The box's preparation (`docs/cli.md`, "Preparing a box"). `start --dest DIR
--repo URL [--ref R]` clones, `start --dest DIR --files` unpacks the tar stream
on stdin, `start` alone runs the recipe again on the last sources; `--limit SEC`
is the time limit. The sources belong to the uid and gid boxctl runs as. Each
prints the state: `{"state", "step", "source", "repo", "ref", "dest", "recipe",
"limit_sec", "started_at", "finished_at", "error", "last_line", "tail"}`.
`wait [--timeout SEC]` returns when the state is not `preparing`, or after the
timeout with the state as it is. `boxctl info` carries the same document as
`prepare`. `session new` refuses with `busy` while preparing and with
`invalid_argument` after a failure, naming the ways out.

| command | stdin | result |
| --- | --- | --- |
| `boxctl info` | – | `{"boxd_version","executors":[ExecutorInfo],"sessions":N,"mem_total_mib","mem_free_mib","cpus","platform":PlatformInfo,"seal":SealStatus,"prepare","usage":Usage}` |
| `boxctl session new --executor prime\|opencode\|claude --model <provider>/<model> [--cwd DIR] [--title T]` | – | `{"session":"<id>"}` |
| `boxctl session ls` | – | `{"sessions":[Session]}` |
| `boxctl session get <id>` | – | `Session` |
| `boxctl send <id>` | prompt text | `{"session","turn":N}` — returns immediately; `busy` if a turn is already running |
| `boxctl wait <id> [--turn N] [--timeout SEC]` | – | `Turn` when the turn has finished; on timeout the same `Turn` with `"state":"running"` and exit 0 |
| `boxctl events <id> [--since SEQ] [--limit N]` | – | `{"events":[Event],"next":SEQ,"total":N,"more":BOOL}` |
| `boxctl cancel <id>` | – | `Turn`, with `"state":"cancelled"` once the executor acknowledged it |
| `boxctl session close <id>` | – | `{"session","state":"closed"}` — stops the executor process and keeps the history |
| `boxctl reload` | – | re-reads the executor credentials without touching the sessions: it answers `{"configured":[...],"restarted":[...],"deferred":[...]}` — idle sessions restart their executor now, a session in a turn defers to the end of that turn |
| `boxctl seal --name NAME [--version V] [--parent V] [--arch amd64\|arm64] [--recipe FILE\|-]` | – (the recipe, with `--recipe -`) | one `{"line":"..."}` per step, then `{"seal":SealResult}` or `{"error":ErrorBody}` — see below |

### `boxctl seal --name NAME [--version V] [--parent V] [--arch amd64|arm64] [--recipe FILE|-]`

Empties the box and makes it an image (`docs/guest-image.md`, "Project images").
It closes every session, stops the executors, deletes the work, the sessions and
the credentials, runs the bundle's `guest/no-secrets.sh` as its proof and releases
the free blocks. `box bake` is what runs it, inside a clone of the box: the box
itself keeps its disk and its work, and its own `boxctl info` is where the bake
asks the seal's gate first (`seal` in the `info` result: `{"ok","reason"}`).

Before it deletes anything it refuses a guest that has the box's credentials
mounted: `/run/box/secrets` is the driver's read-only mount of the host's tmpfs, and
any entry in it means a box somebody works in, not a machine about to become an
image. That is `invalid_argument`, and the sentence says to bake the box instead. A
bake's clone mounts empty stand-ins there, so a bake never meets this refusal.

`--name` is required. `--arch` is `amd64` or `arm64`. The architecture, the
version and the parent are recorded in `/etc/box/image.json` and used for nothing
else here: a version number belongs to the host, which counts it from the images
and the drafts it can see. `--recipe FILE` is the recipe script, or `-` for
stdin, at most 64 KiB; it goes into the record in base64 and to
`/etc/box/recipe.sh`.

The answer is a stream, one JSON object per line on stdout:

```json
{"line": "closing the box's sessions and stopping its executors"}
{"line": "closed 2 session(s); no executor runs in the box now"}
{"line": "the workspace: deleted /work (1.2 GiB)"}
...
{"seal": {"image": {...}, "steps": ["..."]}}
```

or, when it stops,

```json
{"error": {"code": "internal",
           "message": "guest/no-secrets.sh failed in this box: FAIL  credential files are in the image:. A seal empties the machine it runs in, which for a bake is a clone of the box and never the box itself: remove what it names in the box and bake again"}}
```

or when it refuses a guest that holds the box's credentials, before it deletes
anything,

```json
{"error": {"code": "invalid_argument",
           "message": "this guest has the box's credentials mounted in /run/box/secrets (dev, prime), and a seal is only for a guest that is about to become an image. Bake the box instead, which seals a clone with empty stand-ins for the credentials (docs/guest-image.md)"}}
```

`seal.image` is the record it wrote to `/etc/box/image.json`: `schema`,
`version`, `name`, `arch`, `parent`, `platform` (`"mount"`), `recipe` (base64)
and `sealed_at`. `steps` are the lines it printed, in order. That is what the
result carries: the seal runs no search of the disk for a credential value, so
there is no count of the files it read or the values it looked for. Both failure
sentences name the machine they emptied, because in a bake it is the clone and the
box the owner works in keeps its work.

A refusal is a document and exit 1, like every boxctl failure, because the steps
before it are the useful part and they are what a person reads. The codes:

| code | when |
| --- | --- |
| `busy` | a turn runs (`box cancel <box> <session>` stops it) or a preparation is in progress (`box prepare <box> --cancel` stops it) |
| `invalid_argument` | the name, the version, the architecture or the recipe is not one a seal accepts, or the guest has the box's credentials mounted (`/run/box/secrets` holds an entry: a seal is only for a guest that is about to become an image) |
| `internal` | a step failed, a proof failed, or the bundle carries no `lib/no-secrets.sh` |
| `boxd_unavailable` | nothing listens on the socket |

## Objects

`PlatformInfo`: `{"path","mounted","version","boxd","tools","reason"}` -- the
platform bundle the node mounted at `path` (`/opt/box`), out of its
`manifest.json`. `mounted` is false when there is no bundle, and `reason` then
says so in words ("no bundle is mounted at /opt/box", or that the manifest is not
readable). `version`, `boxd` and `tools` (`{"<executor>": "<version>"}`, as the
bundle records them) are present only when it is mounted. It is how a person
answers "which bundle is this box on".

`ExecutorInfo`: `{"name","version","available","configured","reason"}`.
`available` says the binary is installed. `configured` says the box holds
credentials and configuration for it in `/run/box/secrets/<name>/`. `configured`
is omitted when the binary is missing, because `available` already says
everything. `reason` is the sentence a person reads when something is missing.

`SealStatus`: `{"ok","reason"}` -- whether the seal's gate would let `boxctl seal`
run in this box now. `ok` is false while a turn runs or a preparation is in
progress, and `reason` is the sentence the seal would refuse with. That is the gate
and not the whole seal: a guest with the box's credentials mounted is refused by
the seal itself, when it runs (`invalid_argument` above). `box bake` reads this
before it stops anything, because the question is about this box and the clone the
seal runs in has a `boxd` of its own that knows nothing of the box's turn.

`Session`: `{"id","executor","model","cwd","title","state","created_at","last_turn","executor_session","pid"}`
with `state` one of `idle`, `running`, `starting`, `dead`, `closed`.
`executor_session` is the executor's own transcript id, the one a resumed
session reopens.

`Turn`: `{"session","turn","state","started_at","ended_at","stop_reason","text","error","still_running"}`
with `state` one of `running`, `done`, `cancelled`, `error`. `text` is the final
assistant text of the turn.

`still_running` is what the turn left running in the guest: boxd scans `/proc`
at the moment the turn ends and reports the processes that started after the
turn did and that are not the box's own machinery (the bundle's `boxd` and
executors, an executor's home) and not another systemd service's. It is
`{"processes":[{"pid","command","age_seconds","cpu_seconds","children"}],
"more":N}`: one entry per process **at the top of its own tree or process
group**, with everything else in that tree counted in `children`, at most ten of
them and `more` for the rest. `command` is the program and its first few
arguments, cut to about 80 characters -- an argument can hold a token, so a
process list never shows one. The field is omitted when the turn left nothing
running, and a scan that fails leaves it out and says so once in the log.

**It changes nothing about the turn.** When a turn ends is the executor's
decision (the ACP answer's stop reason), so the turn's `state`, its
`stop_reason`, `ended_at` and the exit code of `boxctl wait` are what the
executor said they are. `still_running` says what boxd saw, not that the work
is unfinished: a program an executor installed in `/usr/local/bin` and left
running is indistinguishable from the work, and a build that is about to finish
looks like one that is stuck. `box wait` prints it as a warning on stderr.

`Event`: `{"seq","ts","session","turn","kind","summary","data"}`
with `kind` one of `turn_start`, `message`, `tool`, `tool_result`, `turn_end`,
`error`, `executor_exit`, `executor_restart`, and `summary` one line in plain
words. Sequence numbers are per session and monotonic, which is what makes
`--since` a resumable cursor.

`--since` is the sequence number to resume from and it is **read again**: a page
of a feed loses no event at its boundary, so reading with `--limit` and continuing
at `next` gives every event once. `total` is how many events there are from
`--since`, and `more` says that `events` is a page of them and not the whole feed:
a reader that takes the limit for the end of the events waits for events that are
already there. `boxctl events` also writes one line to stderr when it was cut --
`200 of 350 events, more with --since 201` -- and the `box` CLI says the same.

`Usage`: `{"at","cpu_percent","iowait_percent","load1","load5","mem_total_mib",
"mem_available_mib","memory_pressure","disk_path","disk_total_mib","disk_free_mib",
"top","processes","hogs","leftover"}` -- what boxd measured of the guest, every
five seconds; `box top` and `box status` print it.

- `memory_pressure` is the guest kernel's `some`/`full` avg10 of
  `/proc/pressure/memory`, and is absent on a kernel without PSI.
- `processes` are the guest's busiest processes by CPU, most first, at most five:
  `[{"pid","command","cpu_percent","mem_mib"}]`, with `cpu_percent` the share of
  one core each used since boxd's last sample and `command` cut to about 80
  characters. A sample that cannot read the process table leaves them out.
- `hogs` are the processes that have used a whole core for ten minutes or more:
  `[{"pid","command","cpu_percent","for_seconds"}]`, the longest first. It is a
  measurement, not a fault -- a long compile holds a core for ten minutes as well.
- `leftover` is `{"count":N,"processes":[StillRunningProcess],"more":N}`: the work
  processes alive while **no turn is running in any session**, by the rule of
  `still_running` without its start-time test, so a process older than any turn is
  in it. `processes` is the heaviest few of them in the lines `still_running`
  uses, and `count` is all of them. The field is **omitted while a turn runs**,
  because a process of a running turn cannot be told from the work it was asked to
  do. A scan that fails leaves it out and says so once in the log.

`SealResult`: `{"image":ImageRecord,"steps":["..."]}` -- what a seal wrote and what
it did to get there. `ImageRecord` is
`{"schema","version","name","arch","parent","platform","recipe","sealed_at"}`,
the document at `/etc/box/image.json`; `recipe` is base64 and empty for an image
without one. `steps` are the lines the seal printed, in order.

Session ids are short and stable, generated by boxd (`s-7f3a2c`).

Error codes: `boxd_unavailable`, `busy`, `not_found`, `invalid_argument`,
`session_closed`, `executor_error`, `internal`, `timeout`.

## Executors

An executor is one agent product, driven over the Agent Client Protocol. Three
are known: `prime` (prime-agent), `opencode` and `claude` (claude-code-acp).
Each one runs as one process per session, with its own home below `/run/boxd`,
its own `--provider`/`--model` and its own transcript.

An executor is not installed by the image: the image's `/opt/box` mount holds the
bundle's `bin/`, and boxd puts that directory first on the `PATH` it gives an
executor (and on the `PATH` a version probe runs with). So the executors a box
has are the ones its bundle carries, and `boxctl info` says which of them are
available. The add-ons a bundle may hold are at `/opt/box/addons`, which is where
boxd looks for an addon that declares a model catalog.

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
