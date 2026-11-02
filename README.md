# box — one VM per Nomad task

[![CI](https://github.com/shady2k/nomad-driver-lima/actions/workflows/ci.yml/badge.svg)](https://github.com/shady2k/nomad-driver-lima/actions/workflows/ci.yml)
[![Integration](https://github.com/shady2k/nomad-driver-lima/actions/workflows/integration.yml/badge.svg)](https://github.com/shady2k/nomad-driver-lima/actions/workflows/integration.yml)

`box` gives a Nomad task a whole Linux machine: a Lima VM ("a box") with its own
kernel, disk, package manager and long-running processes. The VM's disk is a
Nomad **dynamic host volume**, so the scheduler places the task on the node that
holds the disk, and a replacement allocation adopts the same VM instead of
starting a new one.

That shape is what makes a box usable for work that outlives a connection: an
agent session, a build, a test run. Stop a box and its files are still there.
Restart the Nomad client and the box keeps running. Delete the box and its disk
goes with it.

- **driver** `box` — the Nomad task driver (`cmd/box-driver`)
- **volume plugin** `box-volume` — creates and deletes a box's disk (`cmd/box-volume`)
- **CLI** `box` — create, start, stop, rm, resize, run, ssh, cp, logs, status,
  capacity, sessions, executor credentials (`cmd/box`)
- **guest daemon** `boxd` / `boxctl` — runs agent sessions inside the box and
  answers the CLI (`cmd/boxd`)
- **skill** — a `box` skill an agent reads, packaged as a Claude Code plugin
  (`claude-plugin/skills/box`)

## Requirements

- **Nomad servers you already run**, 2.0 or newer, with TLS and ACLs as you set
  them. This repository does not install servers. A box needs the dynamic host
  volume support of 2.0.
- **Client nodes**: Linux amd64 with KVM, or macOS 13+ on Apple silicon with
  Apple Virtualization (`vz`). A node needs `limactl` and, on Linux, `qemu`.
- **An unprivileged user on each node** for the VMs. The Nomad client runs as
  root and Lima refuses to run as root, so the driver starts every VM process as
  this user.
- **A base image** you build (or fetch) and publish, and an HTTP store to publish
  it to. This project publishes no images.
- `ansible-core` and `go` on the machine you deploy from.

## Quick start, from nothing

Every name below is a placeholder: `example.com`, `node-01`, `<provider>`. Use
your own.

```sh
# 1. build the release the deploy kit installs (the version must be semver)
./deploy/build.sh --version 0.1.0          # -> dist/0.1.0/

# 2. build a base image, publish it to your own store, prove it boots
images/build.sh amd64 1.0.0                # -> out/box-base-1.0.0-amd64.qcow2 + .sha256
images/publish.sh amd64 1.0.0              # the store URL and token come from the environment
images/verify.sh amd64 1.0.0               # downloads, boots and checks what you published
```

Then install the platform with the Ansible kit:

```sh
cd deploy/ansible
cp -r inventory.example inventory
$EDITOR inventory/hosts.ini inventory/group_vars/*.yml
ansible-vault create inventory/group_vars/all/vault.yml   # the tokens

ansible-playbook -i inventory site.yml --check            # look first
ansible-playbook -i inventory site.yml                    # a node and a coordinator
ansible-playbook -i inventory acl.yml                     # a cluster with ACLs
```

The coordinator gets the `box` CLI. Point it at your image index, give the boxes
their executor credentials, and create the first box:

```sh
set -a; . ~/.config/box/env; set +a        # the role wrote NOMAD_ADDR and the token
export BOX_IMAGE_INDEX=https://images.example.com/box-base/index.json

box capacity                                # which nodes can take a box, and how much room
box images                                  # which image versions exist
box create dev1 --image 1.0.0 --cpus 2 --memory-mib 2048 --disk-gib 16
box session new dev1 --executor prime --model <provider>/<model> --title "first run"
box send dev1 s-7f3a2c "make the tests pass; the repository is in /root/repo"
box wait dev1 s-7f3a2c --timeout 15m        # returns the moment the turn ends
box stop dev1                               # keep the disk, free the RAM
box rm dev1 --yes                           # the disk is deleted
```

The executor credentials are the operator's job, once per cluster:

```sh
deploy/executors-from-prime.sh --out /tmp/box-executors \
    --provider <provider> --base-url https://api.example.com/v1
box executors set --from-dir /tmp/box-executors --provider <provider>
box executors show                          # file names and sizes, never a value
```

`box help` prints the command list; `docs/cli.md` is the reference.
`deploy/README.md` is the whole of the deploy kit: the release layout, the two
roles, the Nomad client configuration, macOS, rollback and the checks the kit
runs on itself.

## Install the `box` skill

The skill is one file, `claude-plugin/skills/box/SKILL.md`, and both install ways
use it. It needs the `box` CLI on `PATH`; it bundles no binary, no hook and no
MCP server.

**Claude Code, as a plugin** (recommended):

```sh
claude plugin marketplace add shady2k/nomad-driver-lima
claude plugin install box@nomad-driver-lima
```

**prime-agent, or any agent that reads a skill directory**:

```sh
install -D claude-plugin/skills/box/SKILL.md ~/.agents/skills/box/SKILL.md
```

The Ansible kit installs the skill for the coordinator in both ways: it copies
`SKILL.md` into `~/.agents/skills/box/` for prime-agent, and by default installs
the Claude Code plugin (`box_claude_skill_mode: plugin`); set it to `copy` for a
Claude Code that cannot add a marketplace.

## How a box works, in five lines

- A box id (`[a-z0-9-]{1,40}`) names a Lima instance: `LIMA_HOME/<id>/` is a real
  directory, and its `disk` file is a symlink into the box's host volume.
- The volume pins the box to one node. The driver never moves a VM; moving a box
  means moving its volume.
- `StartTask` is create-or-adopt; `RecoverTask` re-attaches to a live VM; a
  failed recover makes Nomad start the task again in the same allocation, which
  is the adopt path. `DestroyTask` stops the VM and keeps the disk.
- A resize is stop, edit, start: the guest grows its filesystem on the next boot.
- Inside, `boxd` runs one executor process per agent session over the Agent
  Client Protocol. Credentials live on the guest's tmpfs (`/run/box/secrets`),
  never on the box disk.

`docs/architecture.md` has the diagram and the reasoning.

## Layout

| path | contents |
| --- | --- |
| `cmd/box-driver` | the Nomad task driver plugin |
| `cmd/box-volume` | the Nomad dynamic host volume plugin |
| `cmd/box` | the `box` CLI |
| `cmd/boxd` | `boxd` (guest daemon) and `boxctl` (its client) in one binary |
| `internal/driver` | the driver: task config, lifecycle, stats, exec |
| `internal/box` | one Lima instance: disk on a volume, image cache, resize, cgroup cap |
| `internal/limactl` | a thin `limactl` wrapper |
| `internal/volume` | the dynamic host volume plugin logic |
| `internal/cli` | the CLI: jobs, volumes, executors, capacity, sessions, ACL policies |
| `internal/boxd` | the guest daemon, the session store, the executors |
| `images/` | the base image: recipe, build, publish, verify, guest checks |
| `deploy/` | the deploy kit: Ansible roles, ACL policies, the acceptance script |
| `claude-plugin/` | the `box` skill as a Claude Code plugin |
| `docs/` | architecture, the boxctl contract, the CLI reference, decisions, limits |
| `examples/` | a box job, a box volume and a client configuration |

## Build and test

```sh
go build ./...              # the CLI, the driver, the volume plugin, boxd
go vet ./...
go test ./...               # unit tests; a fake limactl, no VM needed
scripts/integration.sh      # real VMs on this host: needs limactl, qemu, nomad, an image
scripts/integration-cli.sh  # the CLI end to end against a real agent
deploy/ansible/tests/run-tests.sh   # the deploy kit's own checks: no VM, no root
```

## Security model: home-lab grade

Be clear about what this is. A box is a convenient, durable machine for work you
already trust. It is **not** a hard multi-tenant boundary, and nothing here is
hardened against a hostile guest or a hostile user of the CLI.

What is true:

- Each box runs as an unprivileged node user, in its own cgroup scope, with a
  memory cap (`memory.max` plus `memory.swap.max=0`).
- Executor credentials are written to the guest's tmpfs (`/run/box/secrets`,
  root-only, mode 0700) and are never on the box disk or in an image.
- The `box` CLI's Nomad token is scoped: jobs, host volumes, `alloc exec`, read
  logs, node read, and read/write of the one variable that holds the executor
  credentials. `deploy/acl/README.md` lists every capability and the command
  that needs it, and a test fails if the policy and the code drift apart.
- A box can read the executors variable and cannot write it, so one box cannot
  hand credentials to another.

What is **not** hardened:

- **All boxes share one set of executor credentials.** They are one Nomad
  variable. A compromised box can use them, and so can anyone who may read that
  variable — which includes anyone who may use the CLI.
- **The CLI token is close to platform-root.** It can `alloc exec` into any box,
  submit and stop jobs, create and delete host volumes, and rewrite the executor
  credentials. Treat a coordinator as an operator of every box on the cluster.
- **No network isolation.** A guest's traffic leaves through the node's
  user-mode NAT. There is no per-box firewall, no egress policy, and no private
  network between boxes. Two boxes on one node can reach each other only the way
  any two hosts on that network can.
- **The box is not a boundary against the node.** The VM is a process of the
  node's runtime user. A guest escape is a host compromise; the design reduces
  the blast radius of an ordinary box, not of a determined attacker.
- **Root inside the box.** `boxd` and every agent session run as root in the
  guest, and all sessions of a box share its disk. Sessions are separated by
  process, not by user or by filesystem.
- **Credentials at rest** live in the Nomad variable, which is stored in the
  cluster's state. This project does not encrypt it; your Nomad installation's
  mTLS and ACLs are the protection.
- **No quotas.** Nomad accounts for cpu, memory and disk per allocation, and the
  driver counts the VMs Nomad does not know about, but there is no per-user or
  per-box quota and no filesystem quota: a box that fills the shared volume
  filesystem affects its node.
- **Images are trusted by sha256 pin only.** There is no signature and no
  transparency log. You publish the images your boxes boot, and a box boots
  exactly the image its job names.
- **The macOS path is written and checked, not exercised.** Its configuration is
  rendered and validated on Linux, but this project's tests have not run it on a
  Mac. `deploy/README.md` lists what only a Mac can prove.

## Documentation

- `docs/architecture.md` — the parts, the disk, the lifecycle, the guest
- `docs/boxctl-contract.md` — the CLI↔guest interface: commands, JSON, errors
- `docs/cli.md` — every command and flag
- `docs/design-decisions.md` — why it is built this way, and what was rejected
- `docs/known-limits.md` — what is missing, fragile or unproven
- `deploy/README.md` — installing, rolling back, the kit's own checks
- `images/README.md` — building, publishing and verifying a base image
- `claude-plugin/skills/box/SKILL.md` — the skill an agent reads

## License

Apache-2.0 (`LICENSE`). `NOTICE` records the third-party components the compiled
driver links, whose own licenses apply.
