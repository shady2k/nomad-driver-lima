#!/usr/bin/env python3
"""Build a local, checksum-addressed resume review candidate. Never deploys."""
import argparse
import gzip
import hashlib
import json
import os
import pathlib
import re
import subprocess
import sys
import tarfile
import tempfile

ROOT = pathlib.Path(__file__).resolve().parents[1]
TARGETS = [('linux', 'amd64'), ('darwin', 'arm64')]


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def run(args, env, log=None):
    result = subprocess.run(args, cwd=ROOT, env=env, capture_output=True)
    if log is not None:
        log.write(json.dumps(args).encode() + b'\n' + result.stdout + result.stderr + b'\n')
        log.flush()
        os.fsync(log.fileno())
    if result.returncode:
        raise RuntimeError('command failed; inspect verification.log: ' + args[0])
    return result.stdout


def archive(path, data):
    with path.open('wb') as raw:
        with gzip.GzipFile(filename='', fileobj=raw, mode='wb', mtime=0) as compressed:
            compressed.write(data)


CALLER_HELPERS={'plugin/guest/repository_exec.py','plugin/guest/repository_profile.py'}
DEPLOY_HELPERS={'plugin/guest/repository_profile.py'}
def package_files(tracked,kind):
    tracked=set(tracked)
    required=CALLER_HELPERS if kind=='caller' else DEPLOY_HELPERS
    if not required<=tracked:raise ValueError('Required repository schema/executor package dependency missing')
    if kind=='caller':return sorted(name for name in tracked if name.startswith('plugin/')and(not name.startswith(('plugin/guest/','plugin/tests/'))or name in required))
    if kind=='deploy':return sorted({name for name in tracked if name.startswith('deploy/')}|required)
    raise ValueError('Unknown package kind')

def verify_packages(output,env):
    with tempfile.TemporaryDirectory(prefix='nomad-resume-package-')as folder:
        root=pathlib.Path(folder)
        for kind,name in [('caller','caller-plugin.tar.gz'),('deploy','deploy-bundle.tar.gz')]:
            target=root/kind;target.mkdir()
            with tarfile.open(output/name,'r:gz')as archive_file:archive_file.extractall(target,filter='data')
            if kind=='caller':
                code='import sys;sys.path.insert(0,sys.argv[1]);import guest_exec,operations,repository_profiles,private_copy;assert operations.program()'
                run([sys.executable,'-c',code,str(target/'plugin/scripts')],env)
            else:
                code='import sys,importlib.util;from pathlib import Path;p=Path(sys.argv[1]);s=importlib.util.spec_from_file_location("shipped_filter",p);m=importlib.util.module_from_spec(s);s.loader.exec_module(m);assert m.repository_profiles({"repository_profiles":{}})["valid"]'
                run([sys.executable,'-c',code,str(target/'deploy/roles/lima/filter_plugins/lima_transition.py')],env)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--go', required=True, help='Absolute existing Go compiler path')
    parser.add_argument('--output', required=True, type=pathlib.Path)
    parser.add_argument('--baseline',required=True,help='Hex source revision preceding resume work')
    args = parser.parse_args()
    compiler = pathlib.Path(args.go)
    if not compiler.is_absolute() or not compiler.is_file():
        parser.error('--go must identify an existing absolute compiler path')
    env = {k: v for k, v in os.environ.items() if not k.startswith('NOMAD_LIMA_')}
    env['GOTOOLCHAIN'] = 'local'
    env['GOWORK']='off'
    env['GOFLAGS']='-mod=readonly'
    env.pop('GOOS', None)
    env.pop('GOARCH', None)
    env.pop('CGO_ENABLED', None)
    if run(['git', 'status', '--porcelain'], env).strip():
        parser.error('Commit the exact reviewed source first; worktree must be clean')
    revision = run(['git', 'rev-parse', 'HEAD'], env).decode().strip()
    if not re.fullmatch('[a-f0-9]{7,40}',args.baseline):parser.error('--baseline must be a hex source revision')
    baseline=run(['git','rev-parse','--verify',args.baseline+'^{commit}'],env).decode().strip()
    run(['git','merge-base','--is-ancestor',baseline,revision],env)
    output = args.output.resolve()
    if output.exists() and any(output.iterdir()):
        parser.error('Use a new empty output directory; previous candidates are immutable')
    output.mkdir(parents=True, exist_ok=True)
    verification = []
    commands = [
        [str(compiler), 'test', './...'],
        [str(compiler), 'test', '-race', './internal/driver', '-run',
         'TestResume|TestConcurrentResume|TestOldExec|TestIntegrationProfile|TestRepository|TestPrivateCopy', '-count=1'],
        [sys.executable, '-m', 'unittest', 'discover', '-s', 'plugin/tests'],
        [sys.executable, '-m', 'unittest', 'discover', '-s', 'tools'],
        [sys.executable, '-m', 'unittest', 'discover', '-s', 'internal/lima', '-p', 'test_private_copy_guest.py'],
        [sys.executable, '-m', 'unittest', 'discover', '-s', 'deploy/tests'],
    ]
    with (output / 'verification.log').open('wb') as log:
        for command in commands:
            print('Verify: ' + ' '.join(command[1:]), flush=True)
            run(command, env, log)
            verification.append({'command': command[1:], 'result': 'passed'})
        for system, arch in TARGETS:
            print('Build: ' + system + '/' + arch, flush=True)
            buildenv = dict(env, GOOS=system, GOARCH=arch, CGO_ENABLED='0')
            name = 'nomad-driver-lima-' + system + '-' + arch
            run([str(compiler), 'build', '-trimpath', '-buildvcs=false', '-ldflags=-s -w',
                 '-o', str(output / name), './cmd/nomad-driver-lima'], buildenv, log)
    archive(output / 'source.tar.gz', run(['git', 'archive', '--format=tar', revision], env))
    tracked=run(['git','ls-files'],env).decode().splitlines()
    caller=package_files(tracked,'caller');deployment=package_files(tracked,'deploy')
    archive(output / 'caller-plugin.tar.gz',run(['git','archive','--format=tar',revision,*caller],env))
    archive(output / 'deploy-bundle.tar.gz',run(['git','archive','--format=tar',revision,*deployment],env))
    verify_packages(output,env)
    verification.append({'command':['verify_extracted_caller_and_deploy_dependencies'],'result':'passed'})
    guest = json.loads((ROOT / 'docs/guest-assets.json').read_text())
    guest_source={name:digest(ROOT/name) for name in guest['files']}
    guest_baseline={name:hashlib.sha256(run(['git','show',baseline+':'+name],env)).hexdigest() for name in guest['files']}
    guest_matches=guest_source==guest['files']
    artifacts = [{'path': p.name, 'sha256': digest(p), 'bytes': p.stat().st_size}
                 for p in sorted(output.iterdir()) if p.is_file()]
    manifest = {
        'schema_version': 1, 'candidate': 'retained-resume', 'caller_version':json.loads((ROOT/'plugin/.claude-plugin/plugin.json').read_text())['version'],
        'source_revision': revision, 'baseline_revision':baseline, 'source_branch':run(['git','branch','--show-current'],env).decode().strip(),
        'go_version': run([str(compiler), 'version'], env).decode().strip(),
        'go_compiler_sha256': digest(compiler), 'go_sum_sha256': digest(ROOT / 'go.sum'),
        'targets': [system + '/' + arch for system, arch in TARGETS],
        'build_flags': ['CGO_ENABLED=0','GOWORK=off','GOFLAGS=-mod=readonly', '-trimpath', '-buildvcs=false', '-ldflags=-s -w'],
        'artifacts': artifacts, 'offline_verification': verification,
        'declared_guest_release_sha256': guest['release_sha256'],
        'guest_source_sha256':guest_source,'declared_guest_file_sha256':guest['files'],
        'guest_assets_changed_by_resume':guest_source!=guest_baseline,
        'guest_assets_match_declared_release':guest_matches,
        'caller_package_excludes_baked_guest_assets':True,'caller_repository_helpers':sorted(CALLER_HELPERS),'deploy_schema_dependencies':sorted(DEPLOY_HELPERS),'extracted_dependency_imports_verified':True,
        'release_gates':(['declared_guest_manifest_mismatch_requires_installed_guest_compatibility_check'] if not guest_matches else [])+['active_user_vm_check','explicit_rollout_approval','live_acceptance'],
        'independent_review': 'docs/retained-resume-rollout.md',
        'live_acceptance': 'not_run', 'production_ready': False,
        'rollout_authorized': False, 'active_user_vm_checked': False,
        'rollout_plan': 'docs/retained-resume-rollout.md',
    }
    path = output / 'manifest.json'
    path.write_text(json.dumps(manifest, sort_keys=True, indent=2) + '\n')
    print('Manifest: ' + str(path), flush=True)
    print('Manifest SHA256: ' + digest(path), flush=True)


if __name__ == '__main__':
    main()
