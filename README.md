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

## Prerequisites

The versions below are the ones this repository's code, its Ansible roles'
preflight checks and its CI workflows (`.github/workflows/`) use or measure.
Where a role only needs a minimum, that minimum is named.

### Nomad servers — you run them

- **Nomad 2.0 or newer.** A box's disk is a dynamic host volume, which arrived in
  2.0. The kit installs its clients at `nomad_version: '2.0.7'`, the version this
  project is measured with (`docs/known-limits.md`).
- **TLS and ACLs as your installation sets them.** The client role configures
  both (`nomad_tls_*`, `nomad_acl_*`); the CLI reads the usual `NOMAD_*`
  variables.
- **A management token, once.** `acl.yml` writes the two box policies, and only a
  management token may. It belongs to the operator; no node and no coordinator
  gets it.
- The servers are **not** part of this kit: the clients join servers you already
  run (`nomad_servers`). A one-host lab can render a `server` block with
  `nomad_server_block` (`deploy/README.md`, "The Nomad servers"). HashiCorp's own
  install guide: <https://developer.hashicorp.com/nomad/docs/install>.

### Linux nodes — amd64

The nodes run boxes. Each needs, before `site.yml` touches it:

| what | why, and where it is checked |
| --- | --- |
| **amd64** Linux | the published release carries `box-driver_linux_amd64` and `box-volume_linux_amd64` (`deploy/build.sh`). An arm64 Linux node needs a build of your own (`box_release_source: local`) and qemu for that architecture (`qemu-system-arm`, `qemu-utils`, `qemu-efi-aarch64` — the set the arm64 image build installs). |
| **`/dev/kvm`** | it is a qemu VM: hardware virtualisation on, the `kvm` module loaded, and the node not itself a VM without nested virtualisation. `box_node` preflight requires the device and read/write access for the runtime user. |
| **cgroup v2, systemd, and a user manager for the runtime user** | the memory cap of a box is a transient *user* scope (`memory.max`, `memory.swap.max=0`). `box_node` enables linger (`loginctl enable-linger`), starts `user@<uid>.service`, and proves that `systemd-run --user` starts a scope; it stops the play if it cannot. `box_cgroup_enabled: false` installs anyway and accepts uncapped memory. |
| **an unprivileged local user** | Lima refuses to run as root, and the client runs as root, so the driver starts every `limactl`, qemu and `systemd-run` child as this user (`box_runtime_user`, for example `dev`). The role adds it to the `kvm` group (when the node has that group), gives it the Lima home and the image cache — it does **not** create the user. |
| **lima 2.2 or newer** | `box_min_lima: '2.2'`; measured with 2.2.0. `box_limactl` names the binary (default `/usr/local/bin/limactl`). |
| **qemu and its firmware** | `qemu-system-x86_64` on `PATH` (`box_qemu_binary`). The distribution packages are `qemu-system-x86 qemu-utils ovmf` (`box_qemu_packages`); `ovmf` is the UEFI firmware lima boots an x86_64 guest with. `box_install_deps: true` installs those three, and lima from `box_lima_tarball_url`/`box_lima_tarball_sha256` when you name them; it is **off** by default. |
| **disk** | a box's disk lives on a Nomad volume (`nomad_host_volumes_dir`, default `/opt/nomad/volumes`), and the base image is cached in `box_image_cache_dir` (default `/var/lib/box/images`). `box_node` requires `box_disk_free_floor_mib` (2048) MiB free on the filesystem of `box_lima_home` and warns below `box_disk_free_warn_mib` (10240). Size it for every box disk plus one image. |

`box_node` and `nomad_client` also install the Nomad client itself: the node
fetches `nomad_version` from `nomad_release_url` at install time (a mirror is
fine — the pinned sha256 is what makes a mirror safe), so a node needs a route to
that URL or your mirror. A node that already has Nomad from its distribution
packages sets `nomad_install_binary: false` and points `nomad_binary` at it.

### macOS nodes — Apple Silicon

- **arm64 and macOS 13 or newer.** The release carries `box-driver_darwin_arm64`;
  the default `box_vm_type: vz` (Apple Virtualization) needs 13. An older Mac
  sets `box_vm_type: qemu` and installs qemu from Homebrew.
- **lima 2.2 or newer** from Homebrew: `brew install lima`, and
  `box_limactl: /opt/homebrew/bin/limactl`.
- With `vz` there is **no `/dev/kvm` and no qemu**: the VM is a child of the
  `limactl hostagent` that the driver starts as `box_runtime_user`.
- The Nomad client is a **launchd LaunchDaemon** as root
  (`/Library/LaunchDaemons/com.hashicorp.nomad.plist`), with
  `AbandonProcessGroup` so a restart leaves the boxes running. Everything else is
  the Linux setup: the same two roles, the same release layout.
- The macOS path is rendered and validated on Linux, not exercised on a Mac;
  `deploy/README.md` ("macOS: the launchd daemon") lists what only a Mac can
  prove.

### The machine you deploy from — the controller

- **`ansible-core` 2.22.0 and `ansible-lint` 26.10.1**, the versions CI runs:

  ```sh
  python3 -m pip install 'ansible-core==2.22.0' 'ansible-lint==26.10.1'
  ```

  The roles use `ansible.builtin` only, so there are no collections to install.
- **ssh to the hosts, and `sudo` on them**: the plays run with `become: true`.
- **No Go.** The deploy kit installs the published release: the controller
  downloads it once, checks it against the release's `SHA256SUMS`, and copies it
  to the hosts (`box_release_source: download`). The Go toolchain is needed only
  for [building from source](#building-from-source).
- **A route to the release host** (or your own mirror), or a warm cache in
  `box_release_cache_dir`.

### The guest image — where it is built, and where it is served

- **A host that can build it**: lima 2.2+ and `qemu-img`, and `/dev/kvm` for
  speed (without KVM qemu runs the guest with TCG — the same image, only slower;
  the arm64 CI runner has no KVM at all, see `.github/workflows/image.yml`).
- **Network to the upstreams the recipe pins** — Ubuntu's archive, nodejs.org,
  go.dev, PyPI, npm, GitHub, Docker Hub — or your own mirrors (`BOX_MIRROR_*`).
  Every download is version-pinned and checksummed, so a mirror serves the same
  bytes or the build stops.
- **An HTTP store the nodes can read.** The driver on each node downloads the
  image named by the index and verifies its sha256, so the store must be reachable
  from the nodes. `images/publish.sh` uploads to any HTTP store and updates
  `index.json` there (`BOX_STORE_URL`, and a token from `BOX_STORE_TOKEN` or
  `BOX_STORE_TOKEN_FILE`).
- **The index** the coordinator reads: `box_image_index`, which becomes
  `BOX_IMAGE_INDEX` in the coordinator's environment file.

### The coordinator

- **Network to the Nomad API**, over TLS with the CA of that cluster.
- **A Nomad token with the `box-operator` policy.** `deploy/acl/README.md` lists
  every capability it grants; `deploy/acl/apply.sh operator` writes the policy and
  mints the token.
- **Claude Code or the Prime Agent**, for the `box` skill. The coordinator role
  installs the skill for both: a copy in `~/.agents/skills/box/` for the Prime
  Agent, and the Claude Code plugin by default (`box_claude_skill_mode: plugin`;
  `copy` writes `~/.claude/skills/box/SKILL.md` instead, for a Claude Code that
  cannot add a marketplace).

### Model provider credentials for the boxes

The agents inside a box need one provider's credentials and configuration. They
are stored once per cluster, in the Nomad variable `nomad/jobs/box-executors`, by
`box executors set` — never in the image and never in the box job. Any provider
prime-agent or opencode can talk to works.

## Deploy, step by step

Each step says what to run and what success looks like. Replace every
`example.com`, `node-01` and `<provider>` with your own; nothing in this
repository names a host, a mirror or a secret. Commands run from the repository
root unless the step changes the directory; the playbooks are run from
`deploy/ansible`, where `ansible.cfg` is.

The order matters: the box binaries and the image have to exist before a node can
use them, and the coordinator needs the cluster before it can create a box.

### 1. Pick the release version

The box binaries are published as GitHub releases: tag `vX.Y.Z` becomes release
`vX.Y.Z` with every artifact `deploy/build.sh` builds plus `SHA256SUMS`
(`.github/workflows/release.yml`). Choose one:

<https://github.com/shady2k/nomad-driver-lima/releases>

Note the version you picked: step 5 writes it into
`inventory/group_vars/all.yml` as `box_version: '0.1.0'` — the tag without its
`v`, and it must be semver.

A site that mirrors releases sets `box_release_base_url` instead of using GitHub;
the assets and their `SHA256SUMS` are the same bytes.

**Success:** this prints the checksums the roles will verify:

```sh
curl -fsSL https://github.com/shady2k/nomad-driver-lima/releases/download/v0.1.0/SHA256SUMS
```

### 2. Build the guest image

A box boots an image you build and publish yourself; this repository keeps the
recipe and never a binary image. Give the image the `boxd` of the same release as
the nodes and the CLI — one release is one contract between driver, CLI and guest.

```sh
export BOXD_RELEASE=v0.1.0            # boxd_linux_<arch> from that release, sha256 checked
images/build.sh amd64 1.0.0           # -> out/box-base-1.0.0-amd64.qcow2 (+ .sha256, .json)
```

A closed network sets mirrors for the build's sources — the recipe names no
private one (`images/README.md` has the whole list):

```sh
export BOX_MIRROR_APT=https://apt.example.com/ubuntu
export BOX_MIRROR_APT_SECURITY=https://apt.example.com/ubuntu-security
export BOX_MIRROR_PYPI=https://pypi.example.com/simple
images/build.sh amd64 1.0.0
```

The image is a VM with an 8 GiB virtual disk by default; a box's `disk_gib` can
only grow it.

**Success:** the script ends with `built out/box-base-1.0.0-amd64.qcow2`, its
sha256 and size, and writes the manifest that records every source and the
`boxd` it carries. `guest/no-secrets.sh` ran before the disk was captured.

Bringing your own image instead is supported: `docs/guest-image.md` is the
contract it has to meet, and `guest/boxd-install.sh` puts `boxd` into it in one
command.

### 3. Verify the image

```sh
IMAGE_PATH=out/box-base-1.0.0-amd64.qcow2 OUT=out \
  images/verify.sh amd64 1.0.0
```

It boots the image the way the driver does and runs `images/guest-checks.sh`
inside it. **Success:** one `PASS` line per check and exit status 0 — the image
boots, cloud-init finishes, `boxd.service` is enabled and active, the executors
are available, the credential paths are tmpfs, and the disk grew into its
partition. `docs/guest-image.md` lists every check.

### 4. Publish the image and its index

```sh
export BOX_STORE_URL=https://store.example.com/box-images
export BOX_STORE_TOKEN_FILE=/run/secrets/store-token      # or BOX_STORE_TOKEN
images/publish.sh amd64 1.0.0

BOX_STORE_URL=$BOX_STORE_URL images/verify.sh amd64 1.0.0  # the published copy
```

`publish.sh` refuses a version that already exists (a published version never
changes), uploads the image, its `.sha256` and its manifest, reads them back, and
only then merges the version into `index.json` under a lock.

**Success:** `https://store.example.com/box-images/index.json` lists
`images["1.0.0"]["amd64"]` with a `url` and a `sha256`. Point the coordinator at
that document:

```yaml
# inventory/group_vars/box_coordinators.yml
box_image_index: 'https://store.example.com/box-images/index.json'
```

### 5. Write the inventory and the vault

```sh
cd deploy/ansible
cp -r inventory.example inventory
$EDITOR inventory/hosts.ini inventory/group_vars/*.yml
ansible-vault create inventory/group_vars/all/vault.yml    # the tokens
```

The example inventory documents every variable in the place an operator sets it;
the ones a run must give a value are these:

| file | variable | what it is |
| --- | --- | --- |
| `inventory/hosts.ini` | `box_nodes`, `box_nodes_mac`, `box_coordinators` | the hosts of each group (`ansible_host`, `ansible_user`) |
| `group_vars/all.yml` | `box_version` | the release of step 1 |
| `group_vars/all.yml` | `nomad_servers` | the RPC addresses (port 4647) of your servers |
| `group_vars/box_nodes.yml` | `box_runtime_user` | the unprivileged user of the node (no default) |
| `group_vars/box_nodes.yml` | `box_limactl`, `box_qemu_binary` | where lima and qemu are on that node |
| `group_vars/box_nodes_mac.yml` | `box_runtime_user`, `box_limactl`, `box_vm_type: vz` | the Mac's half of the same |
| `group_vars/box_coordinators.yml` | `box_nomad_addr`, `box_image_index` | the Nomad API and the image index of step 4 |
| `vault.yml` | `vault_nomad_acl_token`, `vault_nomad_client_intro_token`, `vault_nomad_management_token` | the cluster's tokens; see `group_vars/vault.example.yml` for the two ways to give them |

With a vaulted `vault.yml`, every `ansible-playbook` below takes
`--ask-vault-pass` (or `ANSIBLE_VAULT_PASSWORD_FILE`).

**Success:** this lists the three groups and the hosts you gave them, and
complains about no undefined or undecryptable value:

```sh
ansible-inventory -i inventory --list >/dev/null && echo inventory ok
```

### 6. Look first, then install

```sh
ansible-playbook -i inventory site.yml --check        # every change, nothing changed
ansible-playbook -i inventory site.yml                # the nodes, then the coordinator
```

`--check` downloads the release into the controller's cache and then reports what
each host would get; it writes nothing on a host. `site.yml` runs two plays: the
node roles (`box_node`, then `nomad_client`) and then the coordinator, one node
at a time (`serial: 1`), so one client restarts while the others keep their boxes.
`--limit node-01` (or `--limit box_coordinators`) narrows a run.

**Success:**

- the `PLAY RECAP` reports `failed=0`, and a second run reports `changed=0`;
- on a node, `nomad node status -verbose <node-id> | grep -E 'box|host_volume'`
  shows `box  true  true  Healthy`, the configured `runtime_user` and the volume
  plugin's version (it needs the coordinator's environment: step 9);
- `/opt/box/current` points at `/opt/box/releases/0.1.0`;
- `box` is on `PATH` on the coordinator.

### 7. The ACL policies — a cluster with ACLs

```sh
ansible-playbook -i inventory acl.yml --check         # shows the policies
ansible-playbook -i inventory acl.yml                 # applies them
```

It needs a management token (`vault_nomad_management_token`, or
`-e box_acl_management_token=$NOMAD_MANAGEMENT_TOKEN`). It writes
`box-operator` (the coordinator CLI) and, for the box ids you name in
`box_acl_workload_jobs`, `box-workload-<id>` attached to that box job's workload
identity. A second run reports `changed=0`.

Then mint the coordinator's own token — a management token can, and
`deploy/acl/apply.sh` does both halves at once (the rest of this guide runs from
the repository root):

```sh
cd ../..
NOMAD_ADDR=https://nomad.example:4646 NOMAD_TOKEN="$NOMAD_MANAGEMENT_TOKEN" \
  ./deploy/acl/apply.sh operator
```

Put that token in `box_nomad_token` (vaulted) and run the coordinator play again
(`cd deploy/ansible`, then `ansible-playbook -i inventory site.yml --limit box_coordinators`).
A box created after this run needs its workload policy once more:
`deploy/acl/apply.sh workloads <box id>` — `box create` does not touch ACLs on
purpose.

**Success:** `nomad acl policy list` shows `box-operator`, and the coordinator's
`box capacity` answers with that token and with it alone (its environment is
step 9).

### 8. The executor credentials

The boxes run agents, and their credentials travel as one Nomad variable. Stage
them from a host that has a prime-agent configuration (from the repository root):

```sh
./deploy/executors-from-prime.sh --out /tmp/box-executors \
    --provider <provider> --base-url https://api.example.com/v1
```

then, where the CLI can reach the cluster — the coordinator, with the environment
of step 9:

```sh
box executors set --from-dir /tmp/box-executors --provider <provider>
box executors show                       # file names and sizes, never a value
rm -rf /tmp/box-executors
```

**Success:** `box executors show` lists the files of each executor and no
credential value. Rotation needs no new box: Nomad re-renders the variable and
the driver copies it into the guest's tmpfs.

### 9. The coordinator: environment and skill

The role wrote `~/.config/box/env` (mode 0600) with `NOMAD_ADDR`, the token and
`BOX_IMAGE_INDEX`. Every shell that runs `box` reads it:

```sh
set -a; . ~/.config/box/env; set +a
```

The skill is installed for the Prime Agent as a copy of
`claude-plugin/skills/box/SKILL.md`. For Claude Code the role runs:

```sh
claude plugin marketplace add shady2k/nomad-driver-lima
claude plugin install box@nomad-driver-lima
```

The role does both by default. `box_claude_skill_mode: copy` writes
`~/.claude/skills/box/SKILL.md` instead of installing the plugin (for a Claude
Code that cannot add a marketplace), and a hand-run install is the same two
commands. Without Ansible at all the skill is still one file, and any agent that
reads a skill directory takes a copy of it:

```sh
install -D claude-plugin/skills/box/SKILL.md ~/.agents/skills/box/SKILL.md
```

**Success:** `claude plugin list` shows `box@nomad-driver-lima`, and `box help`
prints the command list.

### 10. Capacity, images and the first box

```sh
box capacity     # which nodes can take a box, and how much room each has
box images       # the base image versions your index publishes
```

**Success:** `box capacity` lists every node with its driver version, free cpu,
memory and disk; `box images` lists `1.0.0` for the node's architecture.

```sh
box create dev1 --image 1.0.0 --cpus 2 --memory-mib 2048 --disk-gib 16
box run dev1 -- uname -m

# an agent session inside it (the executor's credentials are step 8's)
box session new dev1 --executor prime --model <provider>/<model> --title "first run"
box send dev1 s-7f3a2c "make the tests pass; the repository is in /root/repo"
box wait dev1 s-7f3a2c --timeout 15m          # returns the moment the turn ends

box stop dev1                                # keep the disk, free the RAM
box rm dev1 --yes                            # the disk is deleted
```

### 11. Acceptance

`deploy/accept.sh` walks the whole path with the CLI alone and prints one `PASS`
or `FAIL` per step. It runs on the coordinator, with the environment of step 9:

```sh
./deploy/accept.sh --dry-run                 # the ten steps and their commands
./deploy/accept.sh --node node-01 --model <provider>/<model> \
    --restart-cmd 'ssh node-01 sudo systemctl restart nomad'
```

**Success:** ten `PASS` lines and exit status 0, including step 9: the Nomad
client restarts and the same allocation, VM and session are still there.

### 12. Day 2: upgrade, roll back, uninstall

**Upgrade to a new release**: set `box_version` to the new tag (or pass
`-e box_version=0.1.1`) and run `site.yml` again (`cd deploy/ansible` first, as in
step 6). The new release installs next to the live one, `current` moves when the
install is complete and verified, and each node's client restarts once.

**Roll back** to a release that is already on the host:

```sh
ansible-playbook -i inventory rollback.yml -e box_version=0.1.0 --check
ansible-playbook -i inventory rollback.yml -e box_version=0.1.0
```

It moves the `current` symlink and restarts the client (KillMode=process on
Linux, AbandonProcessGroup on macOS: the boxes keep running). It does not touch
the client configuration, the client's `data_dir`, the Lima home, the image cache,
a volume directory or a box disk. A version that is not on the host is refused,
and the message names the ones that are. An old release is removed with
`rm -rf /opt/box/releases/<version>`.

**Uninstall**, keeping the boxes and the node's state:

```sh
ansible-playbook -i inventory uninstall.yml -e box_uninstall_confirm=true
```

A node loses the client service and configuration, `/opt/box` (every release),
`/etc/box/volume.json` and the image cache. It keeps the Lima home with its boxes,
every volume directory and its disk, the client's `data_dir` and the runtime user.
`box rm <id> --yes` is what deletes a box.

`deploy/README.md` is the reference for all of this: the release layout, the two
roles, the client configuration, the box-volume settings file, macOS, NixOS, the
kit's own checks and what to send back when a step fails.

## Building from source

The deploy kit installs a published release by default. To install a build of your
own, on the machine that runs Ansible:

```sh
go version                    # go.mod asks for go 1.26.7 or newer
./deploy/build.sh --version 0.1.0        # -> dist/0.1.0/ and its SHA256SUMS
```

and tell the roles to read that directory instead of downloading:

```yaml
# inventory/group_vars/all.yml
box_release_source: local
```

The version must be semver (`0.1.0`, or tag `v0.1.0`): Nomad parses the version a
plugin reports and refuses one it cannot. `--linux-only` builds only the Linux
amd64 node; a Mac node needs a full build. `box_dist_dir` (default
`dist/<box_version>` at the repository root) says where the release is.

## Build and test

```sh
go build ./...              # the CLI, the driver, the volume plugin, boxd
go vet ./...
go test ./...               # unit tests; a fake limactl, no VM needed
scripts/integration.sh      # real VMs on this host: needs limactl, qemu, nomad, an image
scripts/integration-cli.sh  # the CLI end to end against a real agent
deploy/ansible/tests/run-tests.sh   # the deploy kit's own checks: no VM, no root
python3 images/publish_test.py      # images/publish.sh against a local fake store
images/build.sh amd64 0.0.0-test    # a real image build (~30 min), then images/verify.sh
```

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
| `internal/driver`, `internal/box`, `internal/volume` | the driver, one Lima instance, the volume plugin logic |
| `internal/cli`, `internal/boxd` | the CLI, and the guest daemon with its session store |
| `images/` | the guest image recipe and its automation: build, verify, publish (no image binary is ever in the repository) |
| `guest/` | the operator's tools for their own image: `boxd-install.sh`, `no-secrets.sh` |
| `deploy/` | the deploy kit: Ansible roles, ACL policies, the acceptance script |
| `claude-plugin/` | the `box` skill as a Claude Code plugin |
| `docs/` | architecture, the boxctl contract, the CLI reference, decisions, limits, the guest image |
| `examples/` | a box job, a box volume and a client configuration |

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
- **Images are trusted by sha256 pin only.** There is no signature and no
  transparency log. You publish the images your boxes boot, and a box boots
  exactly the image its job names. The kit verifies the binaries it installs
  against the release's `SHA256SUMS` for the same reason: pin the bytes, not the
  address.

What is **not** hardened:

- **All boxes share one set of executor credentials** — one Nomad variable. A
  compromised box can use them, and so can anyone who may read that variable,
  which includes anyone who may use the CLI.
- **The CLI token is close to platform-root.** It can `alloc exec` into any box,
  submit and stop jobs, create and delete host volumes, and rewrite the executor
  credentials. Treat a coordinator as an operator of every box on the cluster.
- **No network isolation.** A guest's traffic leaves through the node's
  user-mode NAT; there is no per-box firewall, no egress policy and no private
  network between boxes.
- **The box is not a boundary against the node.** The VM is a process of the
  node's runtime user. A guest escape is a host compromise.
- **Root inside the box.** `boxd` and every agent session run as root in the
  guest and share its disk. Sessions are separated by process, not by user or by
  filesystem.
- **Credentials at rest** live in the Nomad variable, in the cluster's state.
  This project does not encrypt it; your Nomad installation's mTLS and ACLs are
  the protection.
- **No quotas.** Nomad accounts for cpu, memory and disk per allocation and the
  driver counts the VMs Nomad does not know about, but there is no per-user or
  per-box quota and no filesystem quota: a box that fills the shared volume
  filesystem affects its node.
- **The macOS path is written and checked, not exercised.** Its configuration is
  rendered and validated on Linux; this project's tests have not run it on a Mac.

## Documentation

- `docs/architecture.md` — the parts, the disk, the lifecycle, the guest
- `docs/boxctl-contract.md` — the CLI↔guest interface: commands, JSON, errors
- `docs/cli.md` — every command and flag
- `docs/design-decisions.md` — why it is built this way, and what was rejected
- `docs/known-limits.md` — what is missing, fragile or unproven
- `deploy/README.md` — the deploy kit: installing, rolling back, the kit's own checks
- `docs/guest-image.md` — how to build or bring your own box image, what it must contain, and the image index format
- `images/README.md` — the recipe and its automation: build, verify, publish, and the boxd source it takes
- `claude-plugin/skills/box/SKILL.md` — the skill an agent reads

## License

Apache-2.0 (`LICENSE`). `NOTICE` records the third-party components the compiled
driver links, whose own licenses apply.
