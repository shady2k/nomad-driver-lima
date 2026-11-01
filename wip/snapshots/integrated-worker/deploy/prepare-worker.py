#!/usr/bin/env python3
'''Prepare one secret-free x86_64 Lima worker template/job/config; never apply.
Uses only Python stdlib. Generated template is JSON (valid YAML). No host mounts.
'''
import argparse
import base64
import hashlib
import json
import pathlib
import re
import uuid

ROOT = pathlib.Path(__file__).resolve().parents[1]

def sha(data):
    return hashlib.sha256(data).hexdigest()

def build(args):
    for name in ('node_id', 'owner_id'):
        value = str(uuid.UUID(getattr(args, name)))
        if value != getattr(args, name):
            raise ValueError('UUID must be canonical: ' + name)
    if not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.-]{0,62}', args.job_id):
        raise ValueError('invalid job ID')
    if min(args.cpu_mhz, args.memory_mib, args.disk_gib, args.output_token_budget) <= 0:
        raise ValueError('resource values and declared budget must be positive')
    if not args.provider or not args.model:
        raise ValueError('external provider and model are required')
    out = pathlib.Path(args.output).resolve()
    out.mkdir(mode=0o700, parents=True, exist_ok=False)
    entries = {}
    for filename in ('provision.sh', 'package.json', 'package-lock.json', 'uv.lock', 'kernel.lock'):
        src = ROOT / 'deploy/guest/runtime' / filename
        dest = '/opt/worker/acpx/' + filename if filename.startswith('package') else '/opt/worker/assets/' + filename
        entries[dest] = [base64.b64encode(src.read_bytes()).decode(), 0o644]
    for filename in ('adapter.mjs', 'launcher.mjs'):
        entries['/opt/nomad-workers/' + filename] = [base64.b64encode((ROOT/'plugin/guest'/filename).read_bytes()).decode(), 0o644]
    entries['/opt/nomad-workers/start-worker.sh'] = [base64.b64encode((ROOT/'deploy/guest/start-worker.sh').read_bytes()).decode(), 0o755]
    payload = base64.b64encode(json.dumps(entries, sort_keys=True).encode()).decode()
    # Only known explicit assets are embedded; no caller-provided files or secrets.
    install = '''#!/bin/sh
set -eu
umask 022
export PATH=/usr/local/bin:/usr/bin:/bin
export DEBIAN_FRONTEND=noninteractive
. /etc/os-release
test "$ID:$VERSION_ID:$(uname -m)" = ubuntu:24.04:x86_64
if [ ! -f /opt/nomad-workers/runtime.ready ]; then
  # A partial install is retained for diagnosis, never silently replayed.
  test ! -d /opt/worker/prime || { echo 'Partial runtime install; operator reconciliation required.' >&2; exit 70; }
  install -d -m 0755 /opt/worker /opt/worker/acpx /opt/worker/assets /opt/nomad-workers /workspace /var/lib/nomad-workers
  apt-get update
  apt-get install -y --no-install-recommends ca-certificates curl git python3 python3-venv unzip xz-utils bash procps ripgrep
  python3 - <<'ASSETS'
import base64,json,os,pathlib
for name,(content,mode) in json.loads(base64.b64decode('__PAYLOAD__')).items():
    p=pathlib.Path(name);p.parent.mkdir(parents=True,exist_ok=True)
    p.write_bytes(base64.b64decode(content));p.chmod(mode)
ASSETS
  /bin/sh /opt/worker/assets/provision.sh
  /usr/local/bin/uv pip install --python /opt/worker/prime/prime-agent-runtime/.venv/bin/python --require-hashes -r /opt/worker/assets/kernel.lock
  /usr/local/bin/uv pip install --offline --no-build-isolation --no-deps --python /opt/worker/prime/prime-agent-runtime/.venv/bin/python /opt/worker/prime/skills/agent-message
fi
test "$(node --version)" = v24.22.1
node -e 'if(typeof process.execve!=="function")process.exit(1)'
test "$(acpx --version)" = 0.21.0
prime-agent --version | grep -F 0.10.1
node --input-type=module -e 'const r=await import("/opt/worker/acpx/node_modules/acpx/dist/runtime.js");if(typeof r.createSharedAcpRuntime!=="function")process.exit(1)'
install -d -m 0700 -o '{{.User}}' -g '{{.User}}' /var/lib/nomad-workers/provider /var/tmp/nomad-workers /workspace/repo
/opt/worker/prime/prime-agent-runtime/.venv/bin/python -I -c 'import rlm,agent_message,httpx,dill,pytest'
touch /opt/nomad-workers/runtime.ready
chmod 0644 /opt/nomad-workers/runtime.ready
'''.replace('__PAYLOAD__', payload)
    # This is the already-reviewed pinned Ubuntu image and Lima plain policy.
    template = {
        'minimumLimaVersion': '2.1.0', 'vmType': 'qemu', 'arch': 'x86_64',
        'images': [{'location': 'https://cloud-images.ubuntu.com/minimal/releases/noble/release-20261001/ubuntu-24.04-minimal-cloudimg-amd64.img',
                    'arch': 'x86_64', 'digest': 'sha256:a8eb6570f87a941f3de2e000c046b9367f8ab1423756a94830d454fae8ad80ff'}],
        'mounts': [], 'plain': True, 'containerd': {'system': False, 'user': False},
        'ssh': {'forwardAgent': False, 'loadDotSSHPubKeys': False},
        'propagateProxyEnv': False,
        'provision': [{'mode': 'system', 'script': install}],
    }
    (out/'worker-template.yaml').write_text(json.dumps(template, indent=2)+'\n')
    job = (ROOT/'deploy/worker.nomad.hcl').read_text().replace('"__JOB_ID__"', json.dumps(args.job_id))
    (out/'worker.nomad.hcl').write_text(job)
    variables = {k:getattr(args,k) for k in ('namespace','datacenter','node_id','owner_id','cpu_mhz','memory_mib','disk_gib')}
    (out/'worker.vars.hcl').write_text(''.join(k+' = '+json.dumps(v)+'\n' for k,v in variables.items()))
    config = json.loads((ROOT/'plugin/config.example.json').read_text())
    config.update(namespace=args.namespace, job=args.job_id, task='worker', owner_id=args.owner_id,
                  allocation='REPLACE_AFTER_APPROVED_JOB_RUN', receipt_root=args.receipt_root)
    guest = config['guest']
    guest.update(state_dir='/var/tmp/nomad-workers', workspace='/workspace/repo',
                 agent_argv=['/usr/bin/env', 'PI_OFFLINE=1',
                    'PRIME_AGENT_CODING_AGENT_DIR=/var/lib/nomad-workers/provider',
                    'PRIME_AGENT_DAEMON_SOCKET=/var/tmp/nomad-workers/prime.sock',
                    'PRIME_AGENT_KERNEL_PYTHON=/opt/worker/prime/prime-agent-runtime/.venv/bin/python',
                    '/usr/local/bin/prime-agent', '--mode','acp','--offline',
                    '--daemon-socket','/var/tmp/nomad-workers/prime.sock',
                    '--provider',args.provider,'--model',args.model],
                 model=args.model,
                 runtime_contract={'prime_version':'0.10.1', 'provider':args.provider,
                                   'model':args.model,'output_token_budget':args.output_token_budget,
                                   'effective_provider_config_verified':False})
    (out/'coordinator.json').write_text(json.dumps(config, indent=2)+'\n')
    manifest = {'versions': {'node':'24.22.1','acpx':'0.21.0','prime':'0.10.1','uv':'0.13.5'},
                'guest_assets': {p:sha(base64.b64decode(v[0])) for p,v in entries.items()},
                'files': {p.name:sha(p.read_bytes()) for p in sorted(out.iterdir())},
                'model_calls':0, 'applied':False}
    (out/'manifest.json').write_text(json.dumps(manifest,indent=2)+'\n')
    for p in out.iterdir(): p.chmod(0o600)
    return out

def main():
    p=argparse.ArgumentParser(description=__doc__)
    for k in ('output','node-id','owner-id','namespace','datacenter','provider','model','receipt-root'):
        p.add_argument('--'+k,required=True)
    p.add_argument('--job-id',default='lima-worker-canary')
    p.add_argument('--cpu-mhz',required=True,type=int)
    p.add_argument('--memory-mib',default=4096,type=int)
    p.add_argument('--disk-gib',default=20,type=int)
    p.add_argument('--output-token-budget',required=True,type=int)
    a=p.parse_args()
    if not pathlib.Path(a.receipt_root).is_absolute(): p.error('receipt-root must be absolute')
    print(build(a))

if __name__=='__main__': main()
