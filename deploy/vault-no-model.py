#!/usr/bin/env python3
"""Operator-host acceptance: real native Vault/Prime initialization, zero model turns.
A zero-turn health receipt uses the guest's supported prepare_finish/close/ack
protocol; it never invents a model response or calls Adapter.submit.
"""
import argparse,hashlib,json,os,pathlib,subprocess,sys,time
p=argparse.ArgumentParser();p.add_argument('--root',required=True);p.add_argument('--resume-ready',action='store_true');a=p.parse_args();root=pathlib.Path(a.root);sys.path.insert(0,str(root))
import workerctl,diagnostics
s=json.loads((root/'identity.json').read_text())['session'];target=root/'receipts'/s/'target-config.json';adapter=workerctl.Adapter(target)
operation=adapter.local(s)/'no-model-operation.json'
if operation.exists():
 state=json.loads(operation.read_text());bound=adapter.load(s);current=adapter.target()
 if not a.resume_ready or state.get('phase')!='acp_ready' or bound.get('phase')!='ready' or adapter.attempt(current)!=bound['attempt'] or current.get('ClientStatus')!='running':raise SystemExit('EXISTING_OPERATION_NOT_CONFIRMED_READY_NO_REPLAY')
 print(json.dumps({'stage':'resume_health_after_confirmed_acp_ready','session':s,'create_replayed':False}),flush=True)
else:
 if a.resume_ready:raise SystemExit('RESUME_REQUIRES_EXISTING_READY_OPERATION')
 workerctl.atomic(operation,{'phase':'intent','session':s,'model_prompts_submitted':0})
 print(json.dumps({'stage':'acp_initialize_started','session':s}),flush=True)
 adapter.create(s)
 workerctl.atomic(operation,{'phase':'acp_ready','session':s,'model_prompts_submitted':0})
 print(json.dumps({'stage':'acp_ready','session':s}),flush=True)
probe=r'''
import json,pathlib,os,stat,urllib.request,urllib.error,sys,time
s=sys.argv[1];directory=pathlib.Path('/var/tmp/nomad-workers')/s;ready=json.loads((directory/'runtime-ready.json').read_text());volatile=pathlib.Path('/dev/shm/agent-worker-'+str(os.getuid()))/s
assert ready['phase']=='ready'
assert json.loads((directory/'prime-config/auth.json').read_text())=={},'PRIME_AUTH_STORE_NOT_EMPTY'
def names(pid):return {x.partition(b'=')[0].decode() for x in pathlib.Path('/proc/'+str(pid)+'/environ').read_bytes().split(bytes([0])) if x}
bao=names(ready['processes'][0]['pid']);prime=names(ready['processes'][1]['pid']);assert 'LLM_API_KEY' in prime and not {'NOMAD_TOKEN','VAULT_TOKEN','BAO_TOKEN'}&prime and 'LLM_API_KEY' not in bao
ss=pathlib.Path(ready['socket']).lstat();assert stat.S_ISSOCK(ss.st_mode) and (ss.st_dev,ss.st_ino)==(ready['socket_dev'],ready['socket_ino'])
acp=[]
for entry in pathlib.Path('/proc').glob('[0-9]*'):
 try:
  if entry.stat().st_uid!=os.getuid():continue
  args=(entry/'cmdline').read_bytes().split(bytes([0]))
  if b'--mode' in args and b'acp' in args and ready['socket'].encode() in args:
   keys=names(int(entry.name));assert not {'LLM_API_KEY','NOMAD_TOKEN','VAULT_TOKEN','BAO_TOKEN'}&keys;acp.append(int(entry.name))
 except (FileNotFoundError,ProcessLookupError):continue
# sessions new is a one-shot CLI; an exited ACP process is not a failure.
token_file=volatile/'vault_token';ts=token_file.lstat();assert stat.S_IMODE(ts.st_mode)==0o600 and ts.st_uid==os.getuid()
mounts=pathlib.Path('/proc/self/mountinfo').read_text().splitlines();assert any(' /dev/shm ' in x and ' - tmpfs ' in x for x in mounts)
token=token_file.read_text().strip();address=json.loads((directory/'session.json').read_text())['config']['prime_vault']['address']
def call(path,method='GET',value=None):
 req=urllib.request.Request(address+'/v1/'+path,headers={'X-Vault-Token':token,'Content-Type':'application/json'},method=method,data=None if value is None else json.dumps(value).encode())
 try:
  with urllib.request.urlopen(req,timeout=10) as r:return json.load(r)
 except urllib.error.HTTPError as e:raise RuntimeError('TASK_TOKEN_API_HTTP_'+str(e.code)) from None
lookup=call('auth/token/lookup-self')['data'];assert lookup['policies']==['agent-worker-llm-read'] and lookup['renewable'] and lookup['ttl']>0 and lookup['path'].startswith('auth/nomad-workload/login')
renew=call('auth/token/renew-self','POST',{'increment':'30m'})['auth'];assert renew['renewable'] and renew['lease_duration']>0
result={'schema_version':1,'session':s,'official_prime_daemon_ready':True,'llm_environment_present_in_prime':True,'llm_environment_absent_from_bao_parent':True,'server_tokens_absent_from_prime':True,'acp_processes_observed':len(acp),'acp_environment_checked':bool(acp),'private_socket_identity_verified':True,'task_token_tmpfs':True,'prime_auth_store_empty':True,'nomad_workload_login':True,'narrow_policy_exact':True,'task_token_self_renew_ack':True,'remaining_ttl_seconds':renew['lease_duration'],'model_prompts_submitted':0,'automatic_replacement_live_tested':False,'time':time.time()}
del token,lookup,renew
b=json.dumps(result,sort_keys=True,separators=(',',':')).encode()+bytes([10]);path=pathlib.Path('/workspace/repo/vault-no-model-health.json');fd=os.open(path,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
with os.fdopen(fd,'wb') as f:f.write(b);f.flush();os.fsync(f.fileno())
sys.stdout.buffer.write(b)
'''
cmd=[adapter.c['nomad_bin'],'alloc','exec','-namespace='+adapter.c['namespace'],'-task='+adapter.c['task'],'-i=false','-t=false',adapter.c['allocation'],'/usr/bin/python3','-c',probe,s]
r=subprocess.run(cmd,capture_output=True,timeout=45)
if r.returncode:raise SystemExit('NO_MODEL_HEALTH_PROBE_FAILED_RETAIN_SAME_OPERATION')
health=json.loads(r.stdout);assert health['session']==s and health['model_prompts_submitted']==0
out=adapter.local(s)/'no-model-export';out.mkdir(mode=0o700);f=out/'health.json';fd=os.open(f,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
with os.fdopen(fd,'wb') as q:q.write(r.stdout);q.flush();os.fsync(q.fileno())
files=[{'path':'vault-no-model-health.json','size':len(r.stdout),'sha256':hashlib.sha256(r.stdout).hexdigest(),'local_file':'health.json'}]
files.extend(diagnostics.capture(adapter,s,out))
for item in files:
 data=(out/item['local_file']).read_bytes();assert len(data)==item['size'] and hashlib.sha256(data).hexdigest()==item['sha256']
manifest={'schema_version':1,'kind':'zero_model_turn_native_health','session':s,'allocation':adapter.c['allocation'],'actual_backend':'coordinator-local','durability':'local_disk_not_remote_backup','files':files,'model_prompts_submitted':0};workerctl.atomic(out/'manifest.json',manifest);sha=hashlib.sha256(workerctl.canonical(manifest)).hexdigest()
workerctl.atomic(operation,{'phase':'receipt_saved','session':s,'manifest_sha256':sha,'model_prompts_submitted':0});print(json.dumps({'stage':'health_and_native_logs_verified','manifest_sha256':sha}),flush=True)
list(adapter.remote(s,'prepare_finish',files=[x for x in files if x.get('source')!='native_diagnostic'],manifest_sha256=sha));adapter.event(s,'receipt_verified');list(adapter.remote(s,'close'));list(adapter.remote(s,'finish_ack',manifest_sha256=sha))
v=adapter.load(s);v['phase']='finish_requested';workerctl.atomic(adapter.local(s)/'session.json',v)
r=adapter.nomad(['job','stop','-yes','-detach','-namespace='+adapter.c['namespace'],adapter.c['job']],text=True)
if r.returncode:raise SystemExit('STOP_OUTCOME_UNKNOWN_INSPECT_SAME_TARGET')
finished=adapter._wait_finished(s,v);workerctl.atomic(operation,{'phase':'finished','session':s,'manifest_sha256':sha,'model_prompts_submitted':0})
print(json.dumps({'stage':'no_model_native_vault_acceptance_pass','session':s,'allocation':adapter.c['allocation'],'manifest_sha256':sha,'files':len(files),'task_token_revoked':True,'vm_stopped':True,'disk_purge_requested':False,'automatic_replacement_live_tested':False,'model_prompts_submitted':0}),flush=True)
