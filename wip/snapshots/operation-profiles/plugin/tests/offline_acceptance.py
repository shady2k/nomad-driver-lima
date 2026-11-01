#!/usr/bin/env python3
"""Opt-in real acpx + mock ACP, mocked Nomad transport. No model or new VM."""
import os,pathlib,subprocess,tempfile,json,uuid,time,sys
ROOT=pathlib.Path(__file__).resolve().parents[1]
IMAGE=os.environ['LIMA_ADAPTER_FIXTURE_IMAGE']
def docker(*args,**kw):return subprocess.run(['docker',*args],capture_output=True,text=True,**kw)
class Fixture:
 def __init__(self):
  self.tmp=tempfile.TemporaryDirectory();self.root=pathlib.Path(self.tmp.name);self.cid=None
 def __enter__(self):
  p=docker('run','-d','--network','none','--read-only','--tmpfs','/tmp:rw,nosuid,nodev,size=256m','--cpus=1','--memory=1g','--pids-limit=256','-e','HOME=/tmp/plugin-home','--mount','type=bind,src='+str(ROOT)+',dst=/plugin,readonly','--entrypoint','/bin/sh',IMAGE,'-c','mkdir -m700 /tmp/plugin-home /tmp/work; exec sleep 600',timeout=20);assert p.returncode==0,p.stderr;self.cid=p.stdout.strip()
  self.s=str(uuid.uuid4());self.owner=str(uuid.uuid4());self.alloc=str(uuid.uuid4());self.job='fixture-'+self.owner
  self.env={**os.environ,'FIXTURE_CONTAINER':self.cid,'FIXTURE_ALLOCATION':self.alloc,'FIXTURE_JOB':self.job,'FIXTURE_OWNER':self.owner,'FIXTURE_STOP_TRACE':str(self.root/'stops')}
  cfg={'version':1,'namespace':'default','allocation':self.alloc,'job':self.job,'task':'worker','owner_id':self.owner,'nomad_bin':str(ROOT/'tests/mock_nomad.py'),'receipt_root':str(self.root/'receipts'),'guest_node':'/usr/local/bin/node','guest_adapter':'/plugin/guest/adapter.mjs','guest':{'protocol_version':1,'state_dir':'/tmp/plugin-state','workspace':'/tmp/work','node':'/usr/local/bin/node','launcher':'/plugin/guest/launcher.mjs','acpx_bin':'/usr/local/bin/acpx','acpx_version':'0.21.0','runtime_module':'/opt/worker/acpx/node_modules/acpx/dist/runtime.js','agent_argv':['/usr/bin/python3','/plugin/tests/fake_agent.py'],'max_artifact_bytes':1048576,'runtime_contract':{'agent':'mock ACP only','model':'synthetic','output_token_budget':0}}}
  self.config=self.root/'config.json';self.config.write_text(json.dumps(cfg));self.base=[sys.executable,str(ROOT/'scripts/workerctl.py'),'--config',str(self.config)];self.cmd('create');return self
 def cmd(self,cmd,r=None,extra=(),ok=True):
  args=[*self.base,cmd,'--session',self.s]
  if r:args+=['--request',r]
  p=subprocess.run([*args,*extra],env=self.env,capture_output=True,text=True,timeout=35)
  if ok:assert p.returncode==0,(cmd,p.stdout,p.stderr)
  else:assert p.returncode!=0,(cmd,'unexpected success')
  return [json.loads(x) for x in p.stdout.splitlines() if x.strip()]
 def submit(self,r):
  p=self.root/'prompt';p.write_text('synthetic fixture; no model call')
  return self.cmd('submit',r,['--prompt-file',str(p)])
 def watch(self,r):return self.cmd('watch',r)
 def methods(self):
  p=docker('exec',self.cid,'cat','/tmp/methods.jsonl',timeout=10);assert p.returncode==0,p.stderr;return [json.loads(x)['method'] for x in p.stdout.splitlines()]
 def __exit__(self,*a):
  if self.cid:docker('rm','-f',self.cid,timeout=15)
  self.tmp.cleanup()
summary={'real_acpx':'0.21.0','agent':'mock ACP with Prime load/resume=false capabilities','nomad_transport':'mock alloc exec/status/stop via own network-none container','new_vm_count':0,'model_calls':0}
with Fixture() as f:
 r=str(uuid.uuid4());f.submit(r)
 watcher=subprocess.Popen([*f.base,'watch','--session',f.s,'--request',r],env=f.env,stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True)
 first=json.loads(watcher.stdout.readline());assert first['event']['cursor'];watcher.terminate();watcher.wait(timeout=10)
 events=f.watch(r);assert events[-1]['event']['type']=='turn_result';assert events[-1]['event']['result']['status']=='completed'
 assert f.methods().count('session/prompt')==1
 f.submit(r);assert f.methods().count('session/prompt')==1
 f.cmd('finish',r,ok=False);assert not (f.root/'stops').exists()
 f.cmd('collect',r,['result.txt','report.json'])
 # A second turn uses the public strict shared runtime on the same live owner.
 second=str(uuid.uuid4());f.submit(second);f.watch(second);assert f.methods().count('session/prompt')==2
 m=f.cmd('collect',second,['result.txt','report.json'])[0];assert m['actual_backend']=='coordinator-local'
 f.cmd('collect',second,['../outside'],ok=False)
 f.cmd('finish',second,['--retain-for','debug']);m2=f.cmd('result',second)[0];assert m2==m
 assert (f.root/'stops').read_text().splitlines()==['stop']
 summary.update(submit_watch_disconnect_resume=True,repeat_submit_no_resend=True,strict_second_turn=True,prompt_count_success_case=2,manifest_before_finish=True,result_sha_after_stop=True,path_escape_rejected=True)
print(json.dumps({'phase':'success_case_passed','submit_watch_reconnect_manifest_finish':True}),flush=True)
with Fixture() as f:
 r=str(uuid.uuid4());f.submit(r);status=f.cmd('status')[0];pid=status['owner']['pid'];assert pid>1
 p=docker('exec',f.cid,'python3','-c',f'import os; assert b"__queue-owner" in open("/proc/{pid}/cmdline","rb").read(); os.kill({pid},9)',timeout=10);assert p.returncode==0,p.stderr
 f.cmd('watch',r,ok=False);before=f.methods();new=str(uuid.uuid4());p=f.root/'prompt';p.write_text('must not dispatch after owner death');f.cmd('submit',new,['--prompt-file',str(p)],ok=False)
 p=docker('exec',f.cid,'node','/plugin/guest/launcher.mjs','/tmp/plugin-state/'+f.s,timeout=10);assert p.returncode==78
 assert f.methods()==before and before.count('session/prompt')==1
 assert (f.root/'receipts'/f.s/(r+'.json')).exists()
 summary.update(owner_death_explicit_error=True,owner_death_no_resubmit=True,restart_gate_rejects_before_agent=True,receipt_preserved=True)
print(json.dumps(summary,indent=2))
