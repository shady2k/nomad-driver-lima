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
box create dev1 --image <newest> --arch amd64 --disk-gib 16   # 2 vCPU, 2048 MiB by default
box session new dev1 --executor prime --model <provider>/<model> --title "fix the tests"
box send dev1 s-7f3a2c --file prompt.md   # or the prompt as one argument
box wait dev1 s-7f3a2c --timeout 15m      # blocks until the turn ends, prints the answer
box events dev1 s-7f3a2c --since 12       # what happened, resumable
box run dev1 -- python3 /root/repo/check.py   # one command in the guest, to check a result
box cp ./results dev1:/root/in            # files in
box cp dev1:/root/repo/out ./out          # files out
box status dev1                           # state, task events, guest, sessions, disk used
box stop dev1                             # keep the disk, free the RAM
box start dev1                            # same disk, same files
box rm dev1 --yes                         # the disk is deleted
```

`--json` on any command gives one JSON document to parse.

Exit codes: `0` ok, `1` error, `2` usage, `3` not found, `4` timeout (still
running). A non-zero exit is always a sentence you can act on.

## Choosing where and what

- **The architecture is yours to say.** Without `--arch` the box goes to any
  node that has room, Linux or Mac. A task that needs a platform says so:
  `--arch amd64` or `--arch arm64`. `box capacity` lists both, never add their
  free resources together.
- **Take the newest image** from `box images`; an older one lacks fixes.
- **The defaults (2 vCPU, 2048 MiB, 8 GiB) fit an agent session.** Ask for more
  only for a real build or test run, and leave room for the other agents on
  the node.
- **The model is one the box's key may use**, as `<provider>/<model>`. The box
  asks its provider when a session starts, and `box session new` refuses any
  other model with the list there is; `box executors show` shows the staged list.
- **`box <command> --help`** prints a command's arguments and every flag.

## Rules

- **One box per task, several sessions in it.** Start a second
  `box session new` for a second piece of work instead of a second VM: the
  disk, the toolchain and the git checkout are shared, the executors are not.
  Sessions are cheap; VMs are not.
- **`box wait --timeout` instead of sleep loops.** A turn may take twenty
  minutes; `box wait` returns the moment it ends, and `box events --since`
  resumes a stream you already read. Do not write `while ...; sleep 60; done`
  scripts around a box, and do not poll `box status` in a loop.
- **`box wait` exits 1 when the turn ended in an error or was cancelled**, with
  the turn's error; 0 means the turn is done.
- **One turn at a time per session.** A `busy` answer means a turn is running:
  `box wait` for it, or `box cancel` to stop it; do not send a second prompt and
  do not restart the box.
- **Never recreate a box to "fix" it.** `box stop` then `box start` keeps the
  disk, the installed packages and the work in progress.
- **A resize restarts the box and kills a running turn.** `box resize
  --cpus/--memory-mib` stops the VM, edits it and starts it again; wait for
  every session's turn to end first. The disk is kept and can only grow.
- **A repository goes in by clone or by bundle.** What is pushed, the box clones
  itself (`box run <id> -- git clone ...`), which is the fastest. Commits you
  have not pushed go in as a bundle: `git bundle create repo.bundle --all`,
  `box cp repo.bundle <id>:/root/`, then clone from it in the box.
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
- **An orphan** in `box capacity` (a VM with no box task, or without its disk)
  is the operator's: the driver only reports it. Say so; do not try to remove it.

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
  why (no memory, no `box-volume` plugin, wrong architecture). Fix the request
  or wait; do not retry in a loop.
- `Nomad could not place the box: 3 node(s) ran out of memory` - the scheduler
  refused it. Ask for less, or stop a box you own with `box stop`.
- `Nomad rejected the box job ... the Nomad token needs ...` - the token lacks
  a rule, and the message names it. Tell the operator; do not retry.
- `the box is stopped, not running` - `box start <id>` brings it back on its
  disk.
- `the guest did not answer boxctl info` - the box is up but `boxd` is not (yet);
  `box logs <id> -f` shows the guest log, `box status <id>` shows the task
  events.
- `session <id> is still working on a turn` - exactly that; `box wait`.

## For the coordinator agent

1. `box capacity --json` once per task: read every node, per architecture.
2. Create one box per task (not per step), named after the task, with the
   `--arch` the task needs and the newest image.
3. Start one session per unit of work; send the prompt; `box wait` with a
   generous `--timeout`.
4. Read the answer from `box wait` or `box events`, check it with `box run`,
   and copy results out with `box cp` or push a branch.
5. `box stop` when the work pauses; `box rm --yes` when the task is done.
