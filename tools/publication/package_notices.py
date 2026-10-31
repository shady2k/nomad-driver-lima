#!/usr/bin/env python3
"""Merge reviewed per-platform notice inventories into a distribution sidecar."""
import argparse
import hashlib
import json
from pathlib import Path
import shutil

SOURCE_NOTICE = '''Corresponding dependency source

DEPENDENCIES.json records exact module versions and public source archive URLs.
Go module archives correspond to the unmodified module versions used for these
builds; go-dependencies.sum records module checksums. Per-platform inventories
record compiled file hashes and individual SPDX headers when present. Dependency
closure counts describe compiler inputs, not a count after linker elimination.

Nomad contains BUSL-1.1 and MPL-2.0 source files. Its original root LICENSE,
including licensor, Additional Use Grant, Change Date and Change License
parameters, and applicable nested MPL texts are preserved byte for byte.
Original project code is licensed separately under Apache-2.0. This bundle does
not relicense any dependency. Dependencies were built without source changes.

MPL-covered dependency Source Code Form is available at corresponding_source_url
for each exact module in DEPENDENCIES.json. This notice supplies those locations
alongside the original license texts and does not restrict applicable license
rights. Go runtime sources and notices are covered by the inventory as well.

The guest provisioning recipe downloads external runtimes and tools. They are
not bundled in these driver binaries or source/caller archives. Distribution of
a prepared VM image requires its own installed-package license inventory.
'''

def escape_module(value):
    return ''.join('!' + char.lower() if char.isupper() else char for char in value)

def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--platform', action='append', required=True, help='PLATFORM=NOTICE_DIRECTORY')
    p.add_argument('--go-sum', required=True, type=Path)
    p.add_argument('--artifact-hashes', required=True, type=Path)
    p.add_argument('--output', required=True, type=Path)
    a = p.parse_args()
    a.output.mkdir(parents=True, exist_ok=False)
    modules, platforms = {}, {}
    for item in a.platform:
        platform, folder = item.split('=', 1)
        if not platform or '/' in platform or platform in ('.', '..'):
            raise SystemExit('Invalid platform')
        root = Path(folder)
        inventory = json.loads((root / 'inventory.json').read_text())
        if inventory['missing_notice_packages']:
            raise SystemExit('Unresolved dependency notices')
        dest = a.output / 'platforms' / platform
        dest.mkdir(parents=True)
        (dest / 'inventory.json').write_text(json.dumps(inventory, indent=2) + '\n')
        platforms[platform] = {'compiled_dependency_packages': len(inventory['packages']), 'modules_including_go': len(inventory['modules'])}
        for name, expected in inventory['notice_sha256'].items():
            source = root / name
            if source.is_symlink() or '..' in Path(name).parts or Path(name).is_absolute():
                raise SystemExit('Invalid notice path')
            data = source.read_bytes()
            if hashlib.sha256(data).hexdigest() != expected:
                raise SystemExit('Notice checksum mismatch')
            target = a.output / name
            target.parent.mkdir(parents=True, exist_ok=True)
            if target.exists() and target.read_bytes() != data:
                raise SystemExit('Platform notice disagreement')
            target.write_bytes(data)
        for module in inventory['modules']:
            key = module['module'] + '@' + module['version']
            record = modules.setdefault(key, dict(module))
            record['notice_files'] = sorted(set(record['notice_files']) | set(module['notice_files']))
            if module['module'] == 'go':
                record['corresponding_source_url'] = 'https://go.dev/dl/go' + module['version'] + '.src.tar.gz'
            else:
                record['corresponding_source_url'] = 'https://proxy.golang.org/' + escape_module(module['module']) + '/@v/' + escape_module(module['version']) + '.zip'
            record['modifications'] = 'No dependency source modifications in this build'
    shutil.copyfile(a.go_sum, a.output / 'go-dependencies.sum')
    (a.output / 'DEPENDENCIES.json').write_text(json.dumps({'platforms': platforms, 'modules': sorted(modules.values(), key=lambda x: x['module']), 'artifact_hashes': json.loads(a.artifact_hashes.read_text()), 'project_license': 'Apache-2.0 (original project code only)'}, indent=2) + '\n')
    (a.output / 'SOURCE-AVAILABILITY.txt').write_text(SOURCE_NOTICE)
    print(json.dumps({'platforms': platforms, 'unique_modules': len(modules)}))

if __name__ == '__main__':
    main()
