#!/usr/bin/env python3
"""Read-only assertions over the Mac fixture prefixes.

test-macos-user.sh runs the plays and then this script. Nothing here writes: each
check names the file and the property that did not hold, so a failure in CI says
what the roles rendered.
"""

import argparse
import json
import os
import pathlib
import plistlib
import pwd
import stat


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def read_plist(path):
    with open(path, "rb") as handle:
        return plistlib.load(handle)


def read_json(path):
    with open(path, encoding="utf-8") as handle:
        return json.load(handle)


def mode_of(path):
    return stat.S_IMODE(path.stat().st_mode)


def check_user_install(prefix, version, user):
    """The user profile's install: a plist of the agent, the settings files, the config."""
    plist_path = prefix / "LaunchAgents" / "local.nomad.client.plist"
    plist = read_plist(plist_path)

    require("UserName" not in plist, f"{plist_path} carries UserName: a user agent must not")
    require(plist["Label"] == "local.nomad.client", f"{plist_path} has label {plist['Label']}")
    require(plist["AbandonProcessGroup"] is True, f"{plist_path} does not abandon the process group")
    arguments = plist["ProgramArguments"]
    require(arguments[1:3] == ["agent", "-config"], f"{plist_path} runs {arguments[1:3]}, not agent -config")
    require(str(prefix) in arguments[3], f"{plist_path} loads {arguments[3]}, which is not under {prefix}")
    require("-dev" not in arguments, f"{plist_path} starts a development agent")

    environment = plist["EnvironmentVariables"]
    require(environment.get("HOME") == str(pathlib.Path.home()), f"{plist_path} has HOME={environment.get('HOME')}")
    for secret in ("NOMAD_TOKEN", "NOMAD_CLIENT_INTRO_TOKEN"):
        require(secret not in environment, f"{plist_path} carries {secret}, and no token is set for this fixture")

    # The role creates the plist's directory only when it is missing, so the mode the
    # fixture gave it (0711, unlike anything the role writes) has to survive.
    require(mode_of(prefix / "LaunchAgents") == 0o711,
            f"{prefix}/LaunchAgents is {oct(mode_of(prefix / 'LaunchAgents'))}, not the 0711 the fixture gave it")

    release = prefix / "user-node" / "releases" / version
    require(os.path.samefile(prefix / "user-node" / "releases" / "current", release),
            f"{release} is not what the current symlink points at")

    # The volume plugin reads its three settings from the file beside its binary.
    settings_path = release / "host_volume_plugins" / "box-volume.json"
    settings = read_json(settings_path)
    require(settings["runtime_user"] == user, f"{settings_path} runs the volumes as {settings['runtime_user']}")
    require(settings["lima_home"] == str(prefix / "s" / "lima"), f"{settings_path} has lima_home {settings['lima_home']}")
    require(mode_of(settings_path) == 0o644, f"{settings_path} is {oct(mode_of(settings_path))}, not 0644")

    # The client configuration the role rendered for this Mac.
    config = read_json(prefix / "user-node" / "nomad" / "config" / "50-box.json")
    box = config["plugin"]["box"]["config"]
    require(box["vm_type"] == "vz", f"the plugin block has vm_type {box['vm_type']}, not vz")
    require(box["memory_limit"] == "off",
            f"the plugin block has memory_limit {box['memory_limit']}: macOS has no cgroup v2, so it is off")
    require(not pathlib.Path(box["limactl"]).is_absolute() or str(prefix) in box["limactl"],
            f"the plugin block runs {box['limactl']}, which is not the stub inside {prefix}")
    require(config["client"]["host_volume_plugin_dir"] == str(prefix / "user-node" / "releases" / "current" / "host_volume_plugins"),
            f"host_volume_plugin_dir is {config['client']['host_volume_plugin_dir']}, not the live release")


def check_lifecycle(prefix, domain, expect_username, user):
    """One scope's reload, restart and uninstall: the argv it sent, the plist it read."""
    label = "local.nomad.client"
    plist_path = str(prefix / "LaunchAgents" / f"{label}.plist")
    with open(prefix / "commands.jsonl", encoding="utf-8") as handle:
        commands = [json.loads(line) for line in handle if line.strip()]
    expected = [
        ["bootout", f"{domain}/{label}"],
        ["bootstrap", domain, plist_path],
        ["kickstart", "-k", f"{domain}/{label}"],
        ["bootout", f"{domain}/{label}"],
    ]
    require(commands == expected, f"{prefix}: launchctl was called as {commands}, not {expected}")

    rendered_path = prefix / "rendered.plist"
    rendered = read_plist(rendered_path)
    has_username = "UserName" in rendered
    require(has_username == expect_username,
            f"{rendered_path} {'has' if has_username else 'has no'} UserName in the {domain} domain")
    if expect_username:
        require(rendered["UserName"] == user, f"{rendered_path} runs as {rendered['UserName']}, not {user}")
    else:
        home = rendered["EnvironmentVariables"].get("HOME")
        require(home == str(pathlib.Path.home()), f"{rendered_path} has HOME={home}")

    # A box disk lives in the host volumes directory: the uninstall must keep it.
    marker = prefix / "user-node" / "nomad" / "volumes" / "retained-marker"
    require(marker.exists(), f"the uninstall removed the box disk {marker}")
    require(marker.read_text(encoding="utf-8") == "retained", f"{marker} is not what the fixture wrote")
    require(not pathlib.Path(plist_path).exists(), f"the uninstall left {plist_path}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--node", required=True, type=pathlib.Path,
                        help="the prefix the user-profile fixture installed into")
    parser.add_argument("--version", required=True,
                        help="the release version the fixtures installed (BOX_MAC_VERSION)")
    parser.add_argument("--user-lifecycle", required=True, type=pathlib.Path,
                        help="the prefix of the user-scope lifecycle fixture")
    parser.add_argument("--system-lifecycle", required=True, type=pathlib.Path,
                        help="the prefix of the system-scope lifecycle fixture")
    arguments = parser.parse_args()

    user = pwd.getpwuid(os.getuid()).pw_name
    check_user_install(arguments.node, arguments.version, user)
    check_lifecycle(arguments.user_lifecycle, f"gui/{os.getuid()}", False, user)
    check_lifecycle(arguments.system_lifecycle, "system", True, user)
    print(f"PASS: user plist, volume settings, rendered configuration, "
          f"user and system lifecycle argv, kept box disk (as {user})")


if __name__ == "__main__":
    main()
