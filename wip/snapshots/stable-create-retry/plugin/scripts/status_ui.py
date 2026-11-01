"""Bounded parallel metadata/receipt snapshots, never watch or prompt replay."""
import copy,time,concurrent.futures,re

def bounded(service,deadline):
 clone=copy.copy(service);clone.client=copy.copy(service.client);original=service.client.run;factory=service.adapter_factory
 def remaining():
  left=deadline-time.monotonic()
  if left<=0:raise RuntimeError('VM_STATUS_DEADLINE_EXCEEDED')
  return left
 def run(args,input=None,timeout=None):return original(args,input=input,timeout=min(remaining(),timeout)if timeout else remaining())
 clone.client.run=run
 def adapter(*args):
  a=factory(*args);original_nomad=a.nomad
  def nomad(argv,**kw):kw['timeout']=min(remaining(),kw.get('timeout',remaining()));return original_nomad(argv,**kw)
  a.nomad=nomad;return a
 clone.adapter_factory=adapter;return clone

def read(service,vm,seconds=5,deadline=None):
 now=int(time.time());end=min(deadline,time.monotonic()+seconds)if deadline else time.monotonic()+seconds
 try:
  out=bounded(service,end)._status_snapshot(vm)
  return {**out,'observed_unix':now,'snapshot':True,'observation_timeout_seconds':seconds}
 except Exception as error:
  code=str(error);code=code if re.fullmatch('[A-Z][A-Z0-9_]{0,119}',code)else'VM_STATUS_UNAVAILABLE'
  return {'vm_id':vm,'state':'unknown','error':code,'observed_unix':now,'snapshot':True,'prompt_retry':False,'observation_timeout_seconds':seconds}

def listing(service,seconds=6):
 from catalog_cli import uid
 import urllib.parse
 start=time.monotonic();deadline=start+seconds;client=bounded(service,min(deadline,start+2)).client;ns=urllib.parse.quote(client.c['namespace'],safe='')
 try:
  jobs=client.api('/v1/jobs?namespace='+ns+'&prefix=worker-&meta=true&per_page=1025')
  if not isinstance(jobs,list)or len(jobs)>1024:raise RuntimeError('VM_DISCOVERY_LIMIT')
  ids=[]
  for row in jobs:
   meta=row.get('Meta')or{};vm=meta.get('worker_session_id')
   if row.get('Stop')or row.get('Status')=='dead':continue
   if any(meta.get(k)!=v for k,v in {'nomad_workers_owner':client.c['owner_id'],'creator_agent_id':service.me['agent_id'],'creator_session_id':service.me['native_session_id'],'creator_generation':service.me['generation']}.items()):continue
   if not vm or row.get('ID')!='worker-'+uid(vm):raise RuntimeError('VM_DISCOVERY_IDENTITY_CHANGED')
   ids.append(vm)
  if len(ids)>128 or len(ids)!=len(set(ids)):raise RuntimeError('VM_DISCOVERY_AMBIGUOUS_OR_LIMIT')
 except Exception as error:
  code=str(error);return {'vms':[],'inventory_state':'unknown','inventory_error':code if re.fullmatch('[A-Z][A-Z0-9_]{0,119}',code)else'VM_INVENTORY_UNAVAILABLE','observed_unix':int(time.time()),'from_nomad':True,'read_only':True}
 with concurrent.futures.ThreadPoolExecutor(max_workers=4)as pool:
  futures={vm:pool.submit(read,service,vm,4,deadline)for vm in ids};rows=[futures[vm].result()for vm in ids]
 return {'vms':rows,'scope':'mine','inventory_state':'observed','from_nomad':True,'read_only':True,'observed_unix':int(time.time()),'duration_seconds':round(time.monotonic()-start,3),'snapshot_not_watch':True,'unknown_is_not_running':True}


def work_items(a,vm):
 from exchange import protected
 import operations
 out=[]
 paths=sorted([*a.local(vm).glob('operation-*.json'),*a.local(vm).glob('exec-*.json'),*a.local(vm).glob('upload-*.json')])
 if len(paths)>128:return [{'state':'unknown','error':'VM_WORK_ITEM_LIMIT'}]
 for p in paths:
  row=protected(p);kind='operation'if p.name.startswith('operation-')else'upload'if p.name.startswith('upload-')else'legacy_exec'
  item={'kind':kind,'state':row.get('phase','unknown'),'evidence':'local_receipt','receipt_observed_unix':int(p.stat().st_mtime),'activity_revision':row.get('activity_revision')}
  if kind=='upload':item.update(upload_id=row.get('operation_id'),offset_bytes=row.get('offset'),total_bytes=row.get('source',{}).get('size'))
  out.append((p,row,item))
 # Only active/unknown work plus the latest verified revision is relevant to a
 # quick status. Historical log reading belongs to explicit events/history.
 active=[x for x in out if x[1].get('phase')in ('running','launching','launch_intent','start_unknown','unknown','exec_unknown')]
 if active:selected=active
 elif out:
  revisions=[x[1].get('activity_revision')for x in out]
  selected=[max(out,key=lambda x:x[1]['activity_revision'])]if all(type(r)is int for r in revisions)and len(set(revisions))==len(revisions)else out[-1:]if len(out)==1 else[]
 else:selected=[]
 if out and not selected:return [{'state':'unknown','error':'WORK_ITEM_ORDER_UNVERIFIED'}]
 if len(selected)>4:return [{'state':'unknown','error':'MULTIPLE_ACTIVE_WORK_ITEMS'}]
 result=[]
 for p,row,item in selected:
  if item['kind']=='operation':
   try:
    fresh=operations.snapshot(a,vm,row['operation_id']);item.update(state=fresh['phase'],evidence='fresh_owned_guest_operation',observed_unix=int(time.time()),exit_code=fresh.get('exit_code'))
   except Exception:item.update(state='unknown',error='OPERATION_SNAPSHOT_UNAVAILABLE')
  result.append(item)
 return result

def wait(service,vm,seconds=30):
 if type(seconds)is not int or not 1<=seconds<=1800:raise ValueError('VM_WAIT_TIMEOUT_MUST_BE_1_TO_1800_SECONDS')
 end=time.monotonic()+seconds
 while True:
  state=read(service,vm,seconds=min(5,max(.01,end-time.monotonic())),deadline=end)
  work=state.get('work_items',[]);unfinished=any(x.get('state')not in ('completed','failed','cancelled','timed_out','exec_completed','upload_completed','upload_aborted')for x in work)
  if state.get('error'):yield {**state,'wait_state':'unavailable','vm_retained':True};return
  if state.get('task_state')=='terminal'and not unfinished:yield {**state,'wait_state':'terminal_observed'};return
  if state.get('task_state')=='none'and work and not unfinished:yield {**state,'wait_state':'work_terminal_observed'};return
  if state.get('task_state')=='none'and not work:yield {**state,'wait_state':'no_current_work_observed'};return
  if time.monotonic()>=end:yield {**state,'wait_state':'deadline_reached','vm_retained':True,'cancellation_sent':False};return
  yield {**state,'wait_state':'observing_same_vm','cancellation_sent':False}
  time.sleep(min(1,max(0,end-time.monotonic())))
