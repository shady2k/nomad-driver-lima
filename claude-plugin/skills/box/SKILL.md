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
box images                        # 2. which base images exist (newest 1.0.x is fine)
box executors show                # 3. which executors have credentials in this cluster
                                  #    (prime, opencode, ...; any provider)
box create dev1 --image 1.0.0 --cpus 2 --memory-mib 2048 --disk-gib 16
box session new dev1 --executor <executor> --model <provider>/<model> --title "fix the tests"
box send dev1 s-7f3a2c "make the tests pass; the repository is in /root/repo"
box wait dev1 s-7f3a2c --timeout 15m      # blocks until the turn ends
box events dev1 s-7f3a2c --since 12       # what happened, resumable
box cp ./results dev1:/root/in            # files in
box cp dev1:/root/repo/out ./out          # files out
box status dev1                           # state, task events, guest, sessions, disk used
box stop dev1                             # keep the disk, free the RAM
box start dev1                            # same disk, same files
box rm dev1 --yes                         # the disk is deleted, ask first
```

`--json` on any command gives one JSON document to parse.

Exit codes: `0` ok, `1` error, `2` usage, `3` not found, `4` timeout (still
running). A non-zero exit is always a sentence you can act on.

## Rules

- **One box can host several sessions.** Start a second `box session new` for a
  second task instead of a second VM: the disk, the toolchain and the git
  checkout are shared, the executors are not. Sessions are cheap; VMs are not.
- **`box wait --timeout` instead of sleep loops.** A turn may take twenty
  minutes; `box wait` returns the moment it ends, and `box events --since`
  resumes a stream you already read. Do not write `while ...; sleep 60; done`
  scripts around a box, and do not poll `box status` in a loop.
- **Never recreate a box to "fix" it.** `box stop` then `box start` keeps the
  disk, the installed packages and the work in progress. `box rm` deletes the
  disk: use it only when you mean it, and move the results out first.
- **Results travel by git or `box cp`.** A box is disposable; its disk is not
  backed up. Push a branch, or copy the files out with `box cp`, before you
  stop or delete the box. `box cp` streams a tar over `alloc exec`, so it works
  for whole directories.
- **A box disk grows, it does not shrink.** `box ls` shows what each disk really
  occupies on the host ("280 MiB of 8.0 GiB"): the rest is free inside the box.
  A box whose work is done is still holding that disk until `box rm`.
- **A resize restarts the box.** `box resize --cpus/--memory-mib` stops the VM,
  edits it and starts it again; the disk is kept and a disk can only grow.
- **A `busy` answer means a turn is running.** `box wait` for it, or `box cancel`
  to stop it; do not send a second prompt and do not restart the box.

## Executors

`box executors show` lists the executor credentials the cluster has (file names
only, never a value): a box copies them into `/run/box/secrets/<executor>/` when
it starts, a tmpfs in the guest that holds no secret on disk. `box status <id>`
says which executors a box was given, and its guest section says `unconfigured`
for an executor without credentials.

- A box created before `box executors set` starts anyway; it gets the
  credentials at its next `box start`.
- The credentials themselves are the operator's business (the variable
  `nomad/jobs/box-executors`); ask the owner instead of copying keys around.
- `boxctl info` inside the box reports `available` (the binary is installed) and
  `configured` (the credentials are there) for every executor.

## When something is wrong

- `no node can take this box: ...` - the message says which node failed and
  why (no memory, no `box-volume` plugin, wrong architecture). Fix the request
  or wait; do not retry in a loop.
- `Nomad could not place the box: 3 node(s) ran out of memory` - the scheduler
  refused it. Ask for less, or stop a box you own with `box stop`.
- `the box is stopped, not running` - `box start <id>` brings it back on its
  disk.
- `the guest did not answer boxctl info` - the box is up but `boxd` is not (yet);
  `box logs <id> -f` shows the guest log, `box status <id>` shows the task
  events.
- `session <id> is still working on a turn` - exactly that; `box wait`.

## For the coordinator agent

1. `box capacity --json` once per task; remember which node and how much room.
2. Create one box per task (not per step), named after the task.
3. Start one session per unit of work; send the prompt; `box wait` with a
   generous `--timeout`.
4. Read the answer from `box wait` (it prints the final text) or from
   `box events`; copy results out with `box cp` or push a branch.
5. `box stop` when the work pauses; `box rm --yes` when the task is done.
