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

The images a box can be created from, newest first -- the base images published in
the index, the **project images** published with them, and the **drafts** the
nodes baked -- and the platform bundles published with the base images.

An image's row:

| column | what it holds |
| --- | --- |
| `VERSION` | the image's version: `1.0.8` for a base image, `NAME-N` for a project image |
| `ARCH` | `amd64` or `arm64` |
| `KIND` | `base`, or `project` for an image baked from a box |
| `PARENT` | the image a project image was baked on top of; `-` for a base image |
| `BEHIND` | how far a project image's parent is behind the newest base image: `parent 1.0.8, newest base 1.0.12`, or `parent 1.0.8 (newest base)` |
| `SIZE` | the compressed size of the image file |
| `SHA256` | the first 12 characters of its digest |
| `WHERE` | `published`, or `draft on <node>` |

Below the table come the counts, the `box create` line for the newest base image,
lines that say where a draft is created from and how to publish it, and -- when
a project image or a draft was grown from an older base image -- the `box bake
--rebuild` that replays it on the newest one.

A **draft** exists on one node only: the bake wrote it into that node's image
cache, and the driver publishes its record as node attributes. `box create` from
a draft pins the box to that node; `box images publish` makes it available
everywhere. A cluster with no image index still lists the drafts, with a line
that says why there are no published images: baking comes before publishing.

`latest` and the newest-version hint never pick a project image: a box created
with `--image latest` is always a base image.

`--json` gives `index_url`, `available`, `updated_at`, `images` (the base and
published project images), `drafts`, `platforms` and, when the index could not be
read, `error`.

An image whose index entry says `"platform": "mount"` expects a platform bundle:
its `boxd`, `boxctl` and its executors come from that bundle, which is mounted
read-only at `/opt/box` when the box starts. An image without the field runs the
`boxd` it carries, and nothing of it changes.

### `box images publish <version> [--yes] [--timeout D]`

Publishes a draft into the operator's store, which is what makes an image
available on every node. Publishing distributes it and a published version never
changes, so the command refuses without `--yes`; the skill an agent follows says
it asks its owner first.

It reads the drafts of the nodes and refuses a version no node holds, naming the
drafts there are. Then it registers a batch job `box-publish-<version>` of the
driver, pinned to the node that baked the draft -- the image is in that node's
cache and nowhere else -- and waits for it. The task does what
`images/publish.sh` does, in the same order and with the same lock:

1. takes the store's lock, the object `box-base/.lock`: a record that says who
   holds it and when, written and read back, refreshed after every upload, and
   released at the end. A fresh lock stops the publish before anything is
   written ("held by ..."), and a lock older than 30 minutes is taken over, so a
   crashed run cannot block the store for ever;
2. refuses the version when the store already holds any of its files, and when
   `index.json` already names it;
3. checks that the file in the node's cache still is the image its record names
   -- a store that cannot take an upload back is not given a file that changed
   under it -- then uploads the image, its `.sha256` and its manifest under
   `box-base/<version>/<arch>/`, with the names `images/publish.sh` writes. A
   store or proxy that answers 413 to a body of a declared size gets the same
   bytes in chunked form;
4. reads the image back and compares its sha256 (and its size, and the two small
   files byte for byte) before the index names it;
5. re-reads `index.json`, merges the entry, writes it and reads it back: this
   entry is there and every entry that was there before survived, with the
   digest it had.

The store's url, its token, the header that token goes in and the scheme in front
of it reach the task as templated files, out of the variable `box store set`
writes (below); the token is never in the job spec, in a task event or in a log.

The job reads that variable through `deploy/acl/box-store.policy.hcl` attached
to that job, never through a box's policy, and the job is purged when it ends.

It prints the version, the architecture, the sha256, the store, the image's url
and the index's url, and the `box create` line for the image. `--json` gives
`version`, `arch`, `sha256`, `size`, `store`, `url`, `index_url` and `node`.

Exit codes are the page's: `0` published, `1` the job failed or no store is
configured, `2` usage, `3` no node holds that draft, `4` the job did not finish
within `--timeout` (default 30m).

### `box bake <id> --name NAME [--recipe FILE | --no-recipe] [--yes] [--timeout D]`

Turns a box into an image: the box is sealed from inside, stopped, and its disk
is baked into the node's image cache as a draft (`docs/guest-image.md`, "Project
images"). Those are the only flags.

A bake is refused, with a sentence that says why, when:

- the box's image takes no platform bundle (`"platform"` is not `mount`): boxd and
  the executors come from the node's `/opt/box`, so an image that carries its own
  boxd cannot be baked;
- the box is not running: the seal runs inside it;
- `--recipe` and `--no-recipe` are both given: they say opposite things;
- `--yes` is missing.

Without `--yes` the command names the image it would make and says what the seal
does: it deletes `/work` and every session in the box, and the box cannot be
sealed twice. Push the work out of the box first.

The recipe is the script that makes the box's environment from its parent image.
It comes from `--recipe FILE`, or else from the record of the image the box was
created from, and the command says which. `--no-recipe` bakes an image with none:
it can be created from and never rebuilt. A recipe is at most 64 KiB; a longer
script belongs in the project's sources, which the recipe prepares.

| flag | default | meaning |
| --- | --- | --- |
| `--name NAME` | – (required) | the project the image belongs to: `NAME-1`, `NAME-2`, ... |
| `--recipe FILE` | the box's image record | the script that makes this environment from the parent image |
| `--no-recipe` | false | bake without a recipe; such an image cannot be rebuilt |
| `--yes` | false | seal the box without asking |
| `--timeout D` | 30m | how long to wait for the bake job |

The steps, in this order:

1. `boxctl seal` in the guest: it deletes the work, the sessions and the
   credentials, and proves the result. Its lines go to stderr as they happen (not
   under `--json`), and a failed seal fails the command with the seal's own
   message.
2. `box stop` the box. A sealed box is not started again: it is an empty, clean
   machine with the image's files, not the work.
3. The batch job `box-bake-<id>`, which mounts the box's volume read-only -- the
   volume also places the job on the node that holds the disk. It runs no VM: it
   compresses the disk with `qemu-img` and records the image. A node without
   `qemu-img` fails it with a sentence.
4. The job is purged, and the result is printed: version, sha256, size, node, and
   the next steps.

The box is not deleted. It stays stopped with its disk, and `box rm <id> --yes`
removes it when its work is out of it.

The version is `NAME-N`, with `N` one above the highest number among the versions
of that name in the index and the drafts of every node, so a draft and a published
image never share a number. `NAME` is 1 to 63 lower-case letters, digits and
dashes, starting and ending with a letter or a digit, and it may not look like a
base image's version (`1.0.0`, `v2`).

`--json` gives `box` (the sealed box), `version`, `arch`, `name`, `parent`,
`sha256`, `size`, `virtual_bytes`, `node` and `draft`.

Exit codes are the page's: `0` baked, `1` the seal, the stop or the job failed,
`2` usage, `4` the job did not finish within `--timeout` (it keeps running).

### `box bake --rebuild NAME [--base VERSION] [--timeout D]`

Replays an image's recipe on a newer parent. `NAME` is a project image's name or
version; without `--base` the newest base image is the parent.

It refuses an image that carries no recipe: a rebuild replays the recipe that made
the environment, and an image baked with `--no-recipe` has none.

Then it creates a temporary box `NAME-rebuild` from the base image, runs the
recipe in it with the machinery of `box run` -- its output on the terminal, and no
time limit other than `--timeout` (default 1h) -- bakes the result as the next
`NAME-N` with the new parent, and deletes the temporary box. On a failure it keeps
the box and says so: what the box ran is the way to see why (`box logs`, then `box
rm NAME-rebuild --yes`).

It publishes nothing: that is `box images publish`. `--yes` is accepted and
unnecessary, and the command says so: the only box it seals is its own temporary
one.

### `box store set --url URL --from-file FILE | --template FILE`

Writes the operator's image store, which a publish reads: the Nomad variable
`nomad/jobs/box-store`, holding the store's base url and either its write token or
template text that reads the token at render time.

- `--url URL` is required and must be an http(s) url. The `box-base/` layout is
  written below it.
- `--from-file FILE` stores the file's content as the token, in the variable.
- `--template FILE` stores the file's text as a Nomad template, for example
  `{{ with secret "kv/data/box/store" }}{{ .Data.data.token }}{{ end }}`: the
  token is then read from Vault when the publish task renders, and even the
  variable holds a path. The file must contain `{{`, and it must never contain the
  token; this is the form to prefer.
- `--token-header NAME` names the request header the store reads the token in:
  `Authorization` when it is left out.
- `--token-scheme WORD` is the word in front of the token, `Bearer` when it is
  left out. Give it empty (`--token-scheme ""`) for a store that wants a bare key
  header; that is a store with a scheme item that is empty, which is not the same
  as one without the item.

The two are `BOX_STORE_TOKEN_HEADER` and `BOX_STORE_TOKEN_SCHEME` of
`images/publish.sh`, which publishes to the same store. Giving both `--from-file`
and `--template` is a usage error, and so is an empty file. The command prints the
url, how the token reaches a publish (`read from Vault when the task renders`,
`stored in the Variable`, or `none`), the header and scheme it will use, and the
item names. It never prints a value.

### `box store show`

The same lines for the store that is configured, or one line saying there is none:
the url, how the token reaches a publish, the header and scheme, and the item
names. `--json` gives `url`, `token` (`variable`, `template`, or empty for a store
written without authentication), `header`, `scheme`, `items` and `configured`; the
token is never a value here either.

A publish job reads that variable with a workload policy of its own
(`deploy/acl/box-store.policy.hcl`); `deploy/acl/README.md` shows how it is
attached to the job.

### `box create <id> [flags]`

Creates a box: picks a node, creates its volume there, registers the job and
waits until the box is running.

| flag | default | meaning |
| --- | --- | --- |
| `--image V` | – (required) | a published version, a draft's version (`NAME-N`), or a project's name, which means the newest version of it |
| `--arch amd64\|arm64` | the node's | architecture |
| `--platform V` | the newest published for the architecture | the platform bundle to mount at `/opt/box`; only an image built for bundles takes one |
| `--cpus N` | 2 | guest vCPUs |
| `--memory-mib N` | 4096 | guest RAM: an agent session with its tools plus a build of the box's own work needs it, and a guest that runs out of memory stops answering its `boxctl` calls entirely |
| `--disk-gib N` | 8 | disk size (it can only grow later) |
| `--node NAME` | – | pin the box to this node |
| `--vault-role ROLE` | `BOX_VAULT_ROLE`, else the cluster's default role | the Vault role the box reads its secrets with, used when the executors hold a template item (see "Executor credentials") |
| `--timeout D` | 10m | how long to wait for it to run |
| `--wait` | true | wait until it is running |

A draft is created from on its own node: `--image NAME-N` pins the box to the node
that holds the image, and `--node` must name that node or be left out. Its url in
the job is `file:///box-draft/<version>`, which is never fetched: the
node's cache already has the file, keyed by the record's sha256. A node that lost
the file fails the box, and the driver's task event says which image it could not
find. A `--disk-gib` below the image's virtual size is refused,
and the default disk grows to at least that size: an image's disk is the floor of
a box's disk.

`--image NAME` without a number is the newest version of that project, among the
published images and the drafts.

A box id is `[a-z0-9-]{1,40}`. A failed create removes the half-made box, so a
failure leaves no disk behind.

**The node is shared, and free resources are there to be used.** A box is
created whatever its size, as long as the node has the room: nothing here refuses
a box for taking a large part of a node. When a box takes more than a quarter of
its node's CPU, memory or disk, the command says so on stderr -- `this box takes
8 of 16 vCPU on node X; the node is shared: box stop or box rm it when the work
is done` -- and the exit code is the same as without the note.

Choose the size for the task when the box is created. The default (2 vCPU,
4096 MiB, 8 GiB) is for light work without a build; a build or a test run needs
more memory from the start. When in doubt take more: a box can be stopped, but a
guest out of memory cannot be noticed from inside -- it freezes, `box top` and
`box status` stop answering, and the host loses several cores -- so do not wait
for `box top` to show a shortage. `box resize` is for growth you can see coming,
between turns, and for a box whose guest has gone silent (it restarts it; the disk
is kept). Give back what you do not use: `box stop` an idle box and `box rm` a
finished one, because a reservation held by an idle box keeps someone else's work
from starting. If there is no room, stop your own idle boxes or tell the owner;
do not wait silently.

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

Growing the disk of a running box stops it first: Nomad does not update a volume
in use. If the volume cannot grow, the box runs again with its old size.

Changes the size of a box. It is an **offline** change: the VM stops, is edited
and starts again. Memory must be at least 256 MiB, and the disk can only grow.
A box that takes more than a quarter of its node is resized anyway and said on
stderr, as with `box create`; grow a box between turns, and use `box resize` to
bring back a guest that has gone silent (it restarts it, the disk is kept).

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
