# `box` command reference

`box` is the CLI. It keeps no local state: everything it reports comes from the
Nomad API, and every guest call is a `nomad alloc exec` into the box.

```sh
box [--json] <command> [arguments]
```

- `--json` may come before or after the command name and gives one JSON document.
- `--image-index URL` (or `--image-index=URL`) is the image index of this one
  command; it outranks `BOX_IMAGE_INDEX`.
- `--help` (or `box help`) prints the command list; `box <command> --help`
  (or `box help <command>`, `box session new --help`) prints that command's
  arguments and every flag with its default, and runs nothing.
- A `--` ends the flags: everything after it belongs to the command that
  follows (`box run dev1 -- df -h /` gives `df` its own `-h`).
- Exit codes: `0` ok, `1` error, `2` usage, `3` not found, `4` timeout (the
  thing is still running). `box run` and `box ssh` return the guest's own exit
  code.

The environment the CLI reads:

| variable | meaning |
| --- | --- |
| `NOMAD_ADDR`, `NOMAD_TOKEN`, `NOMAD_CACERT`, `NOMAD_CLIENT_CERT`, `NOMAD_CLIENT_KEY`, `NOMAD_NAMESPACE` | the usual Nomad API settings |
| `BOX_IMAGE_INDEX` | where the base image index lives; there is no built-in default, and without it (or `--image-index`) `box images` and `box create` say so and stop |
| `BOX_VAULT_ROLE` | the Vault role a box reads its secrets with; it is the default of `box create --vault-role`, and empty means the cluster's default role |

## Nodes, images, boxes

### `box capacity`

Which nodes can run boxes, and how much room each has: architecture, OS, status,
eligibility, free cpu, memory and disk, the disk space box volumes occupy, the
Lima instances Nomad does not know about, the driver and volume plugin versions,
and how many boxes the node runs. A node's free resources are reported *after*
subtracting the running VMs that no task owns.

### `box images`

The base image versions published in the index, newest first, and the platform
bundles published with them.

An image whose index entry says `"platform": "mount"` expects a platform bundle:
its `boxd`, `boxctl` and its executors come from that bundle, which is mounted
read-only at `/opt/box` when the box starts. An image without the field runs the
`boxd` it carries, and nothing of it changes.

### `box create <id> [flags]`

Creates a box: picks a node, creates its volume there, registers the job and
waits until the box is running.

| flag | default | meaning |
| --- | --- | --- |
| `--image V` | – (required) | base image version from the index |
| `--arch amd64\|arm64` | the node's | architecture |
| `--platform V` | the newest published for the architecture | the platform bundle to mount at `/opt/box`; only an image built for bundles takes one |
| `--cpus N` | 2 | guest vCPUs |
| `--memory-mib N` | 4096 | guest RAM: an agent session with its tools plus a build of the box's own work needs it, and a guest that runs out of memory stops answering its `boxctl` calls entirely |
| `--disk-gib N` | 8 | disk size (it can only grow later) |
| `--node NAME` | – | pin the box to this node |
| `--large` | false | allow more than a quarter of the node's CPU, memory or disk |
| `--vault-role ROLE` | `BOX_VAULT_ROLE`, else the cluster's default role | the Vault role the box reads its secrets with, used when the executors hold a template item (see "Executor credentials") |
| `--timeout D` | 10m | how long to wait for it to run |
| `--wait` | true | wait until it is running |

A box id is `[a-z0-9-]{1,40}`. A failed create removes the half-made box, so a
failure leaves no disk behind.

A node is shared by every agent's boxes, so one box takes at most a quarter of
a node's CPU, memory or disk: a larger request names the node and what it asked
for, and is refused unless `--large` is given. Start at the defaults and grow
with `box resize`, which holds to the same quarter.

An image built for bundles gets one mounted at `/opt/box` without asking: the
newest bundle for the box's architecture. `--platform <version>` picks an older
one, and `box upgrade` moves a box on later. The image that does not take a
bundle refuses `--platform` and says so, and an image that needs one is refused
when the index has no bundle for its architecture: a box never boots with half
of what its image expects.

### `box ls`

Every box with its state, node, allocation, image, size, the disk it really
occupies on the host, and its sessions.

### `box status <id> [--events N]`

One box in detail: state, task events (default 10), the platform bundle it
mounts, the guest and its executors, the sessions, the disk.

The guest is asked with a 20 second bound. A guest that does not answer - what a
guest out of memory does - costs one line ("the guest does not answer (no reply in
20s): ...", `guest_error` under `--json`) instead of the status: everything Nomad
knows is printed either way, and the exit code stays `0` unless the guest was the
whole of what the status had to show.

### `box stop <id> [--timeout D]` / `box start <id> [--timeout D] [--wait]`

Stop keeps the disk and frees the RAM. Start brings the same disk back.

### `box rm <id> [--yes] [--timeout D]`

Deletes the box **and its disk**: the VM is stopped, the instance deleted, the
volume and its directory removed. It asks first unless `--yes` is given, and it
refuses to ask from a non-interactive shell.

### `box resize <id> [--cpus N] [--memory-mib N] [--disk-gib N] [--timeout D] [--wait]`

Changes the size of a box. It is an **offline** change: the VM stops, is edited
and starts again. Memory must be at least 256 MiB, and the disk can only grow.
More than a quarter of the node needs `--large`, as with `box create`.

### `box upgrade <id> [--platform VERSION] [--timeout D] [--wait]`

Moves a box to the newest platform bundle, or to the one `--platform` names. It
re-registers the box job, so the box **restarts** -- its disk, its files and the
transcripts of its sessions are kept -- and comes back with the bundle mounted
at `/opt/box`. This is how a box picks up a new `boxd` or new executors without
a new image, and how a project image derived from a base image stays as it is.

The command needs the image index to know what the box's image expects:

- an image that does not take a bundle runs the `boxd` it carries; `box upgrade`
  refuses it and says why (the image has to be rebuilt for bundles first);
- a bundle version the index does not publish is a not-found error that lists
  the published ones;
- a box created before bundles existed has none: `box upgrade` gives it the
  newest one for its architecture.

`--wait=false` returns as soon as Nomad has taken the new job.

## Preparing a box

Before agents work in a box, the box gets its sources and the project's recipe
runs in them. boxd does it inside the guest, not a shell of the caller's, so it
goes on when the caller goes away and always says how it ended.

### `box prepare <id> [--repo URL [--ref R] | --from PATH] [--dest DIR] [--timeout D]`

- `--repo URL` clones the repository in the box (`--ref` checks out a branch,
  tag or commit). `--from PATH` sends a directory or a file from this machine
  instead, as a tar stream: no git is needed on either side.
- The sources go to `--dest`, by default `/work/<name>` (the repository's or the
  directory's name). A destination that exists is never written over.
- Then `.box/prepare.sh` in the sources runs, if there is one, as root with
  `BOX_WORKSPACE` set to the sources: what to install is the project's to say.
  The sources then belong to the box's user, so `box run` and `box cp` work in
  them.
- The command waits until it is done and prints what the step is doing while it
  runs. `--timeout` (default 1h) is the preparation's time limit in the box and
  how long the command waits; `--no-wait` returns at once.
- `box create` takes the same flags (`--repo`, `--ref`, `--from`, `--dest`,
  `--prepare-timeout`) and returns when the box is ready.

The states and the way out of each:

| state | sessions | out |
| --- | --- | --- |
| `none` | start | `box prepare --repo/--from` |
| `preparing` | refused, with the step and its last line | ends by itself at the time limit; `box prepare <id>` waits, `--cancel` stops it now |
| `failed` | refused, with the reason | `box prepare <id>` runs the recipe again on the sources there are (or new ones with `--repo/--from`); `--skip` lets sessions start without it |
| `ready`, `skipped` | start | `box prepare <id> --again` runs the recipe again; new sources go to a new `--dest` |

A box whose boxd restarts in the middle of a preparation has it `failed`: no
process is left to finish it. A failure prints its reason and the last lines
the step wrote; `box status` and the task log show them too. A session started
with no `--cwd` starts in the prepared sources.

## Inside a box

### `box ssh <id>`

An interactive shell in the box, over `alloc exec`. It needs a terminal.

### `box run <id> [--timeout D] -- cmd [args...]`

Runs one command in the box and returns its exit code and output. The command
runs in a session of its own in the guest: a `box run` that is interrupted
(Ctrl-C, SIGTERM, an outer `timeout`) or runs out of `--timeout` stops that
whole session before it returns, so nothing keeps running in the box with no
one to read how it ended. Nomad itself does not tell the box that the caller
went away while the command prints nothing. `--timeout` ends with exit 4.

### `box cp <src> <dst>`

Copies files or directories in or out, exactly one side inside a box:
`box cp ./dir dev1:/work/dir` and `box cp dev1:/work/out ./out`. It streams a
tar over `alloc exec`, refuses a `..` entry, and never writes outside the
destination.

### `box top <id> [-f]`

What the guest is doing, as boxd measures it every five seconds: CPU and the
part of it waiting on the disk (iowait), the load average, memory used and
available, the memory pressure the guest kernel reports (`some` and `full`
avg10), the disk's use, and the three processes with the most resident memory.
`-f` prints it again every five seconds; `box status` shows the same line as
`usage`.

A guest that does not answer within 20 seconds fails the command with exit code
`4` and a sentence that says what to look at (`box logs`, `box status`); under
`--json` the document carries `guest_error`. `box session ls` behaves the same
way. `box wait`, `box send`, `box run` and `box exec` are not bounded: they are
as long as the work they start.

### `box logs <id> [-f]`

The task log of the box, which is the boxd event stream from the guest.

## Agent sessions

A box can host several agent sessions. Each session is one executor process in
the guest with its own model, transcript and working directory.

### `box session new <id> --executor prime|opencode|claude --model <provider>/<model> [--cwd DIR] [--title T]`

### `box session ls <id>`

### `box session close <id> <session>`

Stops the executor process and keeps the history.

### `box send <id> <session> [--file F] [prompt...]`

Starts one turn and returns at once. A second send while a turn runs answers
`busy`.

### `box wait <id> <session> [--timeout D] [--turn N]`

Waits for the turn and prints its final text. On timeout it exits 4 and the turn
keeps running. `--turn N` waits for an older turn instead of the running one.

### `box events <id> <session> [--since N] [--limit N] [-f]`

The session's event feed. `--since` is a resumable cursor; `-f` follows it.

### `box cancel <id> <session>`

Cancels the running turn. The executor is killed after a short grace period and
the session resumes from its transcript on the next send.

## Executor credentials

### `box executors set --from-dir DIR --provider NAME [--executor NAME] [--dry-run]`

Stores the credentials and configuration the boxes start with. `DIR` holds one
subdirectory per executor (`prime/`, `opencode/`), or is one executor's own
directory with `--executor`. `--provider` keeps only that provider's credential
and definition, because a box runs one provider. It prints the keys it stores,
never a value. The files live in the Nomad variable `nomad/jobs/box-executors`
and are rendered into each box's `secrets/` directory, which the guest mounts
read-only at `/run/box/secrets/<executor>/`.

**An item whose name ends in `.tmpl` is a template, not a file.** Its text is
Nomad template text that reads the secret itself, for example
`prime/auth.json.tmpl`:

```
{"<provider>": {"type": "api_key", "key": {{ with secret "kv/data/box/llm" }}{{ .Data.data.key | toJSON }}{{ end }}}}
```

`box create` embeds that text in the box job and renders it to
`secrets/prime/auth.json`, the name without the suffix. The credential is
therefore read from Vault by Nomad, on the node, with the box's own workload
identity: it is in no host's files, not in the variable, and not in the job spec
— which holds the path. A template item must never contain the secret, and
`box executors set` refuses one that has no `{{` in it, or a `.tmpl` item next to
a plain item that renders to the same file.

A job with at least one template item gets a Nomad `Vault` block, with
`change_mode = "noop"` (a renewed or revoked token must not restart the VM) and
the role from `box create --vault-role` or `BOX_VAULT_ROLE`; without a template
item the job carries no Vault block at all. Nomad does not write its token files
into `secrets/` (`disable_file`), because the driver copies that whole directory
into the guest. `deploy/README.md` sets the Vault side up.

A required file is satisfied by either form: an `auth.json.tmpl` item is enough
for `prime`, which asks for `settings.json` and `auth.json`.

**Rotation needs no new box.** After `box executors set`, Nomad re-renders the
box's templates within seconds; the driver notices the changed task files, puts
them where the guest mounts them and runs `boxctl reload`. boxd re-reads the credentials
and stops the executor of every idle session, so the next turn runs with the new
configuration; a session in the middle of a turn restarts its executor when the
turn ends. The VM, the allocation and the transcripts are untouched. A box that
was stopped picks the new files up at its next start.

### `box executors show`

File names, sizes and executors, never a value. A template item is shown with
`(template)` next to its name, and as `template: Nomad reads the secret at render
time`, so what the variable holds is visible as a path. A required file counts as
present in either form: `prime/auth.json.tmpl` is the `auth.json` of `prime`.
A provider definition is shown with the models staged in it, as `--model` takes
them (`prime/models.json provider myprovider: models come from the provider, fallback list: myprovider/model-a`);
`--json` has them as `models` of that key. The list a box offers is the one its
provider gives its key when a session starts (see `docs/guest-image.md`, "Model
catalog"); the staged list is the fallback, and its sizes are used for a model
the provider names without one.

### `box executors delete [--yes]`

Removes the variable. Boxes that already started keep the credentials they got
until they restart.

## See also

- `docs/architecture.md` — how the parts fit together
- `docs/boxctl-contract.md` — the guest side (`boxctl`, boxd, JSON objects)
- `deploy/README.md` — installing the platform
- `claude-plugin/skills/box/SKILL.md` — the skill an agent reads
