#!/usr/bin/env python3
"""Prepare a private Lima-only client config and discovery move plan. Never apply."""
import argparse
import copy
import hashlib
import json
import pathlib
import re
from urllib.parse import urlparse

FIELDS = {'runtime_user', 'limactl', 'lima_home', 'state_dir', 'template_file',
          'template_sha256', 'expected_version', 'memory_overhead_mib', 'disk_free_floor_mib'}

CATALOG_FIELDS = {'catalog_file', 'catalog_sha256', 'max_total_disk_gib'}

def candidate(source, lima, disabled_dir):
    c = copy.deepcopy(source)
    client = c.get('client', {})
    if client.get('enabled') is not True or (c.get('server') or {}).get('enabled', False):
        raise ValueError('Require a reviewed client-only configuration')
    if set(lima) - {'vault_address'} not in (FIELDS, FIELDS | CATALOG_FIELDS) or not isinstance(lima.get('runtime_user'), str) or lima['runtime_user'] in ('', 'root'):
        raise ValueError('Require the exact reviewed Lima driver config and nonroot runtime user')
    if not re.fullmatch(r'[a-f0-9]{64}', lima['template_sha256']):
        raise ValueError('Require exact reviewed template SHA256')
    if 'catalog_file' in lima:
        if not pathlib.PurePosixPath(lima['catalog_file']).is_absolute() or not re.fullmatch(r'[a-f0-9]{64}', lima['catalog_sha256']):
            raise ValueError('Require pinned absolute catalog')
        if type(lima['max_total_disk_gib']) is not int or lima['max_total_disk_gib'] <= 0:
            raise ValueError('Catalog requires an explicit positive retained-disk budget')
    if 'vault_address' in lima:
        origin = urlparse(lima['vault_address'])
        if origin.scheme != 'https' or not origin.hostname or origin.username or origin.password or origin.path not in ('', '/') or origin.query or origin.fragment:
            raise ValueError('Vault address must be an explicit HTTPS origin without credentials')
    if lima['expected_version'] != '2.2.1':
        raise ValueError('Only reviewed Lima 2.2.1 is prepared')
    if any((not isinstance(lima[k], int) or isinstance(lima[k], bool)) or lima[k] <= 0 for k in ('memory_overhead_mib', 'disk_free_floor_mib')):
        raise ValueError('Require positive integer resource guards')
    for k in ('limactl', 'lima_home', 'state_dir', 'template_file'):
        if not pathlib.PurePosixPath(lima[k]).is_absolute():
            raise ValueError('Lima paths must be absolute')
    plugin_dir = c.get('plugin_dir') or str(pathlib.PurePosixPath(c['data_dir'])/'plugins')
    plugin = pathlib.PurePosixPath(plugin_dir)
    disabled = pathlib.PurePosixPath(disabled_dir)
    if not plugin.is_absolute() or not disabled.is_absolute() or '..' in disabled.parts:
        raise ValueError('Require absolute reviewed plugin/discovery paths')
    if disabled == plugin or plugin in disabled.parents:
        raise ValueError('Disabled executable directory must be outside plugin discovery')
    options = client.setdefault('options', {})
    if 'lima' in options.get('driver.denylist', '').split(','):
        raise ValueError('Existing denylist conflicts with Lima; review separately')
    before_allowlist = options.get('driver.allowlist')
    options['driver.allowlist'] = 'lima'
    plugins = c.get('plugin', [])
    if not isinstance(plugins, list):
        raise ValueError('Expected Nomad JSON plugin block list')
    keep = []
    removed = 0
    for entry in plugins:
        entry = dict(entry)
        if entry.pop('nomad-driver-boxlite', None) is not None: removed += 1
        entry.pop('nomad-driver-lima', None)
        if entry: keep.append(entry)
    keep.append({'nomad-driver-lima': [{'config': [lima]}]})
    c['plugin'] = keep
    plan = {
        'action': 'prepare_only', 'applied': False, 'restart_performed': False,
        'target': 'one reviewed Linux client only',
        'allowlist_before': before_allowlist, 'allowlist_after': 'lima',
        'removed_boxlite_stanzas': removed,
        'discovery_move': {'source': str(plugin/'nomad-driver-boxlite'),
                           'destination': str(disabled/'nomad-driver-boxlite'),
                           'operation': 'move_after_fresh_empty_node_check_and_explicit_approval'},
        'lima_binary_destination': str(plugin/'nomad-driver-lima'),
        'preserve': ['all VM disks and runtime data', 'BoxLite sources/releases',
                     'client capacity/reservations', 'credentials/ACL/firewall',
                     'Mac clients', 'vm-agents server configuration'],
        'service_unit_change': False,
        'activation_gate': 'regression pass, reviewed config/discovery plan, fresh all-namespace and native VM/logmon checks, renewed restart/canary approval',
    }
    return c, plan

def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--source-config', required=True)
    p.add_argument('--lima-config', required=True)
    p.add_argument('--disabled-plugin-dir', required=True)
    p.add_argument('--output', required=True)
    a = p.parse_args()
    raw = pathlib.Path(a.source_config).read_bytes()
    c, plan = candidate(json.loads(raw), json.loads(pathlib.Path(a.lima_config).read_bytes()), a.disabled_plugin_dir)
    out = pathlib.Path(a.output).resolve()
    out.mkdir(mode=0o700, parents=True, exist_ok=False)
    data = (json.dumps(c, indent=2)+'\n').encode()
    plan.update(source_config_sha256=hashlib.sha256(raw).hexdigest(),
                candidate_sha256=hashlib.sha256(data).hexdigest())
    for name, body in [('client.lima-only.json', data),
                       ('transition-plan.json', (json.dumps(plan, indent=2)+'\n').encode())]:
        path = out/name
        path.write_bytes(body)
        path.chmod(0o600)
    print(str(out))

if __name__ == '__main__': main()
