#!/usr/bin/env python3
"""A fake limactl used by the unit tests.

State lives in $LIMA_HOME/.fake-state.json, failure switches in
$LIMA_HOME/.fake-mode.json (written by the test before a call), and every
invocation is appended to $LIMA_HOME/.fake-calls.log. Instance directories are
real directories under $LIMA_HOME, like lima's own.
"""
import hashlib
import io
import json
import os
import shutil
import sys
import tarfile

HOME = os.environ.get("LIMA_HOME")
if not HOME:
    sys.stderr.write("fake limactl: LIMA_HOME is not set\n")
    sys.exit(2)

STATE_PATH = os.path.join(HOME, ".fake-state.json")
MODE_PATH = os.path.join(HOME, ".fake-mode.json")
CALLS_PATH = os.path.join(HOME, ".fake-calls.log")
COPIES_PATH = os.path.join(HOME, ".fake-copies.log")
SEAL_PATH = os.path.join(HOME, ".fake-seal.log")


def load(path, default):
    try:
        with open(path) as f:
            return json.load(f)
    except FileNotFoundError:
        return default


def save_state(state):
    os.makedirs(HOME, exist_ok=True)
    with open(STATE_PATH, "w") as f:
        json.dump(state, f, indent=2)


def size_bytes(value):
    value = value.strip().strip('"')
    units = {"KIB": 1024, "MIB": 1024**2, "GIB": 1024**3, "TIB": 1024**4}
    upper = value.upper()
    for suffix, mult in units.items():
        if upper.endswith(suffix):
            return int(float(upper[: -len(suffix)]) * mult)
    return int(float(value))


def instance_dir(name):
    return os.path.join(HOME, name)


def name_of(args):
    """The instance name is the last positional argument of stop/start/edit."""
    return args[-1]


def main():
    args = sys.argv[1:]
    state = load(STATE_PATH, {})
    mode = load(MODE_PATH, {})
    os.makedirs(HOME, exist_ok=True)
    with open(CALLS_PATH, "a") as f:
        f.write(" ".join(args) + "\n")

    if not args:
        sys.stderr.write("fake limactl: no command\n")
        return 2

    cmd = args[0]

    if cmd == "--version":
        print("limactl version 2.2.0")
        return 0

    if cmd in ("list", "ls"):
        for name, inst in state.items():
            d = instance_dir(name)
            print(json.dumps({
                "name": name,
                "status": inst.get("status", "Stopped"),
                "dir": d,
                "vmType": inst.get("vmType", "qemu"),
                "arch": "x86_64",
                "cpus": inst.get("cpus", 0),
                "memory": inst.get("memory", 0),
                "disk": inst.get("disk", 0),
                "hostAgentPID": inst.get("hostAgentPID", 0),
                "driverPID": inst.get("driverPID", 0),
                "sshConfigFile": os.path.join(d, "ssh.config"),
                "limaVersion": "2.2.0",
            }))
        return 0

    if cmd == "clone":
        # clone OLDINST NEWINST [flags]: a copy of the instance directory --
        # lima copies the disk file, so the clone has a disk of its own.
        positional = [a for a in args[1:] if not a.startswith("-")]
        if mode.get("fail_clone"):
            sys.stderr.write("fake limactl: clone failed\n")
            return 1
        if len(positional) < 2:
            sys.stderr.write("fake limactl: clone needs two instance names\n")
            return 1
        old, new = positional[-2], positional[-1]
        if old not in state:
            sys.stderr.write("fatal: instance %s not found\n" % old)
            return 1
        if new in state:
            sys.stderr.write("fatal: instance %s already exists\n" % new)
            return 1
        src, dst = instance_dir(old), instance_dir(new)
        os.makedirs(dst, exist_ok=True)
        cfg = os.path.join(dst, "lima.yaml")
        for name in ("lima.yaml", "disk"):
            if os.path.exists(os.path.join(src, name)):
                shutil.copyfile(os.path.join(src, name), os.path.join(dst, name))
        if "--mount-none" in args and os.path.exists(cfg):
            # What lima's `--mount-none` does to the configuration: no mount at
            # all, above all no mount of the box's own task files.
            kept, in_mounts = [], False
            for line in open(cfg).read().splitlines(True):
                if line.startswith("mounts:"):
                    in_mounts = True
                    kept.append("mounts: []\n")
                    continue
                if in_mounts:
                    if line[:1] in (" ", "\t", "-"):
                        continue
                    in_mounts = False
                kept.append(line)
            open(cfg, "w").write("".join(kept))
        if mode.get("clone_symlink_disk"):
            # A limactl that copied the disk *link* instead of the file: the
            # clone's disk would be the box's own disk, which a bake must never
            # seal.
            os.remove(os.path.join(dst, "disk"))
            os.symlink(os.path.join(src, "disk"), os.path.join(dst, "disk"))
        inst = dict(state.get(old, {}))
        inst["status"] = "Stopped"
        inst["driverPID"] = 0
        inst["hostAgentPID"] = 0
        state[new] = inst
        save_state(state)
        return 0

    if cmd == "create":
        name = None
        for i, a in enumerate(args):
            if a == "--name":
                name = args[i + 1]
        yaml_path = args[-1]
        if mode.get("fail_create"):
            sys.stderr.write("fake limactl: create failed\n")
            return 1
        if name in state:
            sys.stderr.write("fatal: instance %s already exists\n" % name)
            return 1
        d = instance_dir(name)
        os.makedirs(d, exist_ok=True)
        shutil.copyfile(yaml_path, os.path.join(d, "lima.yaml"))
        cpus, memory, disk, vm_type = 0, 0, 0, "qemu"
        with open(yaml_path) as f:
            for line in f:
                key, _, value = line.strip().partition(":")
                value = value.strip()
                if key == "cpus":
                    cpus = int(value)
                elif key == "memory":
                    memory = size_bytes(value)
                elif key == "disk":
                    disk = size_bytes(value)
                elif key == "vmType":
                    vm_type = value
        with open(os.path.join(d, "disk"), "w") as f:
            f.write("FRESH-DISK")
        with open(os.path.join(d, "lima-version"), "w") as f:
            f.write("2.2.0\n")
        state[name] = {
            "status": "Stopped",
            "cpus": cpus,
            "memory": memory,
            "disk": disk,
            "vmType": vm_type,
        }
        save_state(state)
        return 0

    if cmd == "start":
        name = args[-1]
        inst = state.get(name)
        if inst is None:
            sys.stderr.write("fatal: no instance found\n")
            return 1
        if mode.get("fail_start"):
            sys.stderr.write("fake limactl: start failed\n")
            return 1
        inst["status"] = mode.get("start_status", "Running")
        inst["driverPID"] = mode.get("driver_pid", os.getppid())
        inst["hostAgentPID"] = mode.get("hostagent_pid", os.getppid())
        # lima names the driver's pid file after the VM type: qemu.pid, vz.pid
        with open(os.path.join(instance_dir(name), inst.get("vmType", "qemu") + ".pid"), "w") as f:
            f.write(str(inst["driverPID"]))
        with open(os.path.join(instance_dir(name), "ha.pid"), "w") as f:
            f.write(str(inst["hostAgentPID"]))
        save_state(state)
        return 0

    if cmd == "stop":
        force = "-f" in args or "--force" in args
        name = args[-1]
        inst = state.get(name)
        if inst is None:
            sys.stderr.write("fatal: no instance found\n")
            return 1
        if mode.get("fail_stop") and not force:
            sys.stderr.write("fake limactl: stop failed\n")
            return 1
        inst["status"] = "Stopped"
        inst["driverPID"] = 0
        inst["hostAgentPID"] = 0
        save_state(state)
        # lima removes its pid files when the instance stops
        for name in ("qemu.pid", "vz.pid", "ha.pid"):
            try:
                os.unlink(os.path.join(instance_dir(name_of(args)), name))
            except FileNotFoundError:
                pass
        return 0

    if cmd == "edit":
        name = args[-1]
        inst = state.get(name)
        if inst is None:
            sys.stderr.write("fatal: no instance found\n")
            return 1
        if inst.get("status") == "Running":
            sys.stderr.write("fatal: cannot edit a running instance\n")
            return 1
        i = 1
        while i < len(args) - 1:
            a = args[i]
            if a == "--cpus":
                inst["cpus"] = int(args[i + 1])
                i += 2
            elif a == "--memory":
                inst["memory"] = size_bytes(args[i + 1])
                i += 2
            elif a == "--disk":
                inst["disk"] = size_bytes(args[i + 1])
                i += 2
            else:
                i += 1
        save_state(state)
        return 0

    if cmd == "delete":
        name = args[-1]
        if mode.get("fail_delete"):
            sys.stderr.write("fake limactl: delete failed\n")
            return 1
        state.pop(name, None)
        save_state(state)
        shutil.rmtree(instance_dir(name), ignore_errors=True)
        return 0

    if cmd == "copy":
        if mode.get("fail_copy"):
            sys.stderr.write("fake limactl: copy failed\n")
            return 1
        with open(COPIES_PATH, "a") as f:
            f.write("\t".join(args[1:]) + "\n")
        return 0

    if cmd == "shell":
        if mode.get("fail_shell"):
            sys.stderr.write("fake limactl: shell failed\n")
            return 1
        joined = " ".join(args)
        # The driver runs the bundle's boxctl in the clone. The fake answers the
        # two calls a bake makes -- `info` (does boxd answer?) and `seal` -- and
        # records what the seal was given, so a test can assert on it without a
        # guest.
        if "boxctl" in joined:
            name = args[1] if len(args) > 1 else ""
            if joined.rstrip().endswith("seal") or " seal " in joined + " ":
                stdin = sys.stdin.buffer.read()
                with open(SEAL_PATH, "a") as f:
                    f.write("%s\t%d\t%s\n" % (name, len(stdin), hashlib.sha256(stdin).hexdigest()))
                if mode.get("fail_seal"):
                    print(json.dumps({"line": "closing the box's sessions and stopping its executors"}))
                    print(json.dumps({"error": {"code": "internal", "message": mode["fail_seal"]}}))
                    return 1
                print(json.dumps({"line": "closing the box's sessions and stopping its executors"}))
                print(json.dumps({"line": "the workspace: deleted /work (12.0 MiB)"}))
                print(json.dumps({"line": "guest/no-secrets.sh: 21 check(s) passed"}))
                print(json.dumps({"seal": {"image": {"schema": 1, "name": "project", "platform": "mount"}, "steps": []}}))
                return 0
            print(json.dumps({"boxd_version": "test", "sessions": 0, "seal": {"ok": True}}))
            return 0
        # The driver sends the task files as a tar stream on stdin. The fake
        # records each member's name and the sha256 of its content, never the
        # content: a test checks that no plaintext of a secret reaches the disk.
        if "tar -x" in " ".join(args):
            if mode.get("fail_copy"):
                # The connection drops after the first block: the driver is
                # still writing the stream.
                sys.stdin.buffer.read(512)
                sys.stderr.write("fake limactl: tar: unexpected end of stream\n")
                return 1
            data = sys.stdin.buffer.read()
            with tarfile.open(fileobj=io.BytesIO(data), mode="r|") as tar, open(COPIES_PATH, "a") as f:
                for member in tar:
                    content = tar.extractfile(member).read() if member.isfile() else b""
                    f.write("%s\t%o\t%d:%d\t%s\n" % (member.name, member.mode, member.uid, member.gid,
                                                     hashlib.sha256(content).hexdigest()))
            return 0
        print("fake shell ok")
        return 0

    sys.stderr.write("fake limactl: unsupported command %s\n" % cmd)
    return 1


if __name__ == "__main__":
    sys.exit(main())
