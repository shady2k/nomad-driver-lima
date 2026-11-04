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

The base image versions published in the index, newest first.

### `box create <id> [flags]`

Creates a box: picks a node, creates its volume there, registers the job and
waits until the box is running.

| flag | default | meaning |
| --- | --- | --- |
| `--image V` | – (required) | base image version from the index |
| `--arch amd64\|arm64` | the node's | architecture |
| `--cpus N` | 2 | guest vCPUs |
| `--memory-mib N` | 2048 | guest RAM: an agent session with its tools needs it |
| `--disk-gib N` | 8 | disk size (it can only grow later) |
| `--node NAME` | – | pin the box to this node |
| `--vault-role ROLE` | `BOX_VAULT_ROLE`, else the cluster's default role | the Vault role the box reads its secrets with, used when the executors hold a template item (see "Executor credentials") |
| `--timeout D` | 10m | how long to wait for it to run |
| `--wait` | true | wait until it is running |

A box id is `[a-z0-9-]{1,40}`. A failed create removes the half-made box, so a
failure leaves no disk behind.

### `box ls`

Every box with its state, node, allocation, image, size, the disk it really
occupies on the host, and its sessions.

### `box status <id> [--events N]`

One box in detail: state, task events (default 10), the guest and its
executors, the sessions, the disk.

### `box stop <id> [--timeout D]` / `box start <id> [--timeout D] [--wait]`

Stop keeps the disk and frees the RAM. Start brings the same disk back.

### `box rm <id> [--yes] [--timeout D]`

Deletes the box **and its disk**: the VM is stopped, the instance deleted, the
volume and its directory removed. It asks first unless `--yes` is given, and it
refuses to ask from a non-interactive shell.

### `box resize <id> [--cpus N] [--memory-mib N] [--disk-gib N] [--timeout D] [--wait]`

Changes the size of a box. It is an **offline** change: the VM stops, is edited
and starts again. Memory must be at least 256 MiB, and the disk can only grow.

## Inside a box

### `box ssh <id>`

An interactive shell in the box, over `alloc exec`. It needs a terminal.

### `box run <id> -- cmd [args...]`

Runs one command in the box and returns its exit code and output.

### `box cp <src> <dst>`

Copies files or directories in or out, exactly one side inside a box:
`box cp ./dir dev1:/work/dir` and `box cp dev1:/work/out ./out`. It streams a
tar over `alloc exec`, refuses a `..` entry, and never writes outside the
destination.

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
and are rendered into each box's `secrets/` directory, which the driver writes
to the guest's tmpfs at `/run/box/secrets/<executor>/`.

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
box's templates within seconds; the driver notices the changed task files, copies
them into the guest again and runs `boxctl reload`. boxd re-reads the credentials
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
them (`prime/models.json provider myprovider, staged models: myprovider/model-a`);
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
