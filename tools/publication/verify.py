#!/usr/bin/env python3
"""Report privacy indicators without printing matched values. Complements secret scanners."""
import argparse
import collections
import ipaddress
import json
from pathlib import Path
import re
import tarfile
import zipfile

RULES = {
    'user_home': rb'/(?:Users|home)/[^/\s"\x27]+',
    'email': rb'\b[\w.+-]+@[\w.-]+\.[A-Za-z]{2,}\b',
    'uuid': rb'\b[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}\b',
    'key_marker': rb'-----BEGIN [A-Z ]*PRIVATE KEY-----',
    'credential_url': rb'https?://[^\s/@]+:[^\s/@]+@',
}

def inspect(name, data, blocked, findings):
    for category, pattern in RULES.items():
        count = len(re.findall(pattern, data, re.I))
        if count:
            findings.append({'path': name, 'category': category, 'count': count})
    for pattern in blocked:
        if pattern.search(data):
            findings.append({'path': name, 'category': 'operator_private_indicator', 'count': 1})
    addresses = []
    for candidate in re.findall(rb'\b(?:\d{1,3}\.){3}\d{1,3}\b', data):
        try:
            ip = ipaddress.ip_address(candidate.decode())
        except ValueError:
            continue
        if not ip.is_loopback and not ip.is_unspecified:
            addresses.append(ip)
    if addresses:
        findings.append({'path': name, 'category': 'non_loopback_ipv4', 'count': len(addresses)})


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('root', type=Path)
    p.add_argument('--private-patterns', type=Path, help='Private JSON list of regexes, kept outside the public export')
    p.add_argument('--report', required=True, type=Path)
    a = p.parse_args()
    blocked = [re.compile(x.encode(), re.I) for x in json.loads(a.private_patterns.read_text())] if a.private_patterns else []
    findings = []
    scanned = 0
    for path in sorted(a.root.rglob('*')):
        name = str(path.relative_to(a.root))
        if path.is_symlink():
            findings.append({'path': name, 'category': 'symlink', 'count': 1})
            continue
        if not path.is_file():
            continue
        scanned += 1
        data = path.read_bytes()
        inspect(name, data, blocked, findings)
        if path.suffix in ('.zip', '.gz', '.tgz', '.tar'):
            try:
                if zipfile.is_zipfile(path):
                    with zipfile.ZipFile(path) as archive:
                        for member in archive.infolist():
                            if not member.is_dir():
                                inspect(name + '!/' + member.filename, archive.read(member), blocked, findings)
                else:
                    with tarfile.open(path) as archive:
                        for member in archive:
                            if member.isfile():
                                inspect(name + '!/' + member.name, archive.extractfile(member).read(), blocked, findings)
            except (OSError, ValueError, tarfile.TarError, zipfile.BadZipFile):
                findings.append({'path': name, 'category': 'unreadable_archive', 'count': 1})
    a.report.write_text(json.dumps({'files_scanned': scanned, 'findings': findings}, indent=2) + '\n')
    print(json.dumps({'files_scanned': scanned, 'categories': dict(collections.Counter(x['category'] for x in findings))}))
    if any(x['category'] in ('operator_private_indicator', 'symlink', 'unreadable_archive') for x in findings):
        raise SystemExit(1)

if __name__ == '__main__':
    main()
