# box deploy kit

Everything needed to put the box platform on client nodes and the `box` CLI on a
coordinator, from an empty machine: one release directory per version with the box
driver and the volume plugin, lima and qemu, the runtime user, the cgroup
prerequisites, **the whole Nomad client configuration and its service** (a systemd
unit on Linux, a launchd daemon on macOS), and the CLI with its skill.

The box binaries come from a **published release** by default: the machine that
runs Ansible downloads them once, verifies them against the release's `SHA256SUMS`
and copies them to the hosts, so nothing has to be built and no node needs a route
to the release host (see "Where the release comes from" below).

The only things you supply are secrets and per-site values: an inventory, the
`group_vars` next to it, and the tokens (vaulted or from the environment). No
private hostname, mirror or registry of any particular site is in this repository:
the defaults point at the public upstreams, and everything else is a variable.

Nomad **servers** are not part of this kit: the nodes join servers you already have.
See "The Nomad servers" below for a one-host lab.

## From nothing, in one screen

```bash
# 0. Prerequisites on the machine that runs Ansible (README.md has the whole
#    list): ansible-core 2.22.0 - the version CI runs -, a route to the release
#    host or your own mirror, and Nomad servers the nodes can reach. The box
#    binaries of box_version come from the published release; nothing is built.
#    A release of your own is ./deploy/build.sh --version 0.1.0 and
#    box_release_source: local ("Where the release comes from" below).

cd deploy/ansible
cp -r inventory.example inventory        # then fill in hosts and group_vars
$EDITOR inventory/hosts.ini inventory/group_vars/*.yml    # box_version, nomad_servers, ...
$EDITOR inventory/group_vars/all/vault.yml   # ansible-vault create ... (secrets)

# 1. look first: every change on every host, and nothing changed
ansible-playbook -i inventory site.yml --check

# 2. everything: the release, the driver, the client configuration, the service,
#    the restart, the coordinator
ansible-playbook -i inventory site.yml

# 3. a cluster with ACLs: the policies, once, with a management token
ansible-playbook -i inventory acl.yml

# 4. the executor credentials the boxes run their agents with (once per cluster);
#    the script is in deploy/, so this step runs from the repository root
cd ../..
./deploy/executors-from-prime.sh --out /tmp/box-executors \
    --provider <provider> --base-url <https://api.example.com/v1>
box executors set --from-dir /tmp/box-executors --provider <provider>
box executors show                       # file names and sizes, never a value
rm -rf /tmp/box-executors

# 5. acceptance, on the coordinator, with the environment the role wrote
set -a; . ~/.config/box/env; set +a
./deploy/accept.sh --node node-01 --model <provider>/<model> \
    --restart-cmd 'ssh node-01 sudo systemctl restart nomad'
```

`--limit box_coordinators` runs only the coordinator play, `--limit node-01` only
one node. `deploy/accept.sh --dry-run` prints the ten steps of the acceptance run
and the commands, and runs nothing.

README.md is the guide from an empty machine - prerequisites, the guest image,
these steps and day 2 - and this file is the kit's reference.

## Where the release comes from

Both roles install the box binaries of `box_version`, and `box_release_source`
says where they come from.

**`download` (the default): the published release.** The machine that runs Ansible
fetches `box_release_base_url/SHA256SUMS` and the artifacts each host needs - a
node gets `box-driver_<os>_<arch>` and `box-volume_<os>_<arch>`, the coordinator
gets `box_<os>_<arch>` - into `box_release_cache_dir`, checks every one of them
against the digest `SHA256SUMS` gives it, and copies them to the hosts over the
Ansible connection.

- The fetch is delegated to `localhost` and run once, so **no node needs a route
  to the release host**.
- A cached artifact whose digest is still right is not fetched again, so a second
  node of the same architecture, and every later run, reads the cache.
- `--check` fetches too, into the controller's cache and never onto a host, so
  what it reports is checked against the real bytes. A run with the release host
  unreachable still installs from a warm cache.
- A digest that does not match stops the play before anything reaches a host, and
  so does a release that does not carry an artifact the host installs.

**`local`: a build of your own.** `box_dist_dir` (default `dist/<box_version>` at
the repository root) holds what `deploy/build.sh` wrote. Same verification, no
network, and the Go toolchain on that machine.

```yaml
box_version: '0.1.0'
# box_release_source: download
# box_release_base_url: 'https://github.com/shady2k/nomad-driver-lima/releases/download/v{{ box_version }}'
# box_release_cache_dir: '{{ lookup("env", "HOME") }}/.cache/nomad-driver-lima/releases/{{ box_version }}'
```

A site with its own mirror sets `box_release_base_url`; the assets and their
`SHA256SUMS` are the same bytes. A release's assets never change, which is what
makes the cache safe: `rm -rf {{ box_release_cache_dir }}` is the way back from a
cache that no longer matches - the checksums catch it, they never install it.

## The release layout: one directory per version, one symlink

Both roles install into the same shape, so a rollback is one symlink:

```
/opt/box/releases/0.1.0/plugins/box                      the box driver
/opt/box/releases/0.1.0/host_volume_plugins/box-volume    the volume plugin
/opt/box/releases/0.1.0/bin/box                           the CLI (coordinator)
/opt/box/current -> /opt/box/releases/0.1.0
```

- The Nomad client configuration names the paths **through `current`**
  (`plugin_dir = /opt/box/current/plugins`,
  `client.host_volume_plugin_dir = /opt/box/current/host_volume_plugins`), so the
  paths the client loads never change.
- The CLI is `/usr/local/bin/box -> /opt/box/current/bin/box`.
- A new version installs **next to** the live one; the symlink moves when the
  install is complete and verified against the release checksums.

A rollback switches between installed releases, and nothing else:

```bash
ansible-playbook -i inventory rollback.yml -e box_version=0.1.0 --check
ansible-playbook -i inventory rollback.yml -e box_version=0.1.0
```

It flips `current` on every node (and restarts that node's client once, one node at
a time) and on every coordinator (no restart: the CLI is one process). It does not
touch the client configuration, the client's `data_dir`, the Lima home, the image
cache, any volume directory or the box disks. A version that is not on the host is
refused, and the message lists the ones that are (`ls /opt/box/releases`). An old
release is removed with `rm -rf /opt/box/releases/<version>` once it is of no use.

## The two roles on a node, and why one renders the other's configuration

A node runs two roles, in this order:

| role | owns |
| --- | --- |
| `roles/box_node` | the release directory and the `current` symlink, the box driver and volume plugin binaries, `/etc/box/volume.json` (the volume plugin's settings of this node), lima, qemu, `/dev/kvm` access, the runtime user (kvm group, linger), the cgroup prerequisites of the memory cap |
| `roles/nomad_client` | the pinned Nomad binary, **all** of `/etc/nomad.d` (JSON, merged by file name), the secrets, the service - `nomad.service` or `com.hashicorp.nomad.plist` - `nomad config validate`, and the one restart of the client |

The driver's plugin block (`50-box.json`) is rendered by `nomad_client` **from
`box_node`'s variables** (`box_plugin_dir`, `box_limactl`, `box_lima_home`,
`box_runtime_user`, …). That is on purpose:

- one role writes the client configuration, so there is one place that decides what
  the client runs, one place that validates it, and one restart;
- `box_node` says only *what* the driver needs (`box_plugins_changed` when it copied
  a binary or moved the symlink, `box_runtime_user` for the plugin block); it writes
  no configuration and no unit drop-in, so nothing has to be kept in step with a
  second file that `nomad config validate` never sees;
- a play that runs only `nomad_client` (`nomad_box_enabled: false`) is a plain Nomad
  client; a play that runs only `box_node` is not supported, because a driver
  without a client configuration is not a working node.

The two roles meet in exactly one place: `roles/nomad_client/defaults/main.yml`,
where `nomad_box_*` reads `box_*` with a fallback.

## The box-volume settings file

A dynamic host volume request may carry **no** parameters: whoever creates the
volume (a coordinator, a job spec) must not decide which binary runs on the node,
which Lima home it uses or which user owns the volume directories. Those three are
settings of the node, and `box_node` writes them into one file:

```json
{
  "limactl": "/usr/local/bin/limactl",
  "lima_home": "/var/lib/box/lima",
  "runtime_user": "dev"
}
```

`/etc/box/volume.json`, mode 0644, owned by root; the plugin reads it on every
request and rejects any key it does not know, so a typo does not pass silently.
`BOX_VOLUME_CONFIG` names another file - for running the plugin by hand, not for a
node.

## The client runs as root, the boxes do not

The Nomad client runs as root (a systemd system unit, a launchd LaunchDaemon), and
Lima refuses to run as root. So the driver starts every child it needs - `limactl`
(create, start, stop, edit, list, `shell`, `copy`, and so the guest log stream and
`nomad alloc exec` too), the `systemd-run --user` scope of the memory cap and its
`systemctl --user` calls - as `box_runtime_user`: that user's uid, its supplementary
groups (including `kvm`), and an explicit environment (`PATH`, `HOME`, `LIMA_HOME`,
`XDG_RUNTIME_DIR=/run/user/<uid>`, `DBUS_SESSION_BUS_ADDRESS`), so nothing of the
root client's environment reaches a VM process. There is no default: the right
answer is the node's own unprivileged account, and guessing `root` would make every
box fail (`-e box_runtime_user=dev` is the one variable a run must name).

- `box_node` requires the user, adds it to the `kvm` group, enables linger, gives it
  the Lima home and the image cache, and writes it into `/etc/box/volume.json`.
- `nomad_client` renders it into the plugin block as `runtime_user` and puts
  `XDG_RUNTIME_DIR`/`DBUS_SESSION_BUS_ADDRESS` of that user into the unit's
  environment.
- Everything Lima touches belongs to that user: the Lima home, the image cache, the
  instance directory, the disk in the volume and its symlink, the per-start staging
  directory of the task files (`<lima_home>/_staging`, a name Lima skips), and the
  volume directory.

**Every ancestor of those paths has to let that user in.** A directory the role
creates can be perfectly right while the path above it is closed: on one node
`/opt/nomad` already existed as `root:root 0750`, so the volume directory below it
was created correctly and the first box stopped with permission denied. Once the
directories exist, the role asks the runtime user itself (`test -x`, the way the
driver will reach the box disk) about `nomad_host_volumes_dir` and the Lima home,
and fails the play with the path instead of leaving it to the first box.

Two ways to fix that ancestor:

- change it by hand - give the runtime user's group traversal, `root:<group> 0710`,
  rather than opening it to everyone;
- name it in `nomad_directory_parents`, which the role prepares **before** the leaf
  directories:

```yaml
# group_vars/box_nodes.yml
nomad_directory_parents:
  - path: '{{ nomad_host_volumes_dir | dirname }}'
    owner: '{{ nomad_file_owner }}'
    group: '{{ box_runtime_group }}'
    mode: '0710'
```

Review the ancestor and the group before you add either; a parent that is closed
on purpose is a decision about the whole node. The role never changes the mode of
`nomad_data_dir`, which keeps whatever the operator gave it.

A client that runs as an unprivileged user instead of root works too - point
`nomad_file_owner`/`box_plugin_owner` at that user - but the volume plugin then has
no `runtime_user` to give a volume directory to, so keep the two the same.

## The Nomad client configuration

`/etc/nomad.d/` is rendered from variables and loaded by the service the role writes:

| file | contents |
| --- | --- |
| `10-base.json` | region, datacenter, node name, `data_dir`, bind/advertise, ports, the client block (servers to join, node pool, meta, reserved resources, host volumes directory), telemetry, ACL, mTLS |
| `50-box.json` | `plugin_dir`, `client.host_volume_plugin_dir`, `client.options["driver.allowlist"]`, and the `plugin "box"` block (limactl, lima home, image cache, memory overhead, vm type, `runtime_user`) |
| `nomad.env` (Linux) | the tokens, mode 0600, loaded with `EnvironmentFile=`; on macOS they travel in the plist's `EnvironmentVariables` |

The Linux unit is `nomad.service`: `nomad agent -config /etc/nomad.d`, `Type=notify`,
`KillMode=process` (a restart signals only the nomad process, so the driver plugin,
the lima hostagents and the qemu processes of running boxes survive it),
`KillSignal=SIGINT`, `Delegate=yes`, and the two environment variables the memory cap
needs (`XDG_RUNTIME_DIR`, `DBUS_SESSION_BUS_ADDRESS` of the runtime user). The role
runs `nomad config validate /etc/nomad.d` before the restart and restarts only when
something changed.

Nothing in the repo has to be edited to change any of this: every key is a variable
in `roles/nomad_client/defaults/main.yml` (documented there), and
`nomad_client_extra` / `nomad_extra` merge anything else the client block or the
top-level configuration needs.

## macOS: the launchd daemon

A Mac runs the same two roles. What differs is the service: a **system
LaunchDaemon**, `/Library/LaunchDaemons/com.hashicorp.nomad.plist`, written by
`nomad_client` and loaded by the play (`launchctl bootstrap system …`).

**Why root, and not the runtime user.** Launchd has two domains: a system daemon
(`/Library/LaunchDaemons`, root, started at boot, no login session) and a per-user
agent (`~/Library/LaunchAgents`, needs that user to be logged in). The Nomad client
has to be there whether or not anyone is logged in, and it writes system paths
(`plugin_dir`, the host volumes directory, the allocation directories under
`data_dir`), so it is a LaunchDaemon as root. **Lima does not need root**: with
`vz` (Apple Virtualization, the default `vm_type` on macOS) the VM lives inside the
`limactl hostagent` process, which the driver starts as `box_runtime_user` - that
user's uid, `HOME` and `LIMA_HOME`. So the split is exactly the Linux one: the
client is root, every VM process is the unprivileged user, and the Lima home and the
image cache belong to that user.

**A restart that keeps the VMs.** The plist sets

```xml
<key>AbandonProcessGroup</key><true/>
```

which is the macOS form of `KillMode=process`: launchd signals the whole process
group of a job by default, and this keeps it from doing that, so the limactl
hostagents and the VMs inside them keep running while the client restarts, and Nomad
re-attaches to them (`RecoverTask`).

```bash
# what the sync of a configuration change does (the role does it for you)
sudo launchctl bootout system/com.hashicorp.nomad
sudo launchctl bootstrap system /Library/LaunchDaemons/com.hashicorp.nomad.plist

# a plain restart, keeping every box
sudo launchctl kickstart -k system/com.hashicorp.nomad

# the service, without removing anything of the node
sudo launchctl print system/com.hashicorp.nomad
```

The role writes the plist **0600**: it carries the two Nomad tokens in
`EnvironmentVariables` (macOS has no `EnvironmentFile`). `launchctl` needs it owned
by root, which the play guarantees (it runs with `become`).

**Uninstall**, keeping the boxes and the node's state:

```bash
ansible-playbook -i inventory uninstall.yml -e box_uninstall_confirm=true
```

That takes the daemon down, removes the plist, `/etc/nomad.d`, `/opt/box` (every
release), `/etc/box/volume.json` and the image cache. It keeps the Lima home with
its boxes, every volume directory and disk, the client's `data_dir` and
`host_volumes_dir`, and the runtime user. `box rm <id> --yes` is what deletes a box.

**What only a Mac can prove.** Everything here is rendered and validated on Linux
(see "The checks this kit runs on itself"): the arm64 configuration, the plist as a
property list, the launchd key set, the `vz` driver block. What a Linux host cannot
do is run it:

- `launchctl bootstrap` accepting the plist in the system domain (a real Mac, root),
- the client coming up under launchd, and `nomad node status` showing the box driver
  healthy on arm64 with `vz`,
- a `launchctl kickstart -k` leaving a running box alive (`AbandonProcessGroup`),
- `plutil -lint /Library/LaunchDaemons/com.hashicorp.nomad.plist` (macOS only),
- the VM paths themselves: `vz` needs macOS 13+, and the disks live on the APFS
  volume under the Lima home.

## macOS: the optional user LaunchAgent

The default stays the system LaunchDaemon. `profiles/macos-user.yml` runs the same
client as one login user instead: no root, everything under
`~/Library/Application Support/box`, and launchd's `gui/<uid>` domain.

```bash
cd deploy/ansible
ansible-playbook -i inventory site.yml --limit <mac-host> -e @profiles/macos-user.yml --check
```

**The trade-off is when it starts.** A LaunchAgent starts when that user logs in to
the GUI; the system LaunchDaemon starts at boot. A Mac that has rebooted and sits at
the login window runs no boxes until someone logs in. In exchange that Mac needs no
sudo, no root-owned paths and no system daemon - and the connection has to be the
same non-root user (`ansible_become: false` in the profile, and the Darwin
preflight refuses a user scope whose owner, uid or runtime user is not that user,
or whose paths would leave its home). One client per Mac: do not mix the two
scopes on one machine.

Nomad does not pass a plugin's environment through, so the volume plugin of that
profile reads its settings from the file beside the release's `box-volume`
(`host_volume_plugins/box-volume.json`), not from the plist. An
`/etc/box/volume.json` on the same Mac wins over it: resolve that, do not leave
both.

## NixOS nodes

The role refuses a NixOS node whose configuration or unit path is under `/etc`: on
NixOS that directory belongs to the Nix configuration, and a file written there is
lost at the next `nixos-rebuild switch`. Add the client configuration to the
machine's Nix configuration instead — the keys are the same as `10-base.json` and
`50-box.json`:

```nix
services.nomad = {
  enable = true;
  settings = {
    data_dir = "/opt/nomad/data";
    client = {
      enabled = true;
      servers = [ "nomad.example:4647" ];
      host_volumes_dir = "/opt/nomad/volumes";
      host_volume_plugin_dir = "/opt/box/current/host_volume_plugins";
      options."driver.allowlist" = "box";
    };
    plugin_dir = "/opt/box/current/plugins";
    plugin.box.config = {
      limactl = "/usr/local/bin/limactl";
      lima_home = "/var/lib/box/lima";
      image_cache_dir = "/var/lib/box/images";
      memory_overhead_mib = 150;
      # "enforce" on Linux, "off" on macOS: the v0.1.0 driver needs it written out,
      # newer ones default to the platform's answer.
      memory_limit = "enforce";
      runtime_user = "dev";
    };
  };
};
```

`box_node` (the release, the binaries, the settings file, the runtime user) still
runs there with `nomad_system_changes: false` — or set `nomad_allow_nixos: true` if
you really do manage `/etc/nomad.d` outside Nix.

## The Nomad servers

**Out of scope: this repository does not bring up Nomad servers, and needs none of
its own.** The nodes join servers you already have (`nomad_servers`), with TLS and
ACLs as that installation sets them. A fresh lab is still one host:
`nomad_client` renders a `server` block when you give it one, so a single machine
can be server and client:

```yaml
# group_vars/box_nodes.yml of a one-host lab
nomad_servers: ['192.0.2.11:4647']       # itself
nomad_server_block:
  enabled: true
  bootstrap_expect: 1
```

Anything bigger is a Nomad installation of its own (three servers,
`bootstrap_expect: 3`, mTLS, ACL bootstrap) and is not this kit's job.

## ACLs and secrets

- **Tokens in the inventory.** `vault_nomad_acl_token` (the CLI on the node),
  `vault_nomad_client_intro_token` (the client's introduction token) and
  `vault_nomad_management_token` (for `acl.yml`) are the only secrets, and they come
  from `ansible-vault` or the environment - `inventory.example/group_vars/vault.example.yml`
  shows both. They are written with `no_log`, mode 0600, and never into a JSON file.
- **`deploy/ansible/acl.yml`** applies the two policies of `deploy/acl/` with a
  management token: `box-operator` (the coordinator CLI) and, for the box ids you
  name, `box-workload-<id>` attached to that job's workload identity (`JobACL`). It
  reads each policy first, so a second run reports `changed=0`. The capabilities each
  policy grants, and why a box needs a job-attached policy at all, are in
  `deploy/acl/README.md`.
- `box create` does not touch ACLs on purpose (the CLI token has no ACL write), so a
  box created after `acl.yml` needs `box_acl_workload_jobs=[<id>]` once - or
  `deploy/acl/apply.sh workloads <id>`.
- **The executor credentials never touch a disk.** `box executors set` writes them
  into the Nomad variable `nomad/jobs/box-executors`; a box renders that variable
  into its task `secrets/` directory, and the driver writes those files to the
  guest's tmpfs at `/run/box/secrets/<executor>/` (root-only, mode 0700), not to the
  box disk. `box executors show` prints file names and sizes, never a value.

## Images

A box boots a base image that the coordinator's CLI finds through an image index.
There is no index baked into the binaries: set `box_image_index` in
`group_vars/box_coordinators.yml` (it becomes `BOX_IMAGE_INDEX` in
`~/.config/box/env`), or pass `--image-index` to a single command. Without it the
CLI says so and refuses to create a box.

The repository publishes no images of its own, and it never carries an image
binary. It keeps the recipe and its automation: `images/build.sh` builds an image,
`images/verify.sh` boots and checks it, `images/publish.sh` uploads it and its
`index.json` to the operator's store (with `BOX_STORE_URL`, and a token from
`BOX_STORE_TOKEN` or `BOX_STORE_TOKEN_FILE`). The release also carries `boxd` for
both guest architectures and `boxd-install.sh`, the tool for putting it into an
image of your own. `docs/guest-image.md` is what such an image must contain, the
`BOXD_*`/`BOX_MIRROR_*` settings, and the index format the coordinator reads.

## The release version must be semver

Nomad parses the version a plugin reports and refuses a plugin whose version is not
semver: `failed to parse plugin "box": malformed version`. A published release is
always `vX.Y.Z`, so this bites a `box_release_source: local` build: `deploy/build.sh`
takes its version from `git describe`, which gives `0.1.0-3-gabc123` for a tagged
release but something like `linux-prototype-0.1.29-20-gedb8206-dirty` when the tag
carries a prefix. Pass `--version 0.1.0` (or tag `v0.1.0`) for a release that a node
must load.

## The checks this kit runs on itself

```bash
./deploy/ansible/tests/run-tests.sh
```

No VM and no root: the roles are applied into throwaway prefixes under the
repository (`.test/`, gitignored). The script lints the roles, parses the four
playbooks, builds two releases, and then: serves those releases from a local HTTP
server and installs one over `box_release_source: download` (the digest check, a
second run from the cache with the server gone, `--check` on an empty cache, and a
wrong checksum and a release without the node's artifacts both refused); applies a
release into a prefix and checks
the layout, the symlink, `nomad config validate`, the unit, the secrets file, the
volume settings file and the launchd plist; switches between the two releases with
`rollback.yml` and checks that the configuration, the Lima home, the volumes and the
data directory are the same bytes afterwards; applies the ACL policies to a throwaway
agent with ACLs; starts a real **non-root** Nomad `-dev` agent on the rendered
configuration (its own ports, its data under the prefix) and asserts that the `box`
driver is healthy and the `box-volume` host volume plugin is fingerprinted through
the `current` symlink; and runs `uninstall.yml` to check what it removes and what it
keeps. It needs `nomad`, `limactl`, `qemu-system-x86_64`, `ansible-playbook`,
`ansible-lint`, `python3` and `go` in `PATH`.

## If a step fails, send back

1. The failing command and its whole output (`-vv` on `ansible-playbook` for the
   task that needs it), and the `--check` output of the same playbook.
2. `ls -l /opt/box/current /opt/box/releases` and the same for the coordinator.
3. `nomad node status -verbose <node> | grep -E 'box|host_volume'`, and
   `nomad config validate /etc/nomad.d`.
4. `box capacity` and the `box status <id>` of the box the acceptance run created.
5. The client log from the restart: `journalctl -u nomad -n 200 --no-pager` on Linux,
   `tail -200 /var/log/nomad.err.log` on the Mac.
