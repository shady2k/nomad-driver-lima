"""Compatible VM-ID facade. Existing ownership, dispatch and export remain authoritative."""
import contextlib,fcntl,hashlib,json,os,pathlib,re,uuid,urllib.parse,stat
from catalog_cli import CatalogClient,atomic,canonical,uid
from exchange import protected
from workerctl import Adapter
import activity

def discovery_config(config,explicit=None):
 selected=explicit or os.environ.get('NOMAD_WORKERS_DISCOVERY_CONFIG')
 original=protected(config)
 if not selected:
  if not original.get('allocation'):return str(config)
  selected=str(pathlib.Path(os.environ.get('XDG_CONFIG_HOME',str(pathlib.Path.home()/'.config')))/'nomad-workers/catalog-client.json')
 global_config=protected(selected)
 if global_config.get('allocation'):raise RuntimeError('GLOBAL_DISCOVERY_CONFIG_REQUIRED')
 if any(original.get(k)!=global_config.get(k)for k in ('owner_id','namespace')):raise RuntimeError('DISCOVERY_OWNER_SCOPE_CHANGED')
 return str(selected)

def creator(record=None,env=None):
 env=os.environ if env is None else env;p=record or env.get('NOMAD_WORKERS_CREATOR_RECORD')
 if not p or not pathlib.Path(p).is_absolute():raise RuntimeError('NATIVE_INITIATOR_CONTEXT_REQUIRED')
 native=protected(p)
 if native.get('version')!=1 or native.get('ended')or native.get('evidence')!='native_hook_observation_not_authenticated_liveness':raise RuntimeError('CURRENT_NATIVE_CREATOR_REQUIRED')
 if native.get('native_creator_record'):
  current=protected(native['native_creator_record'])
  if current.get('ended')or any(current.get(k)!=native.get(k)for k in ('agent_instance_id','native_session_id','generation','evidence')):raise RuntimeError('CURRENT_NATIVE_CREATOR_CHANGED')
 elif native.get('native_tool_use_id'):raise RuntimeError('PER_CALL_NATIVE_CONTEXT_REQUIRED')
 me={'agent_id':uid(native['agent_instance_id']),'native_session_id':native['native_session_id'],'generation':uid(native['generation'])}
 if not isinstance(me['native_session_id'],str)or not re.fullmatch('[A-Za-z0-9._:-]{1,256}',me['native_session_id']):raise RuntimeError('NATIVE_INITIATOR_CONTEXT_INVALID')
 return str(p),native,me

def private_dir(path):
 path=pathlib.Path(path);path.mkdir(mode=0o700,parents=True,exist_ok=True);st=path.lstat()
 if path.is_symlink()or not path.is_dir()or st.st_uid!=os.getuid()or st.st_mode&0o077:raise RuntimeError('VM_PRIVATE_STATE_REQUIRED')
 return path

def private_lock(path):
 fd=os.open(path,os.O_RDWR|os.O_CREAT|os.O_NOFOLLOW,0o600)
 st=os.fstat(fd)
 if not stat.S_ISREG(st.st_mode)or st.st_uid!=os.getuid()or st.st_mode&0o077:
  os.close(fd);raise RuntimeError('VM_PRIVATE_LOCK_REQUIRED')
 return os.fdopen(fd,'a')

class VMs:
 def __init__(self,client,record=None,env=None,adapter_factory=Adapter,operations_config=None):
  self.client=client;self.record,self.native,self.me=creator(record,env);self.adapter_factory=adapter_factory;self.operations_config=operations_config
 def ownership(self,job,vm_id):
  meta=job.get('Meta')or{};wanted={'nomad_workers_owner':self.client.c['owner_id'],'creator_agent_id':self.me['agent_id'],'creator_session_id':self.me['native_session_id'],'creator_generation':self.me['generation'],'worker_session_id':vm_id}
  tasks=[t for g in job.get('TaskGroups',[])for t in g.get('Tasks',[])if t.get('Name')==self.client.c['task']]
  return job.get('ID')=='worker-'+vm_id and job.get('Namespace')==self.client.c['namespace'] and all(meta.get(k)==v for k,v in wanted.items())and len(tasks)==1 and tasks[0].get('Driver')=='lima' and tasks[0].get('Config',{}).get('worker_session_id')==vm_id
 def listing(self):
  ns=urllib.parse.quote(self.client.c['namespace'],safe='');jobs=self.client.api('/v1/jobs?namespace='+ns+'&prefix=worker-&meta=true&per_page=1025')
  if not isinstance(jobs,list)or len(jobs)>1024:raise RuntimeError('VM_DISCOVERY_LIMIT')
  out=[];seen=set()
  for stub in jobs:
   meta=stub.get('Meta')or{};vm=meta.get('worker_session_id')
   if not vm:continue
   try:uid(vm)
   except (ValueError,TypeError):continue
   if any(meta.get(k)!=v for k,v in {'nomad_workers_owner':self.client.c['owner_id'],'creator_agent_id':self.me['agent_id'],'creator_session_id':self.me['native_session_id'],'creator_generation':self.me['generation']}.items()):continue
   job=self.client.api('/v1/job/'+urllib.parse.quote(stub['ID'],safe='')+'?namespace='+ns)
   if not self.ownership(job,vm):raise RuntimeError('VM_REMOTE_IDENTITY_CHANGED')
   if vm in seen:raise RuntimeError('VM_DISCOVERY_AMBIGUOUS')
   seen.add(vm)
   if job.get('Stop')or job.get('Status')=='dead':continue
   allocations=self.client.api('/v1/job/'+urllib.parse.quote(job['ID'],safe='')+'/allocations?namespace='+ns)
   active=[a for a in allocations if a.get('ClientStatus')not in ('complete','failed','lost')]
   if len(active)>1:state='ambiguous_allocations'
   elif active:state=active[0].get('ClientStatus','unknown')
   else:state='awaiting_allocation'
   out.append({'vm_id':vm,'state':state,'ambiguous':len(active)>1,'local_binding_available':(self.client.root/vm/'target-config.json').is_file()})
  return {'vms':out,'scope':'mine','from_nomad':True,'read_only':True}
 def resolve(self,vm,initialize=False):
  uid(vm);folder=self.client.root/vm;target=folder/'target-config.json'
  if not target.exists() and initialize and (folder/'allocation-intent.json').exists():
   intent=protected(folder/'allocation-intent.json')
   if (intent.get('creator_agent_id'),intent.get('creator_session_id'),intent.get('creator_generation'))!=(self.me['agent_id'],self.me['native_session_id'],self.me['generation']):raise RuntimeError('VM_LOCAL_OWNER_MISMATCH')
   self.client.request_status(vm)
  if not target.exists():
   candidates=[v for v in self.listing()['vms']if v['vm_id']==vm]
   if not candidates:raise RuntimeError('VM_NOT_VERIFIED_FOR_CURRENT_INITIATOR')
   if candidates[0]['ambiguous']:raise RuntimeError('VM_ALLOCATION_AMBIGUOUS_NO_REBIND')
   return None,candidates[0]
  binding=protected(target)
  if (binding.get('owner_id'),binding.get('namespace'),binding.get('agent_instance_id'),binding.get('creator_session_id'),binding.get('creator_generation'))!=(self.client.c['owner_id'],self.client.c['namespace'],self.me['agent_id'],self.me['native_session_id'],self.me['generation']):raise RuntimeError('VM_LOCAL_OWNER_MISMATCH')
  a=self.adapter_factory(target,self.operations_config);row=a.target()
  if not self.ownership(row.get('Job')or{},vm):raise RuntimeError('VM_REMOTE_OWNER_CHANGED')
  ns=urllib.parse.quote(self.client.c['namespace'],safe='');all_allocs=self.client.api('/v1/job/worker-'+vm+'/allocations?namespace='+ns)
  active=[x for x in all_allocs if x.get('ClientStatus')not in ('complete','failed','lost')]
  if len(active)>1 or (active and active[0]['ID']!=binding['allocation']):raise RuntimeError('VM_ALLOCATION_AMBIGUOUS_NO_REBIND')
  a.require_initiator(vm)
  state_file=folder/'session.json'
  if state_file.exists()and a.load(vm).get('attempt')!=a.attempt(row):raise RuntimeError('VM_ATTEMPT_CHANGED_NO_REBIND')
  return a,{'vm_id':vm,'state':row.get('ClientStatus'),'local_binding_available':True}
 def connect(self,vm):
  a,status=self.resolve(vm,True)
  if a is None:return status
  p=a.local(vm)/'session.json'
  state=a.load(vm)if p.exists()else a.create(vm)
  if state.get('phase')=='management_ready':state=a.create(vm)
  return {'vm_id':vm,'state':'connected'if state.get('phase')=='ready'else state.get('phase','unknown'),'prompt_dispatched':False}
 def tasks(self,a,vm):
  rows=[]
  for path in a.local(vm).glob('*.json'):
   if not re.fullmatch(r'[a-f0-9-]{36}\.json',path.name):continue
   row=protected(path)
   if 'prompt_sha256'not in row:continue
   if row.get('session_id')!=vm or row.get('request_id')!=path.stem:raise RuntimeError('VM_TASK_RECEIPT_CHANGED')
   uid(row['request_id']);rows.append(row)
  if len(rows)>128:raise RuntimeError('VM_TASK_LIMIT')
  return rows
 def choose(self,a,vm,request=None):
  rows=self.tasks(a,vm)
  if request:
   matches=[r for r in rows if r['request_id']==uid(request)]
   if len(matches)!=1:raise RuntimeError('VM_TASK_NOT_FOUND')
   return matches[0]
  unfinished=[r for r in rows if r.get('phase') in activity.UNKNOWN]
  if len(unfinished)>1:raise RuntimeError('VM_TASK_AMBIGUOUS_SELECT_SPECIFIC_TASK')
  if unfinished:return unfinished[0]
  if len(rows)==1:return rows[0]
  if not rows:return None
  revisions=[r.get('activity_revision')for r in rows]
  if any(type(n)is not int or n<1 for n in revisions)or len(set(revisions))!=len(revisions):raise RuntimeError('VM_TASK_ORDER_AMBIGUOUS')
  return max(rows,key=lambda r:r['activity_revision'])
 @contextlib.contextmanager
 def intent(self,action,inputs,make):
  call=self.native.get('native_tool_use_id')
  if not isinstance(call,str)or not re.fullmatch('[A-Za-z0-9._:-]{1,256}',call):raise RuntimeError('NATIVE_CALL_CONTEXT_REQUIRED_NO_DISPATCH')
  base=private_dir(self.client.root/'vm-interface');folder=private_dir(base/'intents');key=hashlib.sha256(canonical({'owner':self.me,'call':call,'action':action})).hexdigest();path=folder/(key+'.json')
  with private_lock(folder/(key+'.lock'))as lock:
   fcntl.flock(lock,fcntl.LOCK_EX);fingerprint=hashlib.sha256(canonical(inputs)).hexdigest();fresh=not path.exists()
   if fresh:row={'version':1,'owner':self.me,'action':action,'input_sha256':fingerprint,'phase':'prepared',**make()};atomic(path,row)
   else:
    row=protected(path)
    if row.get('owner')!=self.me or row.get('action')!=action or row.get('input_sha256')!=fingerprint:raise RuntimeError('VM_CALL_INTENT_CHANGED_NO_REPLAY')
   yield row,fresh,path
 def create_intent(self,vm):
  uid(vm);folder=self.client.root/'vm-interface/intents';matches=[]
  for path in folder.glob('*.json'):
   row=protected(path)
   if row.get('action')=='create'and row.get('vm_id')==vm:matches.append((path,row))
  if len(matches)!=1:raise RuntimeError('VM_CREATE_INTENT_NOT_FOUND_OR_AMBIGUOUS')
  path,row=matches[0]
  if row.get('owner')!=self.me:raise RuntimeError('VM_CREATE_OWNER_CHANGED')
  return path,row
 def retry(self,vm,allow_register=False,params=None):
  path,row=self.create_intent(vm)
  with (private_lock(path.with_suffix('.lock'))if allow_register else contextlib.nullcontext())as lock:
   if lock is not None:fcntl.flock(lock,fcntl.LOCK_EX)
   row=protected(path)
   if params is not None and hashlib.sha256(canonical(params)).hexdigest()!=row['input_sha256']:raise RuntimeError('VM_PAYLOAD_CHANGED_SAME_ID_CONFLICT')
   local=self.client.root/vm/'allocation-intent.json'
   if not local.exists():
    import allocation_retry
    remote=allocation_retry.lookup(self.client,'worker-'+vm)
    if remote is not None:raise RuntimeError('VM_REMOTE_JOB_WITHOUT_DURABLE_ALLOCATION_INTENT_NO_ADOPTION')
    # Missing local/server state alone is not durable proof of non-dispatch.
    if row.get('phase')=='submitted':return {'vm_id':vm,'state':'previously_accepted_missing_durable_intent_no_recreate','registration':'absent','prompt_retry':False}
    if row.get('phase')=='validation_rejected':return {'vm_id':vm,'state':'validation_rejected','registration':'absent','error':row['error'],'new_request_allowed_after_explicit_correction':True,'prompt_retry':False}
    if row.get('never_dispatched_verified')is not True:return {'vm_id':vm,'state':'legacy_or_post_dispatch_absent_lookup_only','observed_job':'absent','registration':'unknown','automatic_registration_allowed':False,'next_action':'status_same_vm_id_only','prompt_retry':False}
    chosen=params if params is not None else row.get('parameters')
    if allow_register and chosen is not None:
     return self.dispatch_create(chosen,row,path)
    return {'vm_id':vm,'state':'not_registered_before_dispatch','registration':'absent','reason':'no_durable_allocation_intent_or_job','next_action':'retry_same_vm_id_with_original_payload_or_report_validation_error','observed_unix':int(__import__('time').time()),'prompt_retry':False}
   observation=self.client.registration_status(vm,allow_register)
   return {**observation,'vm_id':vm,'state':observation.get('state',observation['registration']),'next_action':'connect'if observation['registration']=='accepted'else'status_same_vm_id_only'if observation.get('automatic_registration_allowed')is False else'retry_same_vm_id','prompt_retry':False}
 def dispatch_create(self,params,row,path):
  vm=row['vm_id'];row['never_dispatched_verified']=False;atomic(path,row)
  try:
   self.client.request(vm,params['template'],params['template_version'],params.get('architecture','auto'),params['vcpus'],params['memory_mib'],params['disk_gib'],params.get('purpose','work'),params.get('project',''),creator_record=self.native.get('native_creator_record',self.record),model=params.get('model'),task_title=params['task_title'],source_agent_name=self.native.get('native_agent_name'),source_agent_type=self.native.get('native_agent_type'),repository_profile=params.get('repository_profile'))
   row['phase']='submitted';atomic(path,row)
  except Exception as error:
   submitted=(self.client.root/vm/'allocation-intent.json').exists();row['never_dispatched_verified']=not submitted
   row['phase']='create_unknown'if submitted else'validation_rejected'if isinstance(error,ValueError)else'not_dispatched'
   from secure_logs import Redactor
   safe_code=str(error)if re.fullmatch('[A-Z][A-Z0-9_]{0,119}',str(error))else'PRE_DISPATCH_CHECK_UNAVAILABLE'
   row['error']=Redactor().text(str(error))[:512]if isinstance(error,ValueError)else safe_code if not submitted else'REGISTRATION_OUTCOME_UNKNOWN_USE_SAME_VM_ID'
   atomic(path,row)
   return {'vm_id':vm,'state':row['phase'],'error':row['error'],'registration':'unknown'if submitted else'not_attempted','next_action':'retry_same_vm_id'if row['phase']!='validation_rejected'else'correct_invalid_input_as_explicit_new_request_after_definitive_rejection','prompt_retry':False}
  result={'vm_id':vm,'state':'submitted','next_action':'connect','dispatch_repeated':False,'snapshot_not_reservation':True,'observed_unix':int(__import__('time').time())}
  if hasattr(self.client,'registration_status'):
   try:result['nomad']=self.client.registration_status(vm,False)
   except RuntimeError:result['nomad_observation']='unavailable_keep_same_vm_id'
  return result
 def create(self,params,vm_id=None):
  if any(type(params.get(k))is not int or params[k]<1 for k in ['vcpus','memory_mib','disk_gib']):raise ValueError('POSITIVE_VM_RESOURCES_REQUIRED')
  if vm_id is not None:return self.retry(vm_id,True,params)
  with self.intent('create',params,lambda:{'vm_id':str(uuid.uuid4()),'parameters':params,'never_dispatched_verified':True})as(row,fresh,path):
   if fresh:return self.dispatch_create(params,row,path)
   vm=row['vm_id']
  # Release the same durable intent lock before read-only reconciliation.
  return self.retry(vm,False)
 def execute(self,vm,argv,timeout=None,cwd=None):
  a,status=self.resolve(vm)
  if a is None:raise RuntimeError('VM_LOCAL_BINDING_NOT_AVAILABLE')
  import exec_checkpoint
  if not (a.local(vm)/'session.json').exists():
   with a.lock(vm):exec_checkpoint.ensure_session(a,vm)
  inputs={'vm_id':vm,'argv_sha256':hashlib.sha256(canonical(argv)).hexdigest(),'timeout':timeout,'cwd':cwd}
  with self.intent('exec',inputs,lambda:{'vm_id':vm,'execution_id':str(uuid.uuid4())})as(row,fresh,path):
   op=row['execution_id']
   if not fresh:
    import operations
    result=operations.status(a,vm,op)
   else:
    try:result=a.exec(vm,op,argv,timeout,cwd)
    except Exception:row['phase']='exec_unknown';atomic(path,row);raise
    row['phase']=result['phase'];atomic(path,row)
   public={k:result[k]for k in ('execution_id','operation_id','next_action','phase','outcome','exit_code','stdout','stderr','output_truncated','operation_continues')if k in result}
   return {'vm_id':vm,**public,'transport':'native-nomad-exec','requires_acp_connection':False,'command_replayed':False}
 def send(self,vm,text):
  uid(vm)
  if not isinstance(text,str)or not text or len(text.encode())>1048576:raise ValueError('BOUNDED_NONEMPTY_TEXT_REQUIRED')
  with self.intent('send',{'vm_id':vm,'text_sha256':hashlib.sha256(text.encode()).hexdigest()},lambda:{'vm_id':vm,'request_id':str(uuid.uuid4())})as(intent,fresh,path):
   q=intent['request_id']
   if not fresh:return {'vm_id':vm,'state':intent['phase'],'next_action':'reconnect','dispatch_repeated':False}
   try:
    a,status=self.resolve(vm,True)
    if a is None:raise RuntimeError('VM_NOT_READY_CONNECT_SAME_VM')
    if not (a.local(vm)/'session.json').exists():a.create(vm)
    if (self.client.root/'vm-interface/finishes'/(vm+'.json')).exists():raise RuntimeError('VM_FINISH_IN_PROGRESS_NO_NEW_WORK')
    activity.pending(a,vm)
   except Exception:
    intent['phase']='not_dispatched';atomic(path,intent);raise
   try:receipt=a.submit(vm,q,text);intent['phase']=receipt.get('phase','unknown')
   except Exception:intent['phase']='send_unknown';atomic(path,intent);return {'vm_id':vm,'state':'send_unknown','next_action':'reconnect','prompt_retry':False}
   atomic(path,intent);return {'vm_id':vm,'state':intent['phase'],'next_action':'reconnect','dispatch_repeated':False}
 def status(self,vm):
  import status_ui
  return status_ui.read(self,vm)
 def _status_snapshot(self,vm):
  if not (self.client.root/uid(vm)/'target-config.json').exists():
   try:self.create_intent(vm)
   except RuntimeError:pass
   else:return self.retry(vm,False)
  a,out=self.resolve(vm)
  if a is None:return out
  p=a.local(vm)/'session.json'
  if not p.exists():return {**out,'session_state':'not_initialized','next_action':'connect'}
  state=a.load(vm)
  try:row=self.choose(a,vm);task_state=row.get('phase')if row else'none'
  except RuntimeError:task_state='ambiguous'
  result={**out,'session_state':state.get('phase'),'task_state':task_state,'readiness_evidence':'durable_session_receipt_and_nomad_not_model_probe','prompt_retry':False,'owner_heartbeat':{'state':'unknown','reason':'not_inferred_from_vm_running'}}
  if task_state not in ('none','ambiguous'):
   result['task_evidence']='durable_local_receipt';result['task_receipt_observed_unix']=int((a.local(vm)/(row['request_id']+'.json')).stat().st_mtime)
   try:
    import request_transcript
    guest=request_transcript.snapshot(a,vm,row['request_id']);result['guest_task_snapshot']=guest
    result['task_state']=guest['state'];result['last_observed_event']=guest.get('last_observed_event');result['owner_heartbeat']=guest['owner_heartbeat']
   except Exception:result.update(task_state='unknown',guest_task_snapshot={'observation':'unavailable','error':'GUEST_TASK_SNAPSHOT_UNAVAILABLE','observed_unix':int(__import__('time').time())})
  import status_ui
  result['work_items']=status_ui.work_items(a,vm)
  return result
 def history(self,vm,request=None):
  a,status=self.resolve(vm)
  if a is None:raise RuntimeError('VM_LOCAL_BINDING_NOT_AVAILABLE')
  import request_transcript
  rows=self.tasks(a,vm)
  if request:rows=[self.choose(a,vm,request)]
  elif len(rows)>1:
   revisions=[r.get('activity_revision')for r in rows]
   if any(type(x)is not int or x<1 for x in revisions)or len(set(revisions))!=len(revisions):raise RuntimeError('VM_TASK_ORDER_AMBIGUOUS_SELECT_SPECIFIC_TASK')
   rows.sort(key=lambda x:x['activity_revision'])
  for number,row in enumerate(rows,1):
   cursor=None
   for _ in range(256):
    page=request_transcript.read(a,vm,row['request_id'],cursor=cursor)
    yield {'vm_id':vm,'task':number,'entries':page['entries'],'terminal':page.get('terminal'),'history_complete':page.get('history_complete',False),'has_more':page['has_more'],'scan_in_progress':page.get('scan_in_progress',False),'scan_bytes_read':page.get('scan_bytes_read',0),'scan_events':page.get('scan_events',0),'next_action':'reading_history'if page['has_more']else'inspect_history'}
    if not page['has_more']:break
    if not page.get('cursor')or page['cursor']==cursor:raise RuntimeError('VM_HISTORY_CURSOR_CHANGED')
    cursor=page['cursor']
   else:raise RuntimeError('VM_HISTORY_PAGE_LIMIT')
 def reconnect(self,vm,request=None):
  connected=self.connect(vm)
  if connected['state']!='connected':yield connected;return
  a,_=self.resolve(vm);row=self.choose(a,vm,request)
  if row is None:yield {'vm_id':vm,'state':'connected','task_state':'none'};return
  for frame in a.watch(vm,row['request_id']):
   if frame['event']['type']=='turn_result':yield {'vm_id':vm,'state':'terminal','terminal':__import__('request_transcript').terminal_summary(frame['event'].get('result'))}
  yield from self.history(vm,row['request_id'])
 def cancel(self,vm,request=None,status_only=False):
  a,_=self.resolve(vm)
  if a is None:raise RuntimeError('VM_LOCAL_BINDING_NOT_AVAILABLE')
  row=self.choose(a,vm,request)
  if row is None:raise RuntimeError('VM_TASK_NOT_FOUND')
  import request_cancel
  result=request_cancel.status(a,vm,row['request_id'])if status_only else request_cancel.cancel(a,vm,row['request_id'])
  return {'vm_id':vm,**{k:result[k]for k in ['phase','terminal','cancel_is_terminal_proof','prompt_retry','next_action']},'cancel_outcome_unknown':result.get('cancel_outcome_unknown',False)}
 def finish(self,vm,paths):
  a,status=self.resolve(vm)
  if a is None:raise RuntimeError('VM_LOCAL_BINDING_NOT_AVAILABLE')
  import exec_checkpoint
  if (a.local(vm)/'session.json').exists():state=a.load(vm)
  else:
   with a.lock(vm):state=exec_checkpoint.ensure_session(a,vm)
  candidate=state.get('closing_candidate')
  if candidate:out=a.finish(vm,candidate['request_id'],candidate['snapshot_id'])
  else:
   base=private_dir(self.client.root/'vm-interface');folder=private_dir(base/'finishes');path=folder/(uid(vm)+'.json')
   with private_lock(folder/(vm+'.lock'))as lock:
    fcntl.flock(lock,fcntl.LOCK_EX)
    if path.exists():
     plan=protected(path)
     if plan.get('owner')!=self.me or plan.get('vm_id')!=vm or plan.get('attempt')!=state.get('attempt'):raise RuntimeError('VM_FINISH_BINDING_CHANGED')
     if paths and paths!=plan['paths']:raise RuntimeError('VM_FINISH_EXPORT_DECLARATION_CHANGED')
     if activity.revision(a,vm)!=plan['activity_revision']:raise RuntimeError('VM_FINISH_ACTIVITY_CHANGED')
    else:
     activity.pending(a,vm);rows=self.tasks(a,vm);selected=self.choose(a,vm)
     if any(r.get('phase')!='terminal'for r in rows):raise RuntimeError('FINISH_REQUIRES_VERIFIED_TERMINAL_TASKS')
     if selected is not None and not paths:raise RuntimeError('DECLARED_RESULT_PATHS_REQUIRED_BEFORE_FINISH')
     if selected is None:exec_checkpoint.local_idle(a,vm)
     plan={'vm_id':vm,'owner':self.me,'attempt':state.get('attempt'),'activity_revision':activity.revision(a,vm),'paths':paths,'request_id':selected['request_id']if selected else None,'snapshot_id':str(uuid.uuid4()),'older_exports':[r['request_id']for r in rows if selected is not None and r['request_id']!=selected['request_id']and not r.get('export_verified')]};atomic(path,plan)
    for q in plan['older_exports']:
     # Same durable export identifiers are reused after an unknown transfer.
     # Existing collect validates receipts before it can close anything.
     a.collect(vm,q,plan['paths'])
    a.collect(vm,plan['request_id'],plan['paths'],plan['snapshot_id']);out=a.finish(vm,plan['request_id'],plan['snapshot_id'])
  # Nomad termination is not a disk-deletion receipt. Existing backend retains
  # the disk; no new operator cleanup grants are introduced by this facade.
  return {'vm_id':vm,'state':out.get('phase','unknown'),'workload_stop_confirmed':out.get('phase')=='nomad_terminal','disk_deleted':False,'cleanup_status':'disk_deletion_not_confirmed_by_current_backend','results_verified':out.get('phase')=='nomad_terminal','prompt_retry':False}

def parser(sub):
 vm=sub.add_parser('vm',help='Owned VM-ID workflow; legacy commands stay available');commands=vm.add_subparsers(dest='vm_command',required=True)
 for name in ['create','connect','send','status','history','reconnect','retry','cancel','cancel-status','upload','upload-status','upload-reconcile','upload-abort','exec','wait','list','finish']:
  p=commands.add_parser(name);p.add_argument('--creator-record',help='Compatibility override; normally supplied by the native hook')
  if name not in ['create','list']:p.add_argument('vm_id')
  if name in ['history','reconnect','cancel','cancel-status']:p.add_argument('--request',help='Optional exact task selection; ambiguous tasks are never guessed')
  if name=='list':p.add_argument('--mine',action='store_true',default=True)
  if name=='send':p.add_argument('--text',required=True)
  if name=='wait':p.add_argument('--timeout',type=int,default=30)
  if name=='exec':p.add_argument('--timeout',type=int);p.add_argument('--cwd');p.add_argument('argv',nargs='*')
  if name in ('upload-status','upload-reconcile','upload-abort'):p.add_argument('upload_id',nargs='?',help='Optional selection when more than one transfer is ambiguous')
  if name=='upload':p.add_argument('local_path');p.add_argument('--destination',required=True);p.add_argument('--extract-to')
  if name=='finish':p.add_argument('paths',nargs='*',help='Declared result files to export before stopping')
  if name=='create':
   p.add_argument('--vm-id',help='Retry the exact same creation payload under its saved VM ID; changed input conflicts')
   for field in ['template','template-version','task-title']:p.add_argument('--'+field,required=True)
   for field in ['vcpus','memory-mib','disk-gib']:p.add_argument('--'+field,type=int,required=True)
   p.add_argument('--model');p.add_argument('--architecture',choices=['auto','amd64','arm64'],default='auto');p.add_argument('--project',default='');p.add_argument('--purpose',default='work');p.add_argument('--repository-profile')

def run(args,config,operations_config=None):
 service=VMs(CatalogClient(config),args.creator_record,operations_config=operations_config);command=args.vm_command
 if command=='create':return service.create({k:getattr(args,k)for k in ['template','template_version','task_title','vcpus','memory_mib','disk_gib','architecture','project','purpose','repository_profile','model']},args.vm_id)
 if command=='list':
  import status_ui
  return status_ui.listing(service)
 if command=='retry':return service.retry(args.vm_id,True)
 if command=='connect':return service.connect(args.vm_id)
 if command=='wait':
  import status_ui
  return status_ui.wait(service,args.vm_id,args.timeout)
 if command=='exec':return service.execute(args.vm_id,args.argv,args.timeout,args.cwd)
 if command in ('upload-status','upload-reconcile','upload-abort'):
  import file_transfer
  return file_transfer.control(service,args.vm_id,args.upload_id,command.split('-',1)[1] if command!='upload-status'else'status')
 if command=='upload':
  import file_transfer
  return file_transfer.upload(service,args.vm_id,args.local_path,args.destination,args.extract_to)
 if command=='send':return service.send(args.vm_id,args.text)
 if command=='status':return service.status(args.vm_id)
 if command in ('cancel','cancel-status'):return service.cancel(args.vm_id,args.request,command=='cancel-status')
 if command=='finish':return service.finish(args.vm_id,args.paths)
 stream=service.history(args.vm_id,args.request)if command=='history'else service.reconnect(args.vm_id,args.request)
 return stream
