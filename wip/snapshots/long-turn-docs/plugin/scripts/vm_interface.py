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
 def create(self,params):
  if any(type(params.get(k))is not int or params[k]<1 for k in ['vcpus','memory_mib','disk_gib']):raise ValueError('POSITIVE_VM_RESOURCES_REQUIRED')
  with self.intent('create',params,lambda:{'vm_id':str(uuid.uuid4())})as(row,fresh,path):
   vm=row['vm_id']
   if not fresh:return {'vm_id':vm,'state':row['phase'],'next_action':'status_or_connect','dispatch_repeated':False}
   try:
    self.client.request(vm,params['template'],params['template_version'],params.get('architecture','auto'),params['vcpus'],params['memory_mib'],params['disk_gib'],params.get('purpose','work'),params.get('project',''),creator_record=self.native.get('native_creator_record',self.record),model=params.get('model'),task_title=params['task_title'],source_agent_name=self.native.get('native_agent_name'),source_agent_type=self.native.get('native_agent_type'),repository_profile=params.get('repository_profile'))
    row['phase']='submitted';atomic(path,row)
   except Exception:
    row['phase']='create_unknown';atomic(path,row);return {'vm_id':vm,'state':'create_unknown','next_action':'status_or_connect','prompt_retry':False}
   return {'vm_id':vm,'state':'submitted','next_action':'connect','dispatch_repeated':False}
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
  a,out=self.resolve(vm)
  if a is None:return out
  p=a.local(vm)/'session.json'
  if not p.exists():return {**out,'session_state':'not_initialized','next_action':'connect'}
  state=a.load(vm)
  try:row=self.choose(a,vm);task_state=row.get('phase')if row else'none'
  except RuntimeError:task_state='ambiguous'
  return {**out,'session_state':state.get('phase'),'task_state':task_state,'readiness_evidence':'durable_session_receipt_and_nomad_not_model_probe','prompt_retry':False}
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
    yield {'vm_id':vm,'task':number,'entries':page['entries'],'terminal':page.get('terminal'),'history_complete':page.get('history_complete',False),'has_more':page['has_more']}
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
 def finish(self,vm,paths):
  a,status=self.resolve(vm)
  if a is None:raise RuntimeError('VM_LOCAL_BINDING_NOT_AVAILABLE')
  state=a.load(vm);candidate=state.get('closing_candidate')
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
     if selected is None or any(r.get('phase')!='terminal'for r in rows):raise RuntimeError('FINISH_REQUIRES_VERIFIED_TERMINAL_TASKS')
     if not paths:raise RuntimeError('DECLARED_RESULT_PATHS_REQUIRED_BEFORE_FINISH')
     plan={'vm_id':vm,'owner':self.me,'attempt':state.get('attempt'),'activity_revision':activity.revision(a,vm),'paths':paths,'request_id':selected['request_id'],'snapshot_id':str(uuid.uuid4()),'older_exports':[r['request_id']for r in rows if r['request_id']!=selected['request_id']and not r.get('export_verified')]};atomic(path,plan)
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
 for name in ['create','connect','send','status','history','reconnect','list','finish']:
  p=commands.add_parser(name);p.add_argument('--creator-record',help='Compatibility override; normally supplied by the native hook')
  if name not in ['create','list']:p.add_argument('vm_id')
  if name in ['history','reconnect']:p.add_argument('--request',help='Optional specific historical task; never needed for the ordinary workflow')
  if name=='list':p.add_argument('--mine',action='store_true',default=True)
  if name=='send':p.add_argument('--text',required=True)
  if name=='finish':p.add_argument('paths',nargs='*',help='Declared result files to export before stopping')
  if name=='create':
   for field in ['template','template-version','task-title']:p.add_argument('--'+field,required=True)
   for field in ['vcpus','memory-mib','disk-gib']:p.add_argument('--'+field,type=int,required=True)
   p.add_argument('--model');p.add_argument('--architecture',choices=['auto','amd64','arm64'],default='auto');p.add_argument('--project',default='');p.add_argument('--purpose',default='work');p.add_argument('--repository-profile')

def run(args,config,operations_config=None):
 service=VMs(CatalogClient(config),args.creator_record,operations_config=operations_config);command=args.vm_command
 if command=='create':return service.create({k:getattr(args,k)for k in ['template','template_version','task_title','vcpus','memory_mib','disk_gib','architecture','project','purpose','repository_profile','model']})
 if command=='list':return service.listing()
 if command=='connect':return service.connect(args.vm_id)
 if command=='send':return service.send(args.vm_id,args.text)
 if command=='status':return service.status(args.vm_id)
 if command=='finish':return service.finish(args.vm_id,args.paths)
 stream=service.history(args.vm_id,args.request)if command=='history'else service.reconnect(args.vm_id,args.request)
 return stream
