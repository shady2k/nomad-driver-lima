#!/usr/bin/env python3
"""Export an explicitly reviewed source manifest; never copy Git history or caches."""
import argparse
import hashlib
import io
import json
from pathlib import Path, PurePosixPath
import subprocess
import tarfile

ROOT = Path(__file__).resolve().parents[2]
HERE = Path(__file__).resolve().parent

def digest(data):
    return hashlib.sha256(data).hexdigest()

def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--source', type=Path, default=ROOT)
    p.add_argument('--ref', required=True, help='Exact fully integrated source commit, not a working directory')
    p.add_argument('--output', type=Path, required=True)
    a = p.parse_args()
    source = a.source.resolve()
    commit = subprocess.check_output(['git', '-C', str(source), 'rev-parse', a.ref + '^{commit}'], text=True).strip()
    policy = json.loads((HERE / 'manifest.json').read_text())
    raw = subprocess.check_output(['git', '-C', str(source), 'archive', '--format=tar', commit])
    with tarfile.open(fileobj=io.BytesIO(raw)) as archive:
        files = {m.name: m for m in archive.getmembers() if not m.isdir()}
        known = set(policy['include']) | set(policy['exclude'])
        unknown = sorted(set(files) - known)
        missing = sorted(set(policy['include']) - set(files))
        if unknown or missing:
            raise SystemExit('Manifest review required: ' + json.dumps({'unreviewed_paths': unknown, 'missing_public_paths': missing}))
        # Validate everything before creating the destination. Git symlinks and special files are rejected.
        for name in policy['include']:
            member = files[name]
            path = PurePosixPath(name)
            if path.is_absolute() or '..' in path.parts or not member.isfile():
                raise SystemExit('Unsafe archive member')
            if any(x in path.parts for x in ('.git', '__pycache__', 'bin', '.aws', '.ssh')) or path.suffix in ('.pyc', '.test', '.log', '.zip', '.gz'):
                raise SystemExit('Disallowed publication member')
        a.output.mkdir(parents=True, exist_ok=False)
        hashes = {}
        for name in sorted(policy['include']):
            member = files[name]
            data = archive.extractfile(member).read()
            dest = a.output / name
            dest.parent.mkdir(parents=True, exist_ok=True)
            dest.write_bytes(data)
            dest.chmod(0o755 if member.mode & 0o111 else 0o644)
            hashes[name] = digest(data)
        # Review evidence lives beside the export, not inside the public package.
        a.output.with_suffix('.provenance.json').write_text(json.dumps({'source_commit': commit, 'manifest_sha256': digest((HERE / 'manifest.json').read_bytes()), 'files': hashes}, indent=2) + '\n')
        print(json.dumps({'source_commit': commit, 'public_files': len(hashes), 'excluded_files': len(policy['exclude'])}))

if __name__ == '__main__':
    main()
