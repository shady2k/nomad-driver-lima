"""Opt-in tmpfs delivery fixture: preexisting local image, no network/pull/host mounts."""
import pathlib,json,subprocess,sys,os
source=(pathlib.Path(__file__).resolve().parents[2]/'internal/lima/vault_guest.py').read_text()
fixture='''import json,subprocess,os,stat,uuid
source=SOURCE
sid=str(uuid.uuid4());p={'session':sid,'owner':'a'*32,'action':'sync','token':'synthetic-one'}
def run(v):return subprocess.run(['python3','-c',source],input=json.dumps(v),capture_output=True,text=True)
r=run(p);assert r.returncode==0 and json.loads(r.stdout)=={'ok':True}
d='/dev/shm/agent-worker-'+str(os.getuid())+'/'+sid;f=d+'/vault_token'
assert open(f).read()=='synthetic-one' and stat.S_IMODE(os.stat(f).st_mode)==0o600
p['token']='synthetic-two';assert run(p).returncode==0 and open(f).read()=='synthetic-two'
p['owner']='b'*32;assert run(p).returncode!=0 and open(f).read()=='synthetic-two'
p['owner']='a'*32;p['action']='clear';p['token']='';assert run(p).returncode==0 and not os.path.exists(f)
assert run(p).returncode==0
print(json.dumps({'tmpfs_delivery':True,'atomic_replacement':True,'ownership_rejection':True,'idempotent_clear':True,'model_calls':0,'vm_count':0}))
'''.replace('SOURCE',repr(source))
p=subprocess.run(['docker','run','--rm','--pull=never','--network=none','--read-only','--cap-drop=ALL','--memory=128m','--cpus=0.5','--pids-limit=32','--entrypoint=python3','-i',os.environ['LIMA_ADAPTER_FIXTURE_IMAGE'],'-'],input=fixture,text=True,capture_output=True,timeout=25)
print(p.stdout);print(p.stderr if p.returncode else '',file=sys.stderr);raise SystemExit(p.returncode)
