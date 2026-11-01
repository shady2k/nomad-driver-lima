#!/usr/bin/env python3
"""Local staging Lima adapter. No credentials reader, daemon or model retry loop."""
import argparse,base64,contextlib,fcntl,hashlib,json,os,pathlib,subprocess,sys,tempfile,time,uuid

def digest(b):return hashlib.sha256(b).hexdigest()
def canonical(v):return json.dumps(v,sort_keys=True,separators=(',',':')).encode()
def atomic(p,v):
 p=pathlib.Path(p);fd,tmp=tempfile.mkstemp(prefix='.commit-',dir=p.parent)
 try:
  os.fchmod(fd,0o600)
  with os.fdopen(fd,'wb') as f:f.write(canonical(v));f.flush();os.fsync(f.fileno())
  os.replace(tmp,p)
  d=os.open(p.parent,os.O_RDONLY)
  try:os.fsync(d)
  finally:os.close(d)
 finally:
  if os.path.exists(tmp):os.unlink(tmp)
def uid(v):
 if str(uuid.UUID(v))!=v:raise ValueError('canonical UUID required')
 return v
class Adapter:
 def __init__(self,config,operations_config=None):
  self.operations_policy_file=operations_config
  self.c=json.loads(pathlib.Path(config).read_text());self.hash=digest(canonical(self.c))
  if self.c['version']!=1:raise ValueError('unsupported config version')
  uid(self.c['allocation']);uid(self.c['owner_id'])
  self.root=pathlib.Path(self.c['receipt_root']);self.root.mkdir(parents=True,exist_ok=True,mode=0o700)
  if self.root.is_symlink() or self.root.stat().st_mode&0o077:raise ValueError('private receipt_root required')
 def local(self,s):
  p=self.root/uid(s);p.mkdir(exist_ok=True,mode=0o700);return p
 @contextlib.contextmanager
 def lock(self,s):
  with open(self.local(s)/'lock','a') as f:os.chmod(f.name,0o600);fcntl.flock(f,fcntl.LOCK_EX);yield
 def load(self,s):
  v=json.loads((self.local(s)/'session.json').read_text())
  if v['config_hash']!=self.hash:raise ValueError('immutable config changed')
  return v
 def nomad(self,args,**kw):return subprocess.run([self.c.get('nomad_bin','nomad'),*args],capture_output=True,**kw)
 def target(self):
  r=self.nomad(['alloc','status','-json','-namespace='+self.c['namespace'],self.c['allocation']],text=True)
  if r.returncode:raise RuntimeError('NOMAD_ALLOCATION_UNAVAILABLE')
  a=json.loads(r.stdout);j=a.get('Job',{})
  tasks=[t for g in j.get('TaskGroups',[]) for t in g.get('Tasks',[]) if t.get('Name')==self.c['task']]
  if a.get('ID')!=self.c['allocation'] or a.get('Namespace')!=self.c['namespace'] or j.get('ID')!=self.c['job'] or j.get('Meta',{}).get('nomad_workers_owner')!=self.c['owner_id'] or len(tasks)!=1 or tasks[0].get('Driver')!='lima':raise RuntimeError('TARGET_OWNERSHIP_MISMATCH')
  if self.c.get('resolved_template'):
   import placement
   task=tasks[0]['Config'];expected={'catalog_sha256':task['template_catalog_sha256'],'variants':json.loads(task['template_variants_json'])}
   if placement.allocation_attempt(a,self.c['task'])!=self.c.get('placement_attempt'):raise RuntimeError('TARGET_PLATFORM_ATTEMPT_CHANGED')
   if placement.selected(expected,a,self.c['task'])!=self.c['resolved_template']:raise RuntimeError('TARGET_PLATFORM_BINDING_CHANGED')
  return a
 def attempt(self,a):
  t=a.get('TaskStates',{}).get(self.c['task'],{})
  result={'allocation':a['ID'],'job':self.c['job'],'task':self.c['task'],'job_version':a.get('Job',{}).get('Version'),'restarts':t.get('Restarts',0),'started_at':t.get('StartedAt')}
  if self.c.get('resolved_template'):result['resolved_template']=self.c['resolved_template']
  return result
 def remote(self,s,action,**fields):
  allocation=self.target()
  if action in ('create','submit') and (allocation.get('ClientStatus')!='running' or allocation.get('TaskStates',{}).get(self.c['task'],{}).get('State')!='running'):raise RuntimeError('TASK_NOT_RUNNING_NO_PROMPT_DISPATCH')
  current=self.attempt(allocation);f=self.local(s)/'session.json'
  if f.exists():
   expected=json.loads(f.read_text()).get('attempt')
   if expected is not None and current!=expected:raise RuntimeError('TARGET_ATTEMPT_CHANGED_NO_REPLAY')
  payload={'config':self.c['guest'],'session_id':s,'action':action,**fields}
  args=['alloc','exec','-namespace='+self.c['namespace'],'-task='+self.c['task'],'-i=true','-t=false',self.c['allocation'],self.c['guest_node'],self.c['guest_adapter']]
  p=subprocess.Popen([self.c.get('nomad_bin','nomad'),*args],stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,text=True)
  try:
   p.stdin.write(json.dumps(payload));p.stdin.close()
   for line in p.stdout:
    if len(line)>100000000:raise RuntimeError('REMOTE_FRAME_TOO_LARGE')
    e=json.loads(line)
    if e.get('type')=='error':raise RuntimeError(e.get('code','REMOTE_ERROR'))
    yield e
   if p.wait()!=0:raise RuntimeError('TRANSPORT_FAILED_OUTCOME_UNKNOWN')
  finally:
   if p.poll() is None:p.terminate()
 def create(self,s):
  current=self.target();task=current.get('TaskStates',{}).get(self.c['task'],{})
  if current.get('ClientStatus')!='running' or task.get('State')!='running' or not task.get('StartedAt') or task['StartedAt'].startswith('0001-'):raise RuntimeError('TASK_NOT_RUNNING_RETRY_READINESS_NO_SESSION_BOUND')
  attempt=self.attempt(current)
  with self.lock(s):
   binding=self.root/('allocation-'+self.c['allocation']+'.json')
   try:
    fd=os.open(binding,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
    with os.fdopen(fd,'w') as b:json.dump({'session_id':s,'owner_id':self.c['owner_id']},b);b.flush();os.fsync(b.fileno())
   except FileExistsError:
    if json.loads(binding.read_text())!={'session_id':s,'owner_id':self.c['owner_id']}:raise RuntimeError('ALLOCATION_ALREADY_BOUND')
   f=self.local(s)/'session.json'
   if f.exists():return self.load(s)
   v={'session_id':s,'config_hash':self.hash,'phase':'create_intent','actual_backend':'lima/nomad-exec/acpx','attempt':attempt};atomic(f,v)
   import lease
   lease.create(self.local(s),self.c.get('agent_instance_id',self.c['owner_id']),s,attempt)
   try:
    frames=list(self.remote(s,'create'));v.update(phase='ready',record_id=frames[-1]['record_id']);atomic(f,v)
   except Exception:
    v['phase']='create_unknown';atomic(f,v);raise
   return v
 def require_initiator(self,s):
  if not self.c.get('creator_record'):raise RuntimeError('INITIATOR_RECORD_REQUIRED_NO_FORGED_HANDOFF')
  import exchange
  native=exchange.protected(self.c['creator_record'])
  if native.get('ended') or (native.get('agent_instance_id'),native.get('native_session_id'),native.get('generation'))!=(self.c.get('agent_instance_id'),self.c.get('creator_session_id'),self.c.get('creator_generation')):raise RuntimeError('INITIATOR_GENERATION_CHANGED_NO_AUTOMATIC_HANDOFF')
 def exec(self,s,execution,argv,timeout=None,cwd=None):
  import guest_exec
  return guest_exec.execute(self,s,execution,argv,timeout,cwd)
 def select_model(self,s,selection,model):
  import activity,model_catalog,operations
  uid(selection);self.require_initiator(s)
  provider=self.c['guest']['prime_vault']['provider'];mapping=model_catalog.selectors(provider)
  if model not in mapping:raise ValueError('EXACT_SUPPORTED_MODEL_REQUIRED')
  value=mapping[model]['coordinator_acp_value']
  with self.lock(s):
   path=self.local(s)/('exec-model-'+selection+'.json')
   if path.exists():
    previous=json.loads(path.read_text())
    if previous['model_id']!=model or previous['coordinator_value']!=value:raise ValueError('MODEL_SELECTION_ID_CHANGED')
    return previous
   operations.verify_terminal(self,s);activity.pending(self,s)
   live=list(self.remote(s,'status'))[-1]
   if live.get('owner',{}).get('status')!='alive':raise RuntimeError('COORDINATOR_OWNER_NOT_ALIVE_SELECT_AT_REQUEST')
   row={'session_id':s,'execution_id':selection,'phase':'exec_unknown','model_id':model,'coordinator_value':value,'attempt':self.attempt(self.target()),'prompt_replayed':False}
   activity.record(self,s,path,row)
   try:
    result=list(self.remote(s,'select_model',model_id=model,coordinator_value=value))[-1]
    if result.get('type')!='model_selected' or result.get('coordinator_value')!=value:raise RuntimeError('MODEL_SELECTION_ACK_MISMATCH')
    row.update(phase='completed',result=result);atomic(path,row)
   except Exception:
    atomic(path,row);raise RuntimeError('MODEL_SELECTION_OUTCOME_UNKNOWN_NO_REPLAY') from None
   return row
 def submit(self,s,r,prompt):
  uid(r);self.load(s)
  if self.c.get('creator_record'):
   import exchange
   native=exchange.protected(self.c['creator_record'])
   if native.get('ended') or (native.get('agent_instance_id'),native.get('native_session_id'),native.get('generation'))!=(self.c.get('agent_instance_id'),self.c.get('creator_session_id'),self.c.get('creator_generation')):raise RuntimeError('INITIATOR_GENERATION_CHANGED_NO_AUTOMATIC_HANDOFF')
  with self.lock(s):
   f=self.local(s)/(r+'.json');h=digest(prompt.encode())
   if f.exists():
    v=json.loads(f.read_text())
    if v['prompt_sha256']!=h:raise ValueError('REQUEST_CONTENT_CHANGED')
    return v
   if self.load(s).get('phase')!='ready':raise RuntimeError('SESSION_NOT_READY_OR_FINISHING')
   v={'session_id':s,'request_id':r,'phase':'submit_intent','prompt_sha256':h,'cursor':None}
   import activity
   activity.record(self,s,f,v)
   import lease
   state=self.load(s);observed=lease.create(self.local(s),self.c.get('agent_instance_id',self.c['owner_id']),s,state.get('attempt'))
   lease.observe(self.local(s),observed['owner'],s,observed['attempt'],observed['generation'],observed['sequence']+1,r)
   try:
    frame=list(self.remote(s,'submit',request_id=r,prompt=prompt))[-1];v.update(frame);v['phase']='queued';atomic(f,v)
   except Exception:
    v['phase']='submit_unknown';atomic(f,v);raise
   return v
 def watch(self,s,r):
  self.load(s);f=self.local(s)/(uid(r)+'.json');v=json.loads(f.read_text())
  try:
   for frame in self.remote(s,'watch',request_id=r,cursor=v.get('cursor')):
    e=frame['event'];v['cursor']=e['cursor'];v['acpx_request_id']=e['requestId']
    if e['type']=='turn_result':
     if v.get('export_verified') and v.get('terminal')!=e['result']:raise RuntimeError('TERMINAL_METADATA_CHANGED_AFTER_CHECKPOINT')
     v.update(phase='terminal',terminal=e['result'])
    with self.lock(s):atomic(f,v)
    yield frame
  except Exception:
   if v.get('phase')!='terminal':
    v['phase']='watch_unknown'
    with self.lock(s):atomic(f,v)
   raise
 def event(self,s,stage,r=None):
  list(self.remote(s,'lifecycle',stage=stage,request_id=r))
 def export_record(self,s,r,snapshot=None):
  snapshot=uid(snapshot or r);uid(r)
  if snapshot==r:return self.local(s)/(r+'.json')
  folder=self.local(s)/'checkpoints';folder.mkdir(mode=0o700,exist_ok=True)
  if folder.is_symlink() or folder.stat().st_mode&0o077:raise RuntimeError('PRIVATE_CHECKPOINT_ROOT_REQUIRED')
  return folder/(snapshot+'.json')
 def collect(self,s,r,paths,snapshot=None):
  snapshot=uid(snapshot or r)
  with self.lock(s):
   state=self.load(s)
   if state.get('phase')!='ready':raise RuntimeError('SESSION_NOT_READY_OR_FINISHING')
   import operations,activity
   operations.verify_terminal(self,s)
   rf=self.export_record(s,r,snapshot)
   turn=json.loads((self.local(s)/(uid(r)+'.json')).read_text())
   if rf.exists():v=json.loads(rf.read_text())
   else:v={k:turn[k] for k in ('session_id','request_id','acpx_request_id','terminal') if k in turn};v['phase']='terminal'
   if v.get('request_id')!=r:raise RuntimeError('SNAPSHOT_REQUEST_BINDING_CHANGED')
   if not paths or len(paths)>64:raise ValueError('explicit bounded artifact list required')
   if v.get('declared_paths') is not None and v['declared_paths']!=paths:raise RuntimeError('EXPORT_DECLARATION_CHANGED')
   if v.get('export_verified'):
    old=self.result(s,r,snapshot)
    if old.get('checkpoint')!=activity.checkpoint(self,s,r,snapshot):raise RuntimeError('HISTORICAL_CHECKPOINT_USE_NEW_SNAPSHOT')
    return old
   if v['phase'] not in ('terminal','export_failed_retryable','exporting') or 'terminal' not in v:raise RuntimeError('TURN_NOT_TERMINAL')
   activity.pending(self,s,exclude=rf)
   cp=activity.checkpoint(self,s,r,snapshot)
   if v.get('checkpoint') is not None and v['checkpoint']!=cp:raise RuntimeError('EXPORT_REVISION_CHANGED_VM_RETAINED')
   v['checkpoint']=cp
   if not v.get('export_staging_dir'):
    v['export_staging_dir']=pathlib.Path(tempfile.mkdtemp(prefix='artifacts-'+snapshot+'-',dir=self.local(s))).name
   v.update(phase='exporting',export_verified=False,declared_paths=paths,manifest_dir=v['export_staging_dir']);atomic(rf,v)
   try:return self._collect_locked(s,r,paths,rf,v)
   except Exception:
    failed=json.loads(rf.read_text());failed.update(phase='export_failed_retryable',export_verified=False);atomic(rf,failed);raise
 def _collect_locked(self,s,r,paths,rf,v):
  self.event(s,'result_collection',r)
  output=self.local(s)/v['export_staging_dir'];snapshot=output/'snapshot.json'
  if output.is_symlink() or output.parent!=self.local(s):raise RuntimeError('EXPORT_STAGING_PATH_MISMATCH')
  if snapshot.exists():
   snap=json.loads(snapshot.read_text())
   if snap['declared_paths']!=paths or snap['attempt']!=self.load(s).get('attempt') or snap.get('checkpoint')!=v['checkpoint']:raise RuntimeError('EXPORT_SNAPSHOT_BINDING_MISMATCH')
   files=snap['files']
   for item in files:
    p=output/item['local_file']
    if p.is_symlink() or p.parent!=output:raise RuntimeError('EXPORT_SNAPSHOT_PATH')
    b=p.read_bytes()
    if len(b)!=item['size'] or digest(b)!=item['sha256']:raise RuntimeError('EXPORT_SNAPSHOT_CHANGED')
  else:
   files=[]
   for rel in paths:
    frame=list(self.remote(s,'artifact',request_id=r,path=rel))[-1];b=base64.b64decode(frame['data'],validate=True)
    if len(b)!=frame['size'] or digest(b)!=frame['sha256']:raise RuntimeError('ARTIFACT_CHECKSUM_MISMATCH')
    dest=output/(str(len(files))+'.bin');fd,tmp=tempfile.mkstemp(prefix='.artifact-',dir=output)
    with os.fdopen(fd,'wb') as f:os.fchmod(f.fileno(),0o600);f.write(b);f.flush();os.fsync(f.fileno())
    os.replace(tmp,dest);files.append({'path':rel,'size':len(b),'sha256':digest(b),'local_file':dest.name})
   if self.c.get('diagnostics_required',bool(self.c.get('storage'))):
    import diagnostics
    try:files.extend(diagnostics.capture(self,s,output))
    except Exception as e:raise diagnostics.failure(e) from None
   atomic(snapshot,{'declared_paths':paths,'attempt':self.load(s).get('attempt'),'checkpoint':v['checkpoint'],'files':files})
  manifest={'version':1,'actual_backend':'coordinator-local','durability':'local_disk_not_remote_backup','session_id':s,'request_id':r,'acpx_request_id':v['acpx_request_id'],'terminal':v['terminal'],'files':files,'checkpoint':v['checkpoint']}
  storage=self.c.get('storage')
  if storage is not None:
   if storage.get('backend')!='s3':raise ValueError('EXPLICIT_STORAGE_BACKEND_REQUIRED')
   atomic(rf,{**v,'phase':'exporting','export_verified':False,'manifest_dir':output.name})
   try:
    self.event(s,'result_upload',r)
    import storage as storage_adapter
    proof=storage_adapter.commit(storage,self.c['job'],s,v['checkpoint']['snapshot_id'],files,output,{**self.load(s).get('attempt',{}),'acpx_request_id':v['acpx_request_id'],'terminal':v['terminal'],'diagnostics_required':self.c.get('diagnostics_required',True),'checkpoint':v['checkpoint']});manifest.update(proof)
   except Exception:
    v.update(phase='export_failed_retryable',export_verified=False,manifest_dir=output.name);atomic(rf,v)
    try:self.event(s,'result_upload_failed',r)
    except Exception:pass
    raise RuntimeError('S3_EXPORT_UNAVAILABLE_RETRY_TRANSFER_VM_RETAINED') from None
   v['phase']='terminal'
  v['phase']='terminal'
  atomic(output/'manifest.json',manifest);v['manifest_sha256']=digest(canonical(manifest));v['manifest_dir']=output.name;v['export_verified']=True;atomic(rf,v)
  state=self.load(s);state['last_checkpoint']={'request_id':r,'snapshot_id':v['checkpoint']['snapshot_id'],'manifest_sha256':v['manifest_sha256'],'activity_revision':v['checkpoint']['activity_revision']};atomic(self.local(s)/'session.json',state)
  return manifest
 def result(self,s,r,snapshot=None):
  self.load(s);v=json.loads(self.export_record(s,r,snapshot).read_text())
  if not v.get('export_verified'):raise RuntimeError('RESULT_NOT_EXPORTED')
  output=self.local(s)/v['manifest_dir'];m=json.loads((output/'manifest.json').read_text())
  if digest(canonical(m))!=v['manifest_sha256'] or m['request_id']!=r or m['session_id']!=s:raise RuntimeError('MANIFEST_MISMATCH')
  if snapshot is not None and m.get('checkpoint',{}).get('snapshot_id',r)!=(snapshot or r):raise RuntimeError('SNAPSHOT_MANIFEST_MISMATCH')
  if m.get('actual_backend')=='s3':
   if m.get('checkpoint') is not None and m.get('export_binding',{}).get('checkpoint')!=m['checkpoint']:raise RuntimeError('CHECKPOINT_RECEIPT_BINDING_MISMATCH')
   import storage as storage_adapter
   storage_adapter.verify(self.c.get('storage') or {},m['receipt'],m['backend_binding'],attempt_binding=m['export_binding'])
  elif m.get('actual_backend')!='coordinator-local':raise RuntimeError('UNKNOWN_RECEIPT_BACKEND')
  for item in m['files']:
   p=output/item['local_file']
   if p.is_symlink() or p.parent!=output:raise RuntimeError('LOCAL_RESULT_PATH_MISMATCH')
   if not p.exists() and m.get('actual_backend')=='s3':storage_adapter.restore(self.c.get('storage') or {},m,item,output)
   b=p.read_bytes()
   if len(b)!=item['size'] or digest(b)!=item['sha256']:raise RuntimeError('LOCAL_RESULT_CORRUPT')
  return m
 def lifecycle(self,s):
  self.load(s)
  import lease
  turns=[json.loads(p.read_text()) for p in self.local(s).glob('*.json') if p.name not in ('session.json','lease.json') and 'request_id' in json.loads(p.read_text())]
  active=any(t.get('phase')=='queued' for t in turns);unknown=any(t.get('phase') in ('submit_intent','submit_unknown','watch_unknown','exporting','export_failed_retryable') for t in turns)
  operations_state=[json.loads(p.read_text()).get('phase') for p in self.local(s).glob('operation-*.json')]
  active=active or any(v in ('running','launching','launch_intent') for v in operations_state)
  unknown=unknown or any(v in ('start_unknown','unknown') for v in operations_state)
  unknown=unknown or any(json.loads(p.read_text()).get('phase')=='exec_unknown' for p in self.local(s).glob('exec-*.json'))
  return lease.report(self.local(s),active=active,unknown=unknown)
 def finish(self,s,r,snapshot=None):
  with self.lock(s):return self._finish_locked(s,r,snapshot)
 def _finish_locked(self,s,r,snapshot=None):
  import activity,operations
  snapshot=uid(snapshot or r);previous=self.load(s)
  candidate=previous.get('closing_candidate')
  if candidate and (candidate['request_id'],candidate['snapshot_id'])!=(r,snapshot):raise RuntimeError('FINALIZATION_CANDIDATE_CHANGED')
  if previous.get('phase')=='nomad_terminal':return previous
  if previous.get('phase')=='finish_requested':return self._wait_finished(s,previous)
  if previous.get('phase')=='ready':
   for path in self.local(s).glob('*.json'):
    row=json.loads(path.read_text())
    if row.get('request_id') and row.get('phase') in ('submit_intent','submit_unknown','queued','watch_unknown','exporting','export_failed_retryable'):raise RuntimeError('UNFINISHED_OR_UNKNOWN_LOCAL_TURN_VM_RETAINED')
    if row.get('request_id') and row.get('phase')=='terminal' and not row.get('export_verified'):raise RuntimeError('OTHER_TERMINAL_TURN_NOT_EXPORTED_VM_RETAINED')
  manifest=self.result(s,r,snapshot);cp=activity.require_current(self,s,manifest,r,snapshot)
  if candidate and candidate['manifest_sha256']!=digest(canonical(manifest)):raise RuntimeError('FINALIZATION_MANIFEST_CHANGED')
  if previous.get('phase')=='ready':
   for path in self.local(s).glob('*.json'):
    row=json.loads(path.read_text())
    if row.get('request_id') and row.get('phase') in ('submit_intent','submit_unknown','queued','watch_unknown','exporting','export_failed_retryable'):raise RuntimeError('UNFINISHED_OR_UNKNOWN_LOCAL_TURN_VM_RETAINED')
    if row.get('request_id') and row.get('phase')=='terminal':
     if not row.get('export_verified'):raise RuntimeError('OTHER_TERMINAL_TURN_NOT_EXPORTED_VM_RETAINED')
     self.result(s,row['request_id'])
   operations.verify_terminal(self,s);activity.pending(self,s)
   self.target()
   rows=self.nomad(['job','allocs','-json','-namespace='+self.c['namespace'],self.c['job']],text=True)
   if rows.returncode:raise RuntimeError('JOB_ALLOCS_UNAVAILABLE')
   if any(a['ID']!=self.c['allocation'] and a.get('ClientStatus') in ('pending','running') for a in json.loads(rows.stdout)):raise RuntimeError('OTHER_ALLOCATION_ACTIVE')
   candidate={'request_id':r,'snapshot_id':snapshot,'manifest_sha256':digest(canonical(manifest)),'activity_revision':cp['activity_revision'],'step':'intent'}
   previous.update(phase='finalizing',closing_candidate=candidate);atomic(self.local(s)/'session.json',previous)
  elif previous.get('phase')!='finalizing':raise RuntimeError('SESSION_NOT_READY_OR_FINALIZING')
  if candidate['step']=='intent':
   list(self.remote(s,'prepare_finish',files=[f for f in manifest['files'] if f.get('source')!='native_diagnostic'],manifest_sha256=candidate['manifest_sha256']))
   candidate['step']='prepared';previous['closing_candidate']=candidate;atomic(self.local(s)/'session.json',previous)
  if candidate['step']=='prepared':
   self.event(s,'receipt_verified',r);list(self.remote(s,'close'))
   candidate['step']='closed';atomic(self.local(s)/'session.json',previous)
  if candidate['step']=='closed':
   list(self.remote(s,'finish_ack',manifest_sha256=candidate['manifest_sha256']))
   candidate['step']='acknowledged';atomic(self.local(s)/'session.json',previous)
  previous['phase']='finish_requested';atomic(self.local(s)/'session.json',previous)
  stop=self.nomad(['job','stop','-yes','-detach','-namespace='+self.c['namespace'],self.c['job']],text=True)
  if stop.returncode:raise RuntimeError('STOP_OUTCOME_UNKNOWN_RESULTS_RETAINED')
  return self._wait_finished(s,previous)
 def _cleanup_confirmed(self,a):
  if a.get('DesiredStatus')!='stop' or a.get('ClientStatus') not in ('complete','failed'):return False
  if self.c.get('vault'):
   events=a.get('TaskStates',{}).get(self.c['task'],{}).get('Events',[])
   phases={e.get('DriverMessage') for e in events}
   if not {'lima task_token_revoked','lima vm_stopped'}.issubset(phases):return False
  return True
 def _wait_finished(self,s,v):
  for _ in range(30):
   a=self.target()
   if v.get('attempt') is not None and v['attempt']!=self.attempt(a):raise RuntimeError('TERMINAL_ATTEMPT_CHANGED_CLEANUP_UNCONFIRMED')
   if self._cleanup_confirmed(a):
    v['phase']='nomad_terminal';v['disk_purge_requested']=False;atomic(self.local(s)/'session.json',v);return v
   time.sleep(1)
  raise RuntimeError('STOP_OR_TOKEN_CLEANUP_PENDING_RESULTS_RETAINED')

def main():
 p=argparse.ArgumentParser();p.add_argument('--config',required=True);p.add_argument('--operations-config');sub=p.add_subparsers(dest='cmd',required=True)
 for name in ('templates','models','capacity','request-status','propose','request'):
  q=sub.add_parser(name)
  if name in ('request','request-status'):q.add_argument('--session',required=True)
  if name=='request':
   for k in ('template','template-version','purpose'):q.add_argument('--'+k,required=True)
   q.add_argument('--architecture',choices=['auto','amd64','arm64'],default='auto')
   for k in ('vcpus','memory-mib','disk-gib'):q.add_argument('--'+k,type=int,required=True)
   q.add_argument('--model',help='Exact approved coordinator model ID; worker models remain independently selectable');q.add_argument('--project',default='');q.add_argument('--task-link',default='');q.add_argument('--creator-record')
  if name=='capacity':
   for k in ('template','template-version'):q.add_argument('--'+k)
   for k in ('vcpus','memory-mib','disk-gib'):q.add_argument('--'+k,type=int)
   q.add_argument('--architecture',choices=['auto','amd64','arm64'],default='auto')
  if name=='propose':q.add_argument('--reason',required=True);q.add_argument('--tool',action='append',default=[])

 for name in ('create','status','lifecycle','model-select','exec','template-save-plan','template-save','template-save-status','prepare-template','template-draft','operation-start','operation-status','operation-logs','operation-wait','operation-cancel','submit','watch','collect','result','finish'):
  a=sub.add_parser(name);a.add_argument('--session',required=True)
  if name in ('submit','watch','collect','result','finish'):a.add_argument('--request',required=True)
  if name=='model-select':a.add_argument('--selection',required=True);a.add_argument('--model',required=True)
  if name.startswith('template-save'):
   a.add_argument('--operation',required=True)
   if name=='template-save-plan':
    for flag in ('draft','request'):a.add_argument('--'+flag,required=True)
    a.add_argument('--snapshot');a.add_argument('--data-composition-file',required=True);a.add_argument('--stopped-source-receipt')
   if name=='template-save':a.add_argument('--confirmation-file',required=True);a.add_argument('--operator-policy-file',required=True)
  if name in ('prepare-template','template-draft'):
   a.add_argument('--draft',required=True)
   if name=='prepare-template':
    for flag in ('request','template','template-version','recipe-path','checks-path'):a.add_argument('--'+flag,required=True)
  if name.startswith('operation-'):
   a.add_argument('--operation',required=True)
   if name=='operation-start':
    a.add_argument('--label',required=True);a.add_argument('--timeout',type=int);a.add_argument('--cwd');a.add_argument('argv',nargs=argparse.REMAINDER)
   if name=='operation-logs':a.add_argument('--cursor',type=int)
   if name=='operation-wait':a.add_argument('--wait-seconds',type=int,default=30)
  if name=='exec':
   a.add_argument('--exec-id',required=True);a.add_argument('--timeout',type=int);a.add_argument('--cwd');a.add_argument('argv',nargs=argparse.REMAINDER)
  if name=='submit':a.add_argument('--prompt-file',required=True)
  if name in ('collect','result','finish','prepare-template'):a.add_argument('--snapshot')
  if name=='collect':a.add_argument('paths',nargs='+')
 a=p.parse_args()
 if a.cmd in ('templates','models','capacity','request-status','request','propose'):
  from catalog_cli import CatalogClient
  c=CatalogClient(a.config)
  if a.cmd=='templates':out=c.templates()
  elif a.cmd=='models':
   import model_catalog
   out=model_catalog.listing(c.c)
  elif a.cmd=='capacity':
   fields=[a.template,a.template_version,a.vcpus,a.memory_mib,a.disk_gib]
   if any(v is not None for v in fields):
    if not all(v is not None for v in fields):p.error('capacity request fit requires template/version/vcpus/memory-mib/disk-gib')
    out=c.capacity_request(a.template,a.template_version,a.architecture,a.vcpus,a.memory_mib,a.disk_gib)
   else:out=c.capacity()
  elif a.cmd=='request-status':out=c.request_status(a.session)
  elif a.cmd=='propose':out=c.propose(a.reason,a.tool)
  else:out=c.request(a.session,a.template,a.template_version,a.architecture,a.vcpus,a.memory_mib,a.disk_gib,a.purpose,a.project,a.task_link,a.creator_record,a.model)
  print(json.dumps(out),flush=True);return
 adapter=Adapter(a.config,a.operations_config);s=uid(a.session)
 if a.cmd.startswith('template-save'):
  import template_save,exchange
  operation=uid(a.operation);root=adapter.local(s);path=root/('template-save-'+operation+'.state.json')
  if a.cmd=='template-save-plan':out=template_save.plan(adapter,s,a.draft,a.request,a.snapshot,operation,json.loads(pathlib.Path(a.data_composition_file).read_text()),None if not a.stopped_source_receipt else json.loads(pathlib.Path(a.stopped_source_receipt).read_text()))
  elif a.cmd=='template-save-status':out=template_save.status(path)
  else:
   adapter.require_initiator(s);packet=json.loads((root/('template-save-'+operation+'.plan.json')).read_text());approval=exchange.protected(a.confirmation_file)
   out=template_save.execute(packet,approval,path,template_save.OperatorPipeline(a.operator_policy_file))
  print(json.dumps(out),flush=True);return
 if a.cmd=='model-select':
  print(json.dumps(adapter.select_model(s,a.selection,a.model)),flush=True);return
 if a.cmd in ('prepare-template','template-draft'):
  import template_draft
  out=template_draft.prepare(adapter,s,a.draft,a.template,a.template_version,a.request,a.recipe_path,a.checks_path,a.snapshot) if a.cmd=='prepare-template' else template_draft.inspect_draft(adapter,s,a.draft)
  print(json.dumps(out),flush=True);return
 if a.cmd.startswith('operation-'):
  import operations
  if a.cmd=='operation-start':out=operations.start(adapter,s,a.operation,a.argv[1:] if a.argv[:1]==['--'] else a.argv,a.label,a.timeout,a.cwd)
  elif a.cmd=='operation-status':out=operations.status(adapter,s,a.operation)
  elif a.cmd=='operation-logs':out=operations.logs(adapter,s,a.operation,a.cursor)
  elif a.cmd=='operation-cancel':out=operations.cancel(adapter,s,a.operation)
  else:
   for out in operations.wait(adapter,s,a.operation,a.wait_seconds):print(json.dumps(out),flush=True)
   if out['phase'] not in operations.TERMINAL:sys.exit(3)
   if out['phase']!='completed':sys.exit(2)
   return
  print(json.dumps(out),flush=True)
  if out.get('phase') in ('unknown','start_unknown','failed','timed_out'):sys.exit(2)
  return
 if a.cmd=='create':out=adapter.create(s)
 elif a.cmd=='status':adapter.load(s);out=list(adapter.remote(s,'status'))[-1]
 elif a.cmd=='lifecycle':out=adapter.lifecycle(s)
 elif a.cmd=='exec':out=adapter.exec(s,a.exec_id,a.argv[1:] if a.argv[:1]==['--'] else a.argv,a.timeout,a.cwd)
 elif a.cmd=='submit':out=adapter.submit(s,a.request,sys.stdin.read() if a.prompt_file=='-' else pathlib.Path(a.prompt_file).read_text())
 elif a.cmd=='watch':
  for e in adapter.watch(s,a.request):print(json.dumps(e),flush=True)
  return
 elif a.cmd=='collect':out=adapter.collect(s,a.request,a.paths,a.snapshot)
 elif a.cmd=='result':out=adapter.result(s,a.request,a.snapshot)
 else:out=adapter.finish(s,a.request,a.snapshot)
 print(json.dumps(out),flush=True)
 if a.cmd=='exec' and (out.get('phase')!='exec_completed' or out.get('exit_code')!=0):sys.exit(2)
if __name__=='__main__':
 try:main()
 except Exception as e:print(json.dumps({'error':str(e) if isinstance(e,(ValueError,RuntimeError)) else type(e).__name__,'prompt_retry':False}),file=sys.stderr);sys.exit(1)
