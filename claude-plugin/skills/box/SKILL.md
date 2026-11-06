---
name: box
description: Run work in a box - a Linux VM on the Nomad cluster - with the `box` command. Use when a task needs an isolated machine for a long-running agent session, a build, or a test run that must survive SSH drops and client restarts.
---

# box

`box` runs Linux VMs ("boxes") on the Nomad cluster through the Nomad API. It
keeps no local state: the box is a Nomad job (`box-<id>`) with a dynamic host
volume (`box-<id>`) that holds its disk, so anything the command reports comes
straight from Nomad.

This skill drives the `box` CLI and does not bundle it: the CLI has to be on
`PATH` first (`box --help` answers). The operator installs it and sets the
`NOMAD_*` environment the CLI reads; see the repository's deploy kit.

Every guest call is `nomad alloc exec`, so you never need the guest's network,
an SSH key or a port. **You never shell into a box to "take a look" with a
polling loop**: `box wait` and `box events` report progress.

## The workflow

```bash
box capacity                      # 1. where can a box go, and how much is free
box images                        # 2. which base images exist: take the newest
box executors show                # 3. which executors have credentials, and their models
box create dev1 --image <newest> --arch amd64 --disk-gib 16 \
    --repo https://github.com/org/proj        # 2 vCPU, 4096 MiB by default; returns when the box is ready
box session new dev1 --executor prime --model <provider>/<model> --title "fix the tests"   # starts in /work/proj
box send dev1 s-7f3a2c --file prompt.md   # or the prompt as one argument
box wait dev1 s-7f3a2c --timeout 15m      # blocks until the turn ends, prints the answer
box events dev1 s-7f3a2c --since 12       # what happened, resumable
box run dev1 -- python3 /root/repo/check.py   # one command in the guest, to check a result
box run dev1 -- docker info            # on an image older than the docker-group unit: sudo -n docker info
box cp ./results dev1:/root/in            # files in
box cp dev1:/root/repo/out ./out          # files out
box status dev1                           # state, task events, guest, sessions, disk used
box top dev1 -f                           # CPU, iowait, load, memory, disk, the biggest processes
box stop dev1                             # keep the disk, free the RAM
box start dev1                            # same disk, same files, also tomorrow
box rm dev1 --yes                         # the disk is deleted
```

A box is its disk, not its Nomad job: `box stop` keeps a description of the box
in a Nomad variable, so `box start` brings it back on the same disk after Nomad
has collected the dead job (its GC runs every few hours). A disk nothing
describes - a box made before boxes kept one - shows as `orphan` and cannot be
started again.

`--json` on any command gives one JSON document to parse.

Exit codes: `0` ok, `1` error, `2` usage, `3` not found, `4` timeout (still
running), `5` unknown (contact with the box was lost: the turn or the command may
still run). A non-zero exit is always a sentence you can act on.

**A lost connection is not a lost turn.** `box wait`, `box events -f` and
`box logs -f` keep trying while the connection to the box is away — the node's
client restarted for a driver update, a 5xx, a restarted `boxd` — and say so
twice on stderr (`lost contact with the box; retrying`, then `back in contact
after 1m12s`). `box wait` does not exit 1 for that: it ends with the turn's own
result, and only exits `5` when contact is still lost at `--timeout`. `box run`
cannot be sent twice, so a lost stream exits `5` and says the result is unknown;
check the box (`box run <id> -- ps -ef`, `box logs <id> -f`) instead of running
the command again.

## Keeping an environment: project images

When the same install work is about to happen in a second box, make the first
box's environment an image instead of repeating it.

1. `box images` -- take the closest image there is: a project image of this
   project whose `BEHIND` column says its parent is the newest base image. A
   published one is created from anywhere; a draft (`draft on node X`) only on
   that node.
2. Install what the project needs in the box, and only that: toolchains, package
   sets, the caches a build fills. Keep code in `/work` (`box prepare` puts it
   there): that is the one place the seal removes code from.
3. Write the recipe: the script that makes this environment from the parent
   image, and that can run on a fresh box of it. It is what a later `box bake
   --rebuild` replays on a newer base image, in a temporary box of 2 vCPUs and
   4 GiB with no sources and no credentials for your repositories. `box bake
   --recipe` only stores the recipe and does not run it: run it on a fresh box
   and bake that box, or a broken recipe shows only at the rebuild.
4. `box bake <id> --name <project> --recipe recipe.sh`, between turns. The seal
   runs in a clone of the box's disk, so the box keeps its work: nothing has to be
   pushed out first, and a bake that fails leaves the box as it was. The box is not
   usable while the bake runs, and it starts again with `/work` and its sessions
   when it ends. If another box took its room meanwhile, `box start` it later.
5. Create from the image with `box create <id> --image <project>-N`, and check the
   environment there. A draft is on one node only, so the box goes to that node.
   The box you baked keeps its disk and its sessions: `box rm <id> --yes` when its
   task is done.
6. Propose publishing, and let the owner decide: `box images publish <version>
   --yes` puts the image in the operator's store, which is what makes it
   available on every node. A published version never changes.

**What a seal removes:** `/work` and the `box prepare` destination, the sessions,
`/tmp`, `/var/tmp`, `/var/log`, the SSH host keys, the cloud-init state and the
machine id. In `/root` and `/home/*`: the executors' state, shell histories, `.ssh`
keys, `.netrc`, `.git-credentials`, tokens in `.npmrc` and credential helpers in
`.gitconfig`.

**Everything else stays, and nothing checks it.** That is the point for
`/var/lib/docker` (images, layers, volumes), `/nix`, language caches and installed
packages, so dockerd need not be stopped. It is also the danger: an image goes to
anyone who can pull it. Before a bake:

- `docker logout`: a seal leaves `~/.docker/config.json` in place.
- `docker rm` every container: one with a restart policy starts in the clone.
- No credential in an image's `ENV` or layers, and no code copied into a Docker
  image; no checkout outside `/work` (`~/repo` stays). The seal's proof looks only
  for executor state and for `auth.json` or `credentials.json` in the homes and
  box directories, and such a file fails the bake.
- `docker system prune` and delete what the image does not need: it is what every
  box from this image starts with.

**Size and time.** No size is refused, and a large image is fine when its caches
save more than it costs. `box bake` and `box images publish` wait 30 minutes by
default; the compression uses one core, so a disk of several GiB wants `--timeout
2h`. After a timeout the job goes on: `box images` lists the draft when it is
there. The node checks room only for the copy of the disk, yet it needs that plus
the image, and the first box from the image adds an uncompressed copy of it: plan
on three times what `box ls` says the disk occupies. The box's `--disk-gib` becomes
the smallest disk any box from the image can have. Only an image with the
`platform` field can be baked: `box create` the box from a current base image, not
an old one.

## Choosing where and what

- **The architecture is yours to say.** Without `--arch` the box goes to any
  node that has room, Linux or Mac. A task that needs a platform says so:
  `--arch amd64` or `--arch arm64`. `box capacity` lists both, never add their
  free resources together.
- **Take the newest image** from `box images`; an older one lacks fixes. Then
  look for a project image of your project: a `project` row whose parent is the
  newest base image already holds the toolchain and the caches, and a box from it
  starts ahead of one that installs them again.
- **`box events <id> <session> --tail 20`** is the news of a long session: the
  last N events instead of the feed from a cursor. Use it when you come back to a
  session you did not follow, then `--since` with the last sequence to go on.
- **A box's `boxd` and executors come from a platform bundle, not from its
  image.** For an image built for bundles the driver mounts the newest bundle
  for the box's architecture read-only at `/opt/box` when the box starts;
  `box create --platform <version>` picks another one and `box upgrade <id>
  [--platform <version>]` moves a running box to a newer one (it re-registers
  the job, so the box restarts and its disk is kept). A `boxd` or executor
  update therefore needs no new image, and a project image derived from a base
  image never has to be rebuilt for it.
- **The node is shared, and free resources are there to be used.** Choose the
  size for the task when you create the box. The default (2 vCPU, 4096 MiB,
  8 GiB) is for light work without a build; a build or a test run needs more
  memory from the start, so say `--memory-mib 8192` (or more) when the task
  compiles, installs or runs tests. When in doubt take more: a box can be
  stopped, but a guest out of memory cannot be noticed from inside -- it
  freezes, `box top` and `box status` stop answering, and the host loses several
  cores -- so do not wait for `box top` to show a shortage. `box resize` is for
  growth you can see coming, between turns, and for a box whose guest has gone
  silent (it restarts it; the disk is kept). Several boxes are fine when the
  work is really parallel and there is room; work on one task usually fits in
  sessions of one box. Give back what you do not use: `box stop` an idle box and
  `box rm` a finished one, because a reservation held by an idle box keeps
  someone else's work from starting. If there is no room, stop your own idle
  boxes or tell the owner, or let `box create --wait 30m` wait for room and say
  on stderr what it waits for; do not sit in a retry loop. A box that takes more
  than a quarter of its node is created anyway and said on stderr: it fits, it is
  yours to watch.
- **The model is one the box's key may use**, as `<provider>/<model>`. The box
  asks its provider when a session starts, and `box session new` refuses any
  other model with the list there is; `box executors show` shows the staged list.
- **`box <command> --help`** prints a command's arguments and every flag.

## Rules

- **One box per task, several sessions in it.** Start a second
  `box session new` for a second piece of work instead of a second VM: the
  disk, the toolchain and the git checkout are shared, the executors are not.
  Sessions are cheap; VMs are not.
- **Give the node back.** `box stop` a box whose work is paused and `box rm
  <id> --yes` one that is finished: an idle box holds its node's memory, and the
  node is shared with every other agent's boxes.
- **A box you do not need right now: stop it.** `box stop <id>`, even for an
  hour: its disk, its environment and its sessions are kept, and `box start`
  brings it back. An idle box holds its memory and its cores, and another agent
  may be refused because of it. A finished box: `box rm <id> --yes`. Boxes of
  other projects are not yours to stop: ask their owner.
- **A box's project is `box create --project NAME`, else `BOX_PROJECT`, else the
  name of this directory's git repository.** It is a label, not a protection:
  `box ls --mine` shows your boxes, `box ls --project NAME` another project's, and
  a refusal for lack of room names the idle boxes that are yours to stop.
- **`box wait --timeout` instead of sleep loops.** A turn may take twenty
  minutes; `box wait` returns the moment it ends, and `box events --since`
  resumes a stream you already read. Do not write `while ...; sleep 60; done`
  scripts around a box, and do not poll `box status` in a loop.
- **`box wait` exits 1 when the turn ended in an error or was cancelled**, with
  the turn's error; 0 means the turn is done; `5` means contact with the box was
  lost and the turn's state is unknown, so it may still be running — read
  `box events <id> <session> --since N` when the box answers again instead of
  sending the prompt a second time.
- **A session that is `active outside a turn` is still working.** A turn that
  ended in an error while the executor kept going: `box session ls` and `box
  status` say `active outside a turn`, and what it does is in `box events` with
  turn 0. Do not send it a second prompt in that state, and do not open a second
  session in the same directory: `box session new` and `box send` warn
  (`session s-x is working in the same directory`). Read the work from
  `box events`, not from the turn that ended.
- **A done turn with processes still running is not finished work.** `box wait`
  says so on stderr when the turn left a command running in the box (a build, a
  test run, a server); the exit code stays 0, because when its turn ends is the
  executor's decision. Look before you take the result or start the next step:
  `box top <id>`, `box run <id> -- ps -ef`, and wait for them.
- **One turn at a time per session.** A `busy` answer means a turn is running:
  `box wait` for it, or `box cancel` to stop it; do not send a second prompt and
  do not restart the box.
- **Never recreate a box to "fix" it.** `box stop` then `box start` keeps the
  disk, the installed packages and the work in progress.
- **A resize restarts the box and kills a running turn.** `box resize
  --cpus/--memory-mib` stops the VM, edits it and starts it again; wait for
  every session's turn to end first. The disk is kept and can only grow.
- **The box prepares itself; you do not clone in a shell.** `box create --repo
  URL [--ref R]` (or `box prepare <id> --repo ...` later) clones in the box,
  runs the project's `.box/prepare.sh` and returns when the box is ready, or
  fails with the reason and the last lines. Files that are not pushed, or not
  in git at all, go in with `--from PATH` instead. Sessions refuse to start
  while a preparation runs or after it failed; the refusal names the way out
  (`box prepare <id>` waits or runs it again, `--cancel` stops it, `--skip`
  goes on without it). A big repository takes minutes: the command prints its
  progress, wait for it.
- **Check every step's exit code.** Chain setup commands with `&&`, never `;`,
  and do not start a session after a step that failed. Give `box run` its own
  `--timeout D` instead of wrapping it in `timeout`: both stop the command in
  the box, but `--timeout` says so with exit 4.
- **Results travel by git or `box cp`.** A box is disposable; its disk is not
  backed up. Push a branch, or copy the files out with `box cp`, before you
  stop or delete the box. `box cp` streams a tar over `alloc exec`, so it works
  for whole directories.
- **Check a result yourself.** The agent's answer is a claim: `box run` the
  test, or `box cp` the file out, before you report the task done.

## Cleaning up

A box holds its node's memory while it runs and its disk until it is deleted;
nothing deletes it for you.

- **Your own box, task done:** move the results out (push, `box cp`), then
  `box rm <id> --yes`. No need to ask: you created it for this task.
- **Your own box, work paused:** `box stop <id>` frees the memory and keeps the
  disk.
- **A box you did not create:** leave it alone, even when it looks abandoned;
  tell the owner its id and what `box status` says.
- **An orphan** in `box capacity` (a VM with no box task and no description, or
  without its disk) is the operator's: the driver only reports it. Say so; do not
  try to remove it. A box you stopped is `stopped` there, not an orphan.

## Executors

`box executors show` lists the executor credentials the cluster has (file names
only, never a value). A box copies them into a tmpfs in the guest when it
starts, so no key lands on a disk.

- **prime is the executor that has been run end to end.** opencode works but
  does not resume a session after an executor restart; `claude` has never been
  run. An executor with `no files` is not configured in this cluster.
- **A `(template)` file is read from Vault** when the box starts. The box needs
  a Vault role for it: `--vault-role`, or `BOX_VAULT_ROLE` in the CLI's
  environment. `box create` prints a note when the role is missing, and the box
  then fails to start; ask the operator for the role instead of guessing one.
- The credentials themselves are the operator's business; never copy keys into
  a box or into a prompt.
- `boxctl info` inside the box (`box run <id> -- boxctl info`) reports
  `available` (the binary is installed) and `configured` (the credentials are
  there) for every executor.
- **A rotated key reaches a running box by itself**, within seconds: an idle
  session's executor restarts at once, a session in a turn keeps its executor
  until the turn ends, and a rotation that changes no byte restarts nothing (a
  driver update therefore costs no turn). Nothing to do in the box, and no reason
  to recreate it.

## What a box does not protect

- **The agent in a box runs as root and its tool calls are approved
  automatically.** There is no human to ask, so it can run anything in the box.
- **Every box gets the same executor key**, and anything in the box can read it.
  Do not put untrusted code, or a repository you would not run with that key,
  into a box.
- **A box has the network.** There is no egress filter between it and the
  internet or the cluster.

## When something is wrong

- `no node can take this box: ...` - the message says which node failed and
  why (no memory, no `box-volume` plugin, wrong architecture). Fix the request,
  or let `box create --wait 30m` wait for room instead of retrying in a loop.
- `Nomad could not place the box: 3 node(s) ran out of memory` - the scheduler
  refused it. Ask for less, or stop a box you own with `box stop`.
- `Nomad rejected the box job ... the Nomad token needs ...` - the token lacks
  a rule, and the message names it. Tell the operator; do not retry.
- `the box <id> is stopped` - `box start <id>` brings it back on its disk, also
  when Nomad no longer has its job.
- `box <id> is busy: ... nothing was stopped and nothing was baked` - a turn runs
  or a preparation is in progress, and `box bake` refuses it before it stops
  anything. `box wait` for the turn, then bake again.
- `node <name> has ... free ... and the copy of box <id>'s disk needs ...` - the
  node has no room for the bake's copy of the disk. Bake a box with a smaller
  disk, or tell the owner which box to remove.
- `the box <id> is an orphan` - its disk is there and nothing describes it, so it
  cannot be started; `box rm <id> --yes` deletes the disk. Tell the owner.
- `the guest did not answer boxctl info` - the box is up but `boxd` is not (yet);
  `box logs <id> -f` shows the guest log, `box status <id>` shows the task
  events.
- `session <id> is still working on a turn` - exactly that; `box wait`.
- **A box is slow, hangs or restarted by itself.** `box top <id>` says whether it
  is busy, waiting on its disk (iowait) or out of memory, and which processes
  hold it. `box logs <id>` has the guest kernel's lines (out of memory, I/O
  errors), low memory and disk, and what each executor printed; `box status
  <id>` has the driver's task events, which say when the kernel OOM-killed
  something in the box's cgroup. A box out of room gets more with `box resize`.
- **A tool in a session fails strangely or waits** (a lock, a timeout, "another
  process is setting up..."): look before you retry. `box top <id>` shows who
  is running (an installer from an earlier session may still hold the lock),
  and `box logs <id>` shows what that process printed - an executor's own logs
  appear as `prime log ...` lines, a failed download names its URL. Retrying
  blind repeats the same wait; a closed session's background setup may still
  be running.
- **Kill a process in a box by pid, not by pattern.** `pkill -f pattern` inside a
  `box run` matches the command line that is running it (the pattern is in it) and
  kills its own `box run`; `box top <id>` or `box run <id> -- ps -ef` gives the
  pid, and `kill <pid>` is exact. When only a pattern will do, bracket one
  character so the pattern does not match itself: `pkill -f '[g]o build'`.
- **`box run` returns when its command exits, and takes the command's output
  with it.** A background process left writing to that output dies of SIGPIPE, so
  start a daemon detached, with its output in a file:
  `setsid nohup server </dev/null >/tmp/server.log 2>&1 &`.
- **Inside the box everything is yours to fix.** Install what is missing, fix
  apt, rebuild a cache: `box run <id> -- sudo ...`. Report to the operator only
  what you cannot reach from inside: the VM, its credentials, the node.

## For the coordinator agent

1. `box capacity --json` once per task: read every node, per architecture, and
   see what the running boxes hold.
2. Create one box per task (not per step), named after the task, with the
   `--arch` the task needs, the newest image, and the size the work needs -- a
   build or a test run gets more memory than the default from the start. Say on
   a note when a box takes more than a quarter of its node, and that it will be
   given back.
3. Start one session per unit of work; send the prompt; `box wait` with a
   generous `--timeout`.
4. Read the answer from `box wait` or `box events`, check it with `box run`,
   and copy results out with `box cp` or push a branch.
5. `box stop` when the work pauses; `box rm --yes` when the task is done. A
   finished box left running keeps someone else's work from starting.
