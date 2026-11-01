#!/usr/bin/env python3
"""One isolated native Nomad/catalog/plugin cycle with a synthetic ACP agent.
No provider credentials, model request, production endpoint or disk purge.
"""
import argparse,json,os,pathlib,subprocess,time,uuid,urllib.request
p=argparse.ArgumentParser();p.add_argument('--root',required=True);p.add_argument('--nomad',required=True);p.add_argument('--port',type=int,required=True);a=p.parse_args()
root=pathlib.Path(a.root);assert str(root).startswith('/var/lib/nomad-lima/catalog-accept-') and a.port>1024
identity=root/'canary-identity.json'
if identity.exists():ids=json.loads(identity.read_text())
else:
 ids={k:str(uuid.uuid4()) for k in ('owner','agent','session','request')};identity.write_text(json.dumps(ids));identity.chmod(0o600)
env={'PATH':'/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin','HOME':'/root','NOMAD_ADDR':'http://127.0.0.1:'+str(a.port),'PYTHONDONTWRITEBYTECODE':'1'}
def log(stage,**extra):print(json.dumps({'stage':stage,'timestamp':time.time(),**extra}),flush=True)
def api(path):
 with urllib.request.urlopen(env['NOMAD_ADDR']+path,timeout=5) as f:return json.load(f)
catalog=root/'catalog.json';entry=json.loads(catalog.read_text())['templates'][0]
config=root/'canary-config.json'
if not config.exists():
 d={'version':1,'namespace':'default','datacenters':['dc1'],'task':'worker','owner_id':ids['owner'],'agent_instance_id':ids['agent'],'nomad_bin':a.nomad,'receipt_root':str(root/'receipts'),'catalog_file':str(catalog),'catalog_sha256':__import__('hashlib').sha256(catalog.read_bytes()).hexdigest(),'memory_overhead_mib':128,'request_limits':{'vcpus':2,'memory_mib':4096,'disk_gib':20},'guest_bootstrap_command':['/usr/bin/python3','/opt/nomad-workers/task-main.py','--state-dir','/var/tmp/nomad-workers','--session','{session_id}'],'guest_node':'/usr/local/bin/node','guest_adapter':'/opt/nomad-workers/adapter.mjs','guest':{'protocol_version':1,'state_dir':'/var/tmp/nomad-workers','workspace':'/workspace/repo','node':'/usr/local/bin/node','launcher':'/opt/nomad-workers/launcher.mjs','acpx_bin':'/usr/local/bin/acpx','acpx_version':'0.21.0','runtime_module':'/opt/worker/acpx/node_modules/acpx/dist/runtime.js','agent_argv':['/usr/bin/python3','/var/tmp/catalog-fake-agent.py'],'max_artifact_bytes':1048576,'runtime_contract':{'agent':'synthetic ACP fixture only','model':'none','output_token_budget':0}}}
 config.write_text(json.dumps(d));config.chmod(0o600)
base=['/usr/bin/python3',str(root/'scripts/workerctl.py'),'--config',str(config)]
def cli(command,extra=(),target=None,timeout=120):
 argv=[*base,command,'--session',ids['session'],*extra]
 if target:argv=['/usr/bin/python3',str(root/'scripts/workerctl.py'),'--config',str(target),command,'--session',ids['session'],*extra]
 r=subprocess.run(argv,env=env,capture_output=True,text=True,timeout=timeout)
 (root/('canary-'+command+'.log')).write_text(r.stdout+r.stderr)
 if r.returncode:raise RuntimeError(command+' failed; inspect canary-'+command+'.log; no prompt replay')
 return [json.loads(s) for s in r.stdout.splitlines() if s.strip()][-1]
log('catalog_request_started',session=ids['session'])
cli('request',['--template','agent-base','--template-version',entry['version'],'--architecture','amd64','--vcpus','2','--memory-mib','2048','--disk-gib','20','--purpose','catalog-canary'])
deadline=time.monotonic()+240
while True:
 status=cli('request-status');allocation=status.get('allocation')
 if not allocation and any(x.get('status')=='failed' for x in status.get('allocations',[])):raise RuntimeError('known failed admission; preserve intent, explicit new session required')
 if allocation:
  actual=api('/v1/allocation/'+allocation)
  if actual.get('ClientStatus')=='failed':raise RuntimeError('allocation failed; inspect dev agent logs')
  task=actual.get('TaskStates',{}).get('worker',{})
  if actual.get('ClientStatus')=='running' and task.get('State')=='running':break
 if time.monotonic()>deadline:raise RuntimeError('allocation boot pending; resume status only')
 time.sleep(2)
target=pathlib.Path(status['target_config']);log('native_allocation_running',allocation=allocation)
# Test-only fixture is injected through official alloc exec stdin, never into base.
fixture=(root/'fake-agent.py').read_text().replace('/tmp/work/','/workspace/repo/')
r=subprocess.run([a.nomad,'alloc','exec','-namespace=default','-task=worker','-i=true','-t=false',allocation,'/bin/sh','-c','umask 077; cat > /var/tmp/catalog-fake-agent.py'],input=fixture,text=True,env=env,capture_output=True,timeout=30)
assert r.returncode==0,'fixture staging failed'
cli('create',target=target);log('acpx_fixture_session_ready')
prompt=root/'synthetic-prompt.txt';prompt.write_text('Synthetic ACP acceptance only; no model. Produce fixture result files.')
cli('submit',['--request',ids['request'],'--prompt-file',str(prompt)],target=target)
cli('watch',['--request',ids['request']],target=target);log('synthetic_turn_terminal',model_calls=0)
manifest=cli('collect',['--request',ids['request'],'result.txt','report.json'],target=target)
assert manifest['actual_backend']=='coordinator-local' and len(manifest['files'])>=2
cli('finish',['--request',ids['request']],target=target);readback=cli('result',['--request',ids['request']],target=target)
assert readback==manifest
summary={'session':ids['session'],'request':ids['request'],'allocation':allocation,'job':'worker-'+ids['session'],'real_nomad_transport':True,'real_acpx':'0.21.0','agent':'synthetic ACP fixture','model_calls':0,'storage_backend':'coordinator-local','sha_result_after_stop':True,'disk_purge_requested':False,'manifest':manifest}
(root/'canary-summary.json').write_text(json.dumps(summary,indent=2)+'\n');log('isolated_catalog_cycle_pass',allocation=allocation,result_after_stop=True)
