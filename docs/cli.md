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
  thing is still running), `5` unknown (contact with the box was lost and the
  turn or the command may still run: see "When the connection is lost"), `6` the
  turn ended but left work running in the box (`box wait --strict`). `box run`
  and `box ssh` return the guest's own exit code.

The environment the CLI reads:

| variable | meaning |
| --- | --- |
| `NOMAD_ADDR`, `NOMAD_TOKEN`, `NOMAD_CACERT`, `NOMAD_CLIENT_CERT`, `NOMAD_CLIENT_KEY`, `NOMAD_NAMESPACE` | the usual Nomad API settings |
| `BOX_IMAGE_INDEX` | where the base image index lives; there is no built-in default, and without it (or `--image-index`) `box images` and `box create` say so and stop |
| `BOX_VAULT_ROLE` | the Vault role a box reads its secrets with; it is the default of `box create --vault-role`, and empty means the cluster's default role |
| `BOX_PROJECT` | the project a box belongs to when `--project` is not given; empty means the name of the git repository of the directory, if it has one (see "Projects and idle boxes") |

## Nodes, images, boxes

### `box capacity`

Which nodes can run boxes, and how much room each has: architecture, OS, status,
eligibility, free cpu, memory and disk, the disk space box volumes occupy, the
Lima instances Nomad does not know about, the driver and volume plugin versions,
and how many boxes the node runs. A node's free resources are reported *after*
subtracting the running VMs that no task owns.

`vCPU` is how many vCPUs the node offers -- its total CPU divided by what one
vCPU reserves -- and `OVERCOMMIT` is how many vCPUs its cores are shared out to
(`cpu_overcommit` in the driver's plugin block, 1 on a node that does not set
it). With overcommit 2 an 8-core node offers 16 vCPUs, so it takes eight boxes of
2 vCPU instead of four. A vCPU's share of a core is rounded down, so a node never
offers fewer vCPUs than its overcommit says (a 6-core node of 4349 MHz cores at
overcommit 2 offers 12 vCPUs of 2174 MHz). Nothing else changes: a box keeps its vCPUs, its memory
and its `cpu.max` cap (its vCPUs plus one core), so a busy box cannot take the
node, and memory is never overcommitted -- a guest out of memory freezes.

Each running box's line says how long it has been idle: `idle 3h12m` since its
last turn ended, or `busy` while a turn runs. `?` is a guest that did not answer
and `-` a boxd too old to report it. This is a report and nothing more: no box is
ever stopped for being idle.

Each box's disk line also says which project the box belongs to
(`(dev1 is running, project my-project)`), and the disks of `--json` carry it as
`project`: the nodes are shared, so whose box holds a disk is part of the picture.

A disk whose box is not running says when it is deleted: "its disk is deleted in
22h", "on the next reap pass" when its term has run out, or that no pass has seen
the box stopped yet. A stopped box is deleted after its term, 24 hours by default
(`box reap term` shows it, `box reap term set 48h` sets another), and so is an
`orphan` disk with a Nomad volume. A term that cannot be read says so: the time is
unknown, not the default.

A VM with no task is an orphan to the driver after ten minutes, and that is what
every stopped box looks like to it. So this command answers as `box ls` does: a box
with a job or a stored description is named with its state from there -- a stopped
box is `stopped`, and the line reads "no box task, and the cluster knows this box as
stopped" -- and only a VM that is neither is an orphan. A Lima instance with no
Nomad volume is reported here and never removed; one with a volume is deleted by
`box reap` like a stopped box.

The free room of a node is the free room of the filesystem that holds its Lima
home, which is where a box's disk and a bake's copy of it are written.

**Two disk columns, and only one of them is free space.** `FREE disk (driver)` is
the free space the node's box driver measures on that filesystem, and the number a
box disk grows into; `unknown` means the driver has not reported it (an older
driver, or a filesystem it could not measure), never that the node is roomy.
`DISK free/total (scheduler)` is Nomad's scheduling arithmetic -- the node's total
less what it reserves and what its allocations take -- and it stays large on a node
whose filesystem is full, so it is no measure of free space. `disk_mib` in
`--json` is that same arithmetic, kept for compatibility; the free space is
`disk_free_bytes`, with `disk_free_known` saying whether there is one,
`disk_total_bytes` the size of the filesystem, and `disk_reserve_bytes` the part of
it the node keeps for itself (see `disk_reserve_gib` and `disk_reserve_percent` in
the driver's plugin block). The summary line of the per-architecture subtotals
gives the drivers' free space and says what Nomad's figure is, so the scheduler's
sum is never read as free disk.

A node below its reserve is printed as `LOW DISK` on stderr, with what is free,
what it keeps back and what that means: a new box is refused there, and a running
VM freezes when the free space reaches zero. A node whose driver did not report its
free space is listed in one line of its own, because "unknown" is not "roomy". A node whose
image cache is on another filesystem also reports that room, as
`disk_image_cache_free_bytes` in `--json` (the node attribute
`driver.box.disk.image_cache_free_bytes`), because a bake writes the image there
and the two needs do not share one pool. The field is left out when the cache is
under the Lima home, and then a bake's copy and image both come out of
`disk_free_bytes`.

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

### `box bake <id> --name NAME [--version NAME-N] [--recipe FILE | --no-recipe] [--timeout D]`

A project image version can contain one artifact for each architecture. Bake the
same `--version` from an amd64 box and an arm64 box to build one cross-architecture
release; the version is immutable per architecture, so an existing version for
that box's architecture is refused, while the other architecture may still be
added. Without `--version`, the CLI chooses the next unused project version across
the index and drafts on all nodes.

Turns a box into an image without emptying the box: a copy of its disk is sealed
and baked into the node's image cache as a draft, and the box carries on with its
work (`docs/guest-image.md`, "Project images"). Those are the only flags.

A bake is refused, with a sentence that says why, when:

- the box's image takes no platform bundle (`"platform"` is not `mount`): boxd and
  the executors come from the node's `/opt/box`, so an image that carries its own
  boxd cannot be baked;
- a turn runs in the box, or a preparation is in progress: the seal would refuse
  it, and the box is asked before anything is stopped;
- the node cannot hold both the copy of the box's disk and the image: a bake
  clones the disk before it seals it and compresses that copy, and the refusal
  says how much room there is, how much the copy needs and how much the image may
  need, and names the directory or directories;
- `--recipe` and `--no-recipe` are both given: they say opposite things.

A bake destroys nothing, so it asks nothing: `--yes` is not one of its flags. A
refusal stops nothing and bakes nothing.

The recipe is the script that makes the box's environment from its parent image.
It comes from `--recipe FILE`, or else from the record of the image the box was
created from, and the command says which. `--no-recipe` bakes an image with none:
it can be created from and never rebuilt. A recipe is at most 64 KiB; a longer
script belongs in the project's sources, which the recipe prepares.

| flag | default | meaning |
| --- | --- | --- |
| `--name NAME` | – (required) | the project the image belongs to: `NAME-1`, `NAME-2`, ... |
| `--version NAME-N` | next unused version | exact project image version; may add a different architecture to an existing version |
| `--recipe FILE` | the box's image record | the script that makes this environment from the parent image |
| `--no-recipe` | false | bake without a recipe; such an image cannot be rebuilt |
| `--timeout D` | 30m | how long to wait for the bake job |

The steps, in this order:

1. `boxctl info` in the box, a call that changes nothing: a turn running or a
   preparation in progress refuses the bake with a sentence, and nothing is
   stopped. The check happens here because the clone's own boxd is a fresh daemon
   and cannot know about the box's turn. A box that is already stopped skips this
   step: it has no turn to lose.
2. The room on the node, against the space the box's disk occupies there: the
   clone's copy plus the 1 GiB a booted clone writes before the seal trims it,
   and the image the compression writes beside it. The two come out of one pool
   when the Lima home and the image cache are on one filesystem, and each of its
   own otherwise.
3. `box stop`, on the path `box stop` takes, which records that the box was
   running: a clone of a running box is a copy of a disk being written.
4. The batch job `box-bake-<id>`, which mounts the box's volume -- the volume also
   places the job on the node that holds the disk. The mount is read-write because
   Nomad does not place a read-only request on a `single-node-writer` host volume;
   the task reads the box's disk and writes only the image in the cache. In one
   driver task it clones the box as `<id>-bake-<n>` -- a full copy of the stopped
   instance's disk, a new hostname, no snapshot (measured with Lima 2.2.0) --
   starts the clone with the platform bundle at `/opt/box` and two empty
   directories where `boxd.service` waits for its mounts (`/run/box/secrets` and
   `/etc/box/local`), runs `boxctl seal` in it through `limactl shell`, stops it,
   compresses its disk into the node's image cache with `qemu-img convert -c`,
   writes the draft's record, and deletes the clone again -- also when a step
   fails or the task is stopped. The clone carries none of the box's task files
   and none of its credentials, so the machine the seal empties never sees a
   credential, and the seal -- which refuses a guest that has the box's credentials
   mounted -- has nothing to refuse here. A clone that a killed driver left behind
   is removed by the next bake of the same box, and until then the driver reports
   it as an orphan. Every step of the driver, and every line of the seal, is
   printed while it runs. A node without `qemu-img` fails the task with a sentence.
5. Whatever the bake answered, the box starts again if it was running, and the
   command says so: same disk, `/work` and the sessions are still there. A box that
   was stopped stays stopped. A box that does not come back is an error naming
   `box start <id>`: its disk is untouched, so that command runs it again with its
   work.

The box is not deleted and not emptied: it keeps its disk, its `/work` and its
sessions, and `box rm <id> --yes` deletes it -- or `box reap`, after the box's term
(24 hours by default). The bake job is purged when it ends, and the task reserves
the box's own CPU and memory plus a core for the converter, because it boots a VM
as big as the box.

The version is `NAME-N`, with `N` one above the highest number among the versions
of that name in the index and the drafts of every node, so a draft and a published
image never share a number. `--version NAME-N` instead requests that exact version;
it is refused if that version already exists for the box's architecture, but can
add another architecture to that version. A version may therefore have separate
amd64 and arm64 artifacts; each is immutable once present. Without `--version`,
allocation remains global across architectures and drafts. `NAME` is 1 to 63 lower-case letters, digits and
dashes, starting and ending with a letter or a digit, and it may not look like a
base image's version (`1.0.0`, `v2`).

`--json` gives `box` (the box that baked, when it runs again), `restarted`,
`version`, `arch`, `name`, `parent`, `sha256`, `size`, `virtual_bytes`, `node` and
`draft`.

Exit codes are the page's: `0` baked, `1` the seal, the stop, the job or the
restart failed, `2` usage, `4` the job did not finish within `--timeout` (it keeps
running).

### `box bake --rebuild NAME [--base VERSION] [--timeout D]`

Replays an image's recipe on a newer parent. `NAME` is a project image's name or
version; without `--base` the newest base image is the parent.

It refuses an image that carries no recipe: a rebuild replays the recipe that made
the environment, and an image baked with `--no-recipe` has none.

Then it creates a temporary box `NAME-rebuild` from the base image -- with the
project this command resolves, like any box created here: a rebuild that fails
keeps that box, and it is then the caller's own to find and to remove -- runs the
recipe in it with the machinery of `box run` -- its output on the terminal, and no
time limit other than `--timeout` (default 1h) -- bakes the result as the next
`NAME-N` with the new parent, and deletes the temporary box. On a failure it keeps
the box and says so: what the box ran is the way to see why (`box logs`, then `box
rm NAME-rebuild --yes`).

It publishes nothing: that is `box images publish`. It takes `--base` and
`--timeout` only: the image names itself, and its recipe is the one it carries.

The bake is the one `box bake <id>` runs, on the temporary box: it works on a
clone, and the temporary box is not started again, because the command deletes it.

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
| `--project NAME` | `BOX_PROJECT`, else the git repository's name | the project the box belongs to: a label `box ls` and `box capacity` show, so an agent can tell its boxes from other projects' (see "Projects and idle boxes") |
| `--timeout D` | 10m | how long to wait for it to run |
| `--wait [D]` | true | wait until it is running; with a duration, also wait that long for room |

A new box's job is shown in Nomad as `<project>/box-<id>`: the jobs page has no column for its meta, and the job's id stays `box-<id>` for every command and link. A box without a project keeps `box-<id>`, and a box keeps the name it was created with: a resize, an upgrade or a start does not change it.

**`--wait 30m` waits for room.** `box create` refuses at once when nothing fits,
and room is a moving target: another box finishes, another agent creates one, a
`box stop` frees a node's memory. With `--wait D`, a refusal *for lack of room*
(CPU, memory or disk on the candidate nodes) is retried: the reason goes to stderr
once at the start and again every 5 minutes, the capacity is read again every 30
seconds, and the placement is the CLI's normal choice each time. After D the
command ends with the refusal it would have given at once, and its exit code.

Waiting is for room only. A wrong architecture, a missing image, an unhealthy
driver, a node without the box-volume plugin or a usage error ends the command
immediately: waiting changes none of them. `--wait` alone is what it always was
(wait for the box to run); `--wait=false` returns as soon as the job is
registered. `box start` and `box resize` do not take a duration: they re-register
the box's own job on its node, where Nomad, not the CLI, places it.

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
8 of 16 vCPU on node X; the node is shared: box rm <id> --yes when the task
is done` -- and the exit code is the same as without the note.

Choose the size for the task when the box is created. The default (2 vCPU,
4096 MiB, 8 GiB) is for light work without a build; a build or a test run needs
more memory from the start. When in doubt take more: `box resize` gives a box
more between turns, but a guest out of memory cannot be noticed from inside -- it
freezes, `box top` and `box status` stop answering, and the host loses several
cores -- so do not wait for `box top` to show a shortage. `box resize` is also for
a box whose guest has gone silent (it restarts it; the disk is kept). Give back
what you do not use: `box rm <id> --yes` a box whose task is done, because a
reservation held by an idle box keeps someone else's work from starting. If there
is no room, delete your own finished boxes or tell the owner -- the refusal names
them (`your idle boxes: a 2h ...`, "Projects and idle boxes") -- or let the command
wait for room itself with `--wait 30m` (below), which says on stderr what it is
waiting for; do not sit in a retry loop.

A created box ends with one more line on stderr: it is for this task, the results
go to git, and `box rm <id> --yes` deletes the box and its disk when the task is
done; a box that is not running has its disk deleted after its term (24 hours by
default). It is advice for the end of the task, and the exit code is the same with
it and without it.

An image built for bundles gets one mounted at `/opt/box` without asking: the
newest bundle for the box's architecture. `--platform <version>` picks an older
one, and `box upgrade` moves a box on later. The image that does not take a
bundle refuses `--platform` and says so, and an image that needs one is refused
when the index has no bundle for its architecture: a box never boots with half
of what its image expects.

### `box ls [--project NAME | --mine]`

Every box with its project, state, node, allocation, image, size, the disk it
really occupies on the host, when its disk is deleted, and its sessions.

The `PROJECT` column is the project the box belongs to, and `-` for a box that
carries no label: nothing fails for such a box. `--project NAME` shows only that
project's boxes, and `--mine` shows the boxes of the project this directory
resolves to (`BOX_PROJECT`, else the name of its git repository). A box with no
label is never one of a project's boxes, and `--mine` in a directory that belongs
to no project is a usage error that says how to name one. The project is part of
`--json` as `boxes[].project`, absent for a box that has none.

The `IDLE` column says what a running box has been doing: `idle 3h12m` since its
last turn ended, or `busy` while a turn runs. It comes from the guest -- `boxctl
info`, under the 20 second bound every guest call has, one call per running box in
parallel, so a table of boxes costs one bound and not one each. `?` is a guest
that did not answer, `-` a box that is not running or a boxd from an older image
that does not report it. Under `--json` it is `activity: {"busy",
"idle_seconds"}`, and `guest_error` says why there is none. Nothing is stopped for
being idle: the number is what an owner needs to decide.

A box is its volume and its disk, not its job: Nomad deletes a dead job after
`job_gc_threshold` (4 hours by default), and the box is still there. `box ls`,
`box status` and `box capacity` show it as `stopped` from the description kept in
the Nomad variable `nomad/jobs/box-<id>/spec`. A disk with neither a job nor a
description - a box made before boxes kept one - is shown as `orphan`: `box rm
<id> --yes` deletes it, and there is no way to start it again.

The `DISK UNTIL` column says when the disk of a box that is not running is
deleted: how long is left, `due` when the term has run out and the next pass
deletes it, `-` for a box that runs (a running box is never touched, however long
it idles), `not seen` for a box no pass has seen not running yet, and `unknown`
when the cluster's term cannot be read. The same two times are `stopped_at` and
`removes_at` under `--json`, and a box with no pass yet has neither field: the
command says so instead of inventing a time. `removes_at` is computed from the
term the cluster has now, so a box already stopped follows a change to it.

### `box status <id> [--events N]`

One box in detail: state, task events (default 10), the platform bundle it
mounts, the guest and its executors, the sessions, the disk. A box that is not
running also has a `disk` line: when its disk is deleted ("deleted at
2026-11-03T11:00:00Z, 24 hours after it stopped (in 23h)" on a cluster at the
default term; it names the real term otherwise), or that no pass has seen it
stopped yet. A term that cannot be read says so and the command still prints the
rest of the box: it does not fail and does not invent a time. Under `--json` it is
`disk_term` with `stopped_at`, `removes_at` and the sentence. A box that runs has
none: it is never on a term.

A session whose turn ended in an error while the executor went on working shows
as `idle (active outside a turn)`, with `executor_active` (the seconds since its
last update) under `--json`; `box session ls` shows the same state.

When processes are running in the guest and no turn is in progress in any
session, the guest's block carries one line for them: `12 work process(es) are
running with no turn in progress: cc1 -c big.c (pid 341, 3h2m, 2 children,
1120.4s CPU); ...`. They are what an earlier turn, an earlier `boxd` or a person
left running, and what the box's sessions do not account for: a box can be busy
with every session idle. The field is `leftover` under `--json`
(`{"count","processes","more"}`). While a turn runs the line is not printed at
all: a process of a running turn cannot be told from the work it was asked to do.

The guest is asked with a 20 second bound. A guest that does not answer - what a
guest out of memory does - costs one line ("the guest does not answer (no reply in
20s): ...", `guest_error` under `--json`) instead of the status: everything Nomad
knows is printed either way, and the exit code stays `0` unless the guest was the
whole of what the status had to show.

### `box stop <id> [--timeout D]` / `box start <id> [--timeout D] [--wait]`

**A box is for one task, and `box stop` is a pause, not storage.** Stop frees the
RAM and keeps the disk for its term, **24 hours by default**: a box that is not
running has its disk deleted after its term, whether you stopped it, Nomad did or
it crashed. `box start` brings the same disk back in a minute or a day later, as
long as the term has not run out; after that the disk is gone. There is no
extension and no per-box setting: the term is one value for the whole cluster
(`box reap term`, below), and a disk worth keeping is baked into an image with
`box bake`. Get the results out (push a branch, or `box cp`) before the pause, and
delete the box with `box rm <id> --yes` when the task is done.

While the box exists, `box start` registers the stored job again, with the
executor credentials that are staged now and the CPU reservation of the box's
node; on a box that is already running it only refreshes that description. The
description is the Nomad variable `nomad/jobs/box-<id>/spec`, which no garbage
collection removes, so a start works after Nomad has collected the dead job. A
node whose `cpu_overcommit` changed takes effect at the box's next `box start`,
`box resize` or `box upgrade`: an existing box keeps the reservation it was made
with, so an operator can raise the variable and refill the node without touching
the boxes already on it.

### `box rm <id> [--yes] [--timeout D]`

Deletes the box **and its disk**: the VM is stopped, the instance deleted, the
volume and its directory removed, and the stored description and the stopped
clock of the box deleted with it. It works when Nomad no longer has the job, and
on an `orphan` disk. It asks first unless `--yes` is given, and it refuses to ask
from a non-interactive shell.

This is the last step of a task: push the results to git (or copy them out with
`box cp`) first, then delete the box, because nothing on the disk survives it.

When the disk is deleted and the description cannot be, the command says it is
**partial** instead of reporting a failure of the whole: the box is gone, the
record is left, and `box ls` still lists a box with no disk. Exit code `1`, as
for any failure, with the sentence that says what to do:

```
box: the box dev1 is gone -- its job and its disk were deleted -- but its
description nomad/jobs/box-dev1/spec is left behind, so `box ls` still shows it
and `box start dev1` would find a box with no disk. Delete the record with
`nomad var delete nomad/jobs/box-dev1/spec` (Nomad could not delete the variable
nomad/jobs/box-dev1/spec: Permission denied)
```

### `box reap [--dry-run]`

One pass over the cluster that deletes the disks of the boxes whose term ran out:
a box that is not running has its disk deleted after the cluster's stopped-box
term, 24 hours by default. It is the backstop for an agent that forgot to delete
its box, not the normal path: the normal path is `box rm <id> --yes`.

The pass is run every 15 minutes by the periodic Nomad job `reap-boxes`, which is a
task of the box driver (deploy/reap-boxes.nomad.hcl, deploy/acl/box-reap.policy.hcl).
Its name is outside `box-<id>` on purpose: a job named `box-reap` would be a box of
the id `reap` and could be taken for one.
`box reap` runs the same pass by hand, from the coordinator; `box reap --dry-run`
lists what the next pass would delete and when, and changes nothing -- not even
the clock of a box no pass has seen yet.

At the first pass that sees a box not running it writes the time into the Nomad
variable `nomad/jobs/box-<id>/stopped`. Every way a box can stop is covered
because the pass looks at Nomad, not at how the box was stopped: `box stop`,
`nomad job stop`, a crash and the stop a bake makes all leave a box that is not
running. `box start` deletes that variable before it registers the job, and so
does `box rm`, so a box that comes back is not on a term any more, and a disk can
only ever be deleted late, never early. `broken` boxes and `orphan` disks with a
Nomad volume count as not running and get the same term.

One deletion prints one line, with the box, the size of its disk and how long it
had been stopped; the task log of the periodic job is the record. The pass does
not delete, and tries again on the next one, when it cannot read the state of the
box, when the box has a live allocation, when a bake or publish on its disk is
alive, or when its node is down or unreachable. A box that fails to delete does
not stop the pass over the others, and the command exits `1` so the job's log says
so.

### `box reap term [show]` / `box reap term set <duration>` / `box reap term unset`

The term is one value for the whole cluster: the Nomad variable
`nomad/jobs/box-reap`, item `stopped_term`, a Go duration such as `48h`. It is not
an environment variable, because `box ls`, `box status` and the periodic pass run
on different hosts and must agree. No variable, or no item, means the default of
24 hours; `box reap term` prints the value and says whether it is the default.

`box reap term set 48h` writes it, and `box reap term unset` deletes the variable,
which returns the cluster to 24 hours. Both refuse a value below one hour and one
that does not parse, with a sentence: a typo such as `1m` would otherwise delete
the disks of boxes stopped a moment ago. A change applies at once, because the
term is compared with the recorded time in `nomad/jobs/box-<id>/stopped`: a box
already stopped for longer than a lowered term loses its disk at the next pass
(`box reap` runs every 15 minutes), and a raised term keeps a disk that the old
one would have deleted.

The pass reads the term once, at the start, and uses it for every box. A term it
cannot use -- the variable cannot be read (permission denied, a Nomad error), or
it holds something unparsable or below one hour -- stops the pass before it looks
at a box: it deletes nothing, writes no clock, says why in its log and exits `1`.
Falling back to 24 hours there would delete a cluster's disks early. A missing
variable (404) is the default; an unreadable one is not.

### `box resize <id> [--cpus N] [--memory-mib N] [--disk-gib N] [--timeout D] [--wait]`

The disk grows in the driver, from the job's `disk_gib`. Nomad's dynamic host
volume keeps the capacity its plugin reported when the volume was created - a
re-registration of the volume never changes it - so the volume record goes on
saying the old size, and `box status` prints both figures when they disagree.

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

## Projects and idle boxes

Boxes are never stopped automatically: several projects share the nodes, and a box
is somebody's work. What the tools do instead is say, in the moment an agent
decides, which boxes are its own and what giving one back costs.

**A box belongs to a project.** The project comes from `--project NAME`, else from
`BOX_PROJECT`, else from the base name of the git repository the command runs in
(`git rev-parse --git-common-dir`, so a linked worktree carries the name of the
main repository), and else it has none. A label is lower-case letters, digits and
dashes; `--project` and `BOX_PROJECT` are refused in a sentence when they are not
that, because a label that does not say what was meant is worse than a refusal. A
name read from a directory is sanitized into a label instead (`My Project` is
`my-project`), because nobody typed it.

The label is stored in the job's meta as `project`, and so in the description the
box keeps: a resize, an upgrade and a start write it back, and the temporary box
of `box bake --rebuild` carries it too. `box ls` and `box capacity` show it.

**It is a label, not a protection.** Nothing is refused or stopped because of a
project, and `box ls --project` is a view and nothing more: the label is what lets
one agent tell its own boxes from another's. Deciding that another project's box
may be stopped is its owner's call.

**Hints, on stderr, in the moments that matter.** None of them changes an exit
code, and none of them stops anything:

- `box create` says, after the box is created, that the box is for this task, that
  the results go to git, and that `box rm <id> --yes` deletes the box and its disk;
- `box wait` says so once a turn has finished and no session of the box has a turn
  running;
- `box create` refused for lack of room names the idle boxes on the nodes it could
  have used, grouped by whether they are the caller's to stop:

  ```
  your idle boxes: a 2h, b 40m (box rm <id> --yes when their task is done)
  other idle boxes: c 3h (not yours to stop)
  ```

  An idle box is one whose guest says so (`boxctl info`, under the same 20 second
  bound every guest call has, asked in parallel so the whole list costs one bound
  and not one per box); a box with a turn running, a guest that does not answer and
  a boxd too old to report are left out, and a failure of any kind leaves the list
  out rather than delaying the refusal. With no project of the caller's own the
  list is simply `idle boxes: c 3h`. `--wait D` names them when it starts waiting
  and again with the refusal it ends with.

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
  The sources are then handed to the user that asked for them, so a plain
  `box run` and `box cp` work there. What the agent writes in them afterwards
  belongs to `boxd`'s user like the rest of `/work`: `box run --executor` is the
  way in, and `box cp` writes through `sudo -n` when it has to.
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

## When the connection is lost

A box is reached through Nomad: the API for its state and `alloc exec` for
everything in the guest. Either one can go away for a moment — the client node
restarts for a driver update, the server loses its path to the node, boxd
restarts in the guest — and none of that is work that ended. A lost connection
used to cost a turn: the command exited `1`, which an agent reads as "this turn
failed", and the work in the box went on without anyone waiting for it.

The four commands that follow work — `box wait`, `box events -f`, `box logs -f`
and `box run` — tell the two apart:

| what happened | what the command does |
| --- | --- |
| the box answered, with `busy`, `not_found`, the turn's result or an error | reports it, exit code as always |
| the box is stopped, failed, orphaned or gone | ends with the box's own sentence, exit `1` |
| contact was lost (EOF, connection refused, no path to node, a 5xx, a restarted client) | `box wait`, `box events -f` and `box logs -f` keep trying with a pause between attempts, resume where they were, and say so twice on stderr |
| contact is still lost when the budget runs out | exit `5`: the turn's state is unknown and it may still be running |
| contact was lost in `box run` | exit `5` at once, and the command is **not** sent again |

The two lines are one each, however many attempts they take:

```
lost contact with the box; retrying
back in contact after 1m12s
```

Only `box wait` has a budget of its own (`--timeout`); `box events -f` and `box
logs -f` follow until they are interrupted, and `Ctrl-C` still ends them.

## Inside a box

### `box ssh <id> [--executor NAME] [--cwd DIR]`

An interactive shell in the box, over `alloc exec`. It needs a terminal.

A plain `box ssh` is the box's login user. `--executor prime|opencode|claude`
gives the shell that executor's sessions' environment instead: as `boxd`'s user
(root), with the executor's `HOME`, its `XDG_CONFIG_HOME`, the bundle's `bin`
and the agent's own tool directories in front of `PATH`, in the box's workspace
(`--cwd DIR` for another directory). It is the shell an agent works in, so it
finds what the agent installed and made.

### `box run <id> [--timeout D] [--executor NAME] [--cwd DIR] -- cmd [args...]`

Runs one command in the box and returns its exit code and output. The command
runs in a session of its own in the guest: a `box run` that is interrupted
(Ctrl-C, SIGTERM, an outer `timeout`) or runs out of `--timeout` stops that
whole session before it returns, so nothing keeps running in the box with no
one to read how it ended. Nomad itself does not tell the box that the caller
went away while the command prints nothing. `--timeout` ends with exit 4.

**`--executor NAME` runs the command as that executor's sessions run.** `boxd`
runs as root, so the executors and what an agent makes -- `/work` and everything
in it, the tools it installs in its `HOME` -- belong to root. A plain `box run`
is the box's login user, a different user with a different `HOME` and `PATH`:
`git -C /work/proj fetch` there is "Permission denied", and `bun --version` is
"command not found" although the agent installed bun in its own home. With
`--executor`, the command goes in through `sudo -n` into `boxctl exec`, which
builds the environment a session of that executor gets
(`docs/boxctl-contract.md`, `boxctl exec`): its `HOME`, its `XDG_CONFIG_HOME`,
the bundle's `bin` and the agent's own `~/go/bin`, `~/.local/bin`, `~/.bun/bin`,
`~/.cargo/bin` and `~/.deno/bin` on `PATH`, and the box's workspace as the
working directory -- `/work/<repo>` when the box has one, as a session starts
there -- or `--cwd DIR`. Everything else is a plain `box run`: the command's
exit code is the command's, its output streams as it comes, `--timeout` stops
it, and a lost stream still ends with exit 5.

An unknown `--executor` is a usage error that names the executors the box has.
`--executor` needs passwordless sudo in the box, which every image this project
builds gives its login user; a box without it says so, and the command is not run.
A box whose `boxd` predates `boxctl exec` answers ``unknown command "exec"``:
`box upgrade <id>` moves it to the bundle the node carries.

**A command is never run twice.** When the connection to the box is lost while
the command runs, `box run` does not send it again — it cannot know whether it
ran to the end or not at all — and exits `5` with a sentence that says the
result is unknown and the command may still be running in the box. A box that
stopped ends the same way a command in a stopped box does: exit `1`, with the
box's own sentence.

### `box cp <src> <dst>`

Copies files or directories in or out, exactly one side inside a box:
`box cp ./dir dev1:/work/dir` and `box cp dev1:/work/out ./out`. It streams a
tar over `alloc exec`, refuses a `..` entry, and never writes outside the
destination.

The copy runs as the box's login user, and a target that user owns stays its
own. A target it cannot write -- `/work` and what `boxd`'s user made there, which
is where an agent works -- is made and written through `sudo -n`, and what is
written is given to the owner of the nearest directory that was already there.
That owner is the one the copy lands among: `box cp ./x dev1:/work/in/x` makes
`/work/in` and the file the owner of `/work`, not the uid of the machine the copy
started from. A copy out reads through `sudo -n` when the login user cannot read
the source, so a result an agent wrote in `/work` comes out without a detour
through `box run`. A box whose login user has no passwordless sudo says so, and
names what is left: a directory that user owns.

### `box top <id> [-f]`

What the guest is doing, as boxd measures it every five seconds: CPU and the
part of it waiting on the disk (iowait), the load average, memory used and
available, the memory pressure the guest kernel reports (`some` and `full`
avg10), the disk's use, and the three processes with the most resident memory.
`-f` prints it again every five seconds; `box status` shows the same line as
`usage`.

Under the summary line it prints, without needing a flag:

- the busiest processes, five of them, each with its pid, the share of one core
  it used since boxd's last sample, the memory it holds and its command cut
  short: a summary line alone says the box is busy and not what makes it busy;
- a process that has used a whole core for ten minutes or more, with the pid and
  how long: `systemd-logind (pid 851) has used a whole core for 4h12m`. boxd also
  writes that to the journal once, and says when it stops. It is what the box
  did, not a fault: a long compile is a whole core for ten minutes too;
- the work running with **no turn in progress** (see `box status`).

`--json` carries all three as `usage.processes`, `usage.hogs` and
`usage.leftover` (`docs/boxctl-contract.md`, `Usage`).

A guest that does not answer within 20 seconds fails the command with exit code
`4` and a sentence that says what to look at (`box logs`, `box status`); under
`--json` the document carries `guest_error`. `box session ls` behaves the same
way. `box wait`, `box send`, `box run` and `box exec` are not bounded: they are
as long as the work they start.

### `box logs <id> [-f]`

The task log of the box, which is the boxd event stream from the guest.

`-f` follows it. Both files (stdout and stderr of the task) are followed at
once, and each one remembers how many bytes it has printed: a connection that
is lost and comes back continues at that byte, so no line is printed twice and
none is skipped.

## Agent sessions

A box can host several agent sessions. Each session is one executor process in
the guest with its own model, transcript and working directory.

### `box session new <id> --executor prime|opencode|claude --model <provider>/<model> [--cwd DIR] [--title T]`

`box session new` prints a warning on stderr — never a refusal — when another
session of this box has a **running turn or is active outside a turn** in the same
working directory:

```
session s-7f3a2c is working in the same directory
```

Two sessions in one box are normal; two writers in one working copy are what a
lost edit, a surprise rebase and a half-written file look like afterwards. The
session is created anyway and the exit code stays `0`.

### `box session ls <id>`

### `box session close <id> <session>`

Stops the executor process and keeps the history.

### `box send <id> <session> [--file F] [prompt...]`

Starts one turn and returns at once. A second send while a turn runs answers
`busy`. The same warning as `box session new` is printed on stderr when another
session of the box is working in this session's directory; the send goes ahead
and the exit code does not change.

### `box wait <id> <session> [--timeout D] [--turn N] [--strict] [--json]`

Waits for the turn and prints its final text. On timeout it exits 4 and the turn
keeps running. `--turn N` waits for an older turn instead of the running one.
`--strict` exits 6 when the turn ended but left work running (see below).

A lost connection to the box — the Nomad client restarted for a driver update,
the server lost the path to the node, a 5xx, boxd restarted in the guest — is
not the end of the turn. The wait keeps trying, with a pause between the
attempts, until `--timeout` runs out, and it says so twice on stderr:

```
lost contact with the box; retrying
back in contact after 1m12s
```

It reconnects to the **same turn**: the turn is asked for by its number after
the first loss, so a turn that finished while the box was unreachable is
reported with its real result and its real text, once. Only a box that is really
gone — stopped, failed, removed — ends the wait, with the box's own sentence.

When `--timeout` runs out with contact still lost, the exit code is `5` and the
sentence says the turn's state is unknown and it may still be running. That is
not a failure and not a timeout: `box events <id> <session> --since N` reads what
the turn did as soon as the box answers again.

When the turn ends with processes still running in the box, the warning comes
after the text, on stderr, and the exit code stays the turn's:

```
the turn ended, but these processes it may have started are still running in the box:
  go build ./... (pid 123, 4m10s, 4 children, 12.5s CPU)
  and 2 more
`box top dev1` shows what the guest is doing and `box run dev1 -- ps -ef` lists the processes;
when they are done, `box send dev1 s-7f3a2c` starts the next turn, or ask the session to wait for them.
```

**A done turn with processes still running is not finished work.** The executor
ended its turn (ACP `end_turn`) and the command a tool started did not: taking
the answer as the result of a build, a test run or a server start that is still
going is how a coordinator reports work that was never done. Look before you go
on, and say what you saw. `--json` carries the same list as `still_running`
(`docs/boxctl-contract.md`, `Turn`); nothing is printed on stderr then, and the
field is missing when the turn left nothing running.

**`--strict` makes that turn an exit code.** A script cannot read a warning, so
with `--strict` a turn that ended normally and left at least one process running
exits `6` instead of `0`. Everything printed is the same -- the answer, the
warning and the `--json` document -- and stderr adds one sentence:

```
box: the turn 1 of session s-7f3a2c left work running in the box, so this wait exits 6: `box send dev1 s-7f3a2c` with "wait for it, then finish" is the usual next step
```

The usual next step is the one the sentence names: send the session a follow-up
that waits for the work and then finishes the task. Every other ending keeps its
own exit code under `--strict` (a failed or cancelled turn `1`, a timeout `4`, a
lost contact `5`); `6` is only for a turn that would otherwise have exited `0`.

**When the turn is over and the box is doing nothing, the wait says so.** If the
guest reports that no session of the box has a turn running, the wait ends with
one line on stderr:

```
box rm dev1 --yes when the task is done: the box and its disk go with it. `box stop dev1` is only a pause, and a box that is not running loses its disk after its term (24 hours unless the operator set another)
```

It is advice for the moment the box is done, not a change of the result: the exit
code is the turn's, and `--json` prints nothing on stderr. A turn still running in
another session, a guest that does not answer and a boxd too old to report its
activity say nothing.

### `box events <id> <session> [--since N] [--limit N] [--tail N] [-f]`

The session's event feed. `--since` is a resumable cursor; `-f` follows it, and
picks the feed up again at the last sequence it printed when the connection to
the box is lost (the same two lines on stderr as `box wait`).

`--tail N` prints the last N events instead of the feed: the news of a long
session, for a person or an agent coming back to it. It is two calls — one for
how many events there are, one for the last N — and `--json` prints the same last
N documents. `--tail` and `-f` are different questions and cannot be asked
together.

One call returns at most `--limit` events (200). When there are more, the command
says so on stderr and gives the cursor that continues them:

```
2 of 9 events, more with --since 3
```

That line is not an error: the feed is longer than one call, and `--since` with
that number reads the rest. `--json` carries the same fact as one more document
on stdout, after the events: `{"more":true,"next_since":3}`. `--since` reads that
event again, so reading by pages gives every event once and loses none at a page
boundary.

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
box's templates within seconds; the driver compares the **content** of the task
files with the files the guest mounts, puts the new ones where the guest mounts
them and runs `boxctl reload`. boxd re-reads the credentials and stops the
executor of every idle session, so the next turn runs with the new
configuration; a session in the middle of a turn keeps the executor it started
with, and it restarts when the turn ends — the journal says so in one line. The
VM, the allocation and the transcripts are untouched. A box that was stopped
picks the new files up at its next start.

**A rotation that changes no byte restarts nothing.** A template re-rendered with
the same content is not a new credential: the driver stages nothing, calls no
reload, and boxd compares the credential files' content, not their modification
time. That is what makes a driver update — which re-attaches to every running box
— cost no turn: a recovered task stages nothing and reloads nothing.

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
