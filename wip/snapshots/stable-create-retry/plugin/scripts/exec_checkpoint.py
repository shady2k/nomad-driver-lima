"""Typed no-agent-turn checkpoint; ownership is independent of activity type."""
import json,pathlib,base64,tempfile,os,re
from exchange import protected

def project_binding(a,s,meta):
 file=a.local(s)/'allocation-intent.json'
 if file.exists():
  intent=protected(file)
  if 'project'in intent and meta.get('project','')!=intent['project']:raise RuntimeError('CHECKPOINT_PROJECT_BINDING_CHANGED')

def verify_owner(a,s,live):
 a.require_initiator(s);meta=live.get('Job',{}).get('Meta')or{}
 for k,v in {'worker_session_id':s,'creator_agent_id':a.c.get('agent_instance_id'),'creator_session_id':a.c.get('creator_session_id'),'creator_generation':a.c.get('creator_generation')}.items():
  if not v or meta.get(k)!=v:raise RuntimeError('CHECKPOINT_OWNER_CHANGED')
 project_binding(a,s,meta)

def local_idle(a,s):
 import activity,operations
 a.require_initiator(s);state=a.load(s);live=a.target()
 if a.attempt(live)!=state.get('attempt'):raise RuntimeError('CHECKPOINT_ATTEMPT_CHANGED')
 meta=live.get('Job',{}).get('Meta')or{}
 for k,v in {'worker_session_id':s,'creator_agent_id':a.c.get('agent_instance_id'),'creator_session_id':a.c.get('creator_session_id'),'creator_generation':a.c.get('creator_generation')}.items():
  if not v or meta.get(k)!=v:raise RuntimeError('CHECKPOINT_OWNER_CHANGED')
 project_binding(a,s,meta)
 activity.pending(a,s)
 for p,row in activity.records(a,s):
  if row.get('request_id'):raise RuntimeError('CHECKPOINT_AGENT_TURN_PRESENT_USE_TURN_FINISH')
  if row.get('execution_id')and row.get('phase')!='exec_completed':raise RuntimeError('CHECKPOINT_EXEC_NOT_KNOWN_TERMINAL')
 operations.verify_terminal(a,s)
 return state

def ensure_session(a,s):
 from workerctl import atomic
 a.require_initiator(s);file=a.local(s)/'session.json'
 if file.exists():return a.load(s)
 live=a.target();meta=live.get('Job',{}).get('Meta')or{}
 for k,v in {'worker_session_id':s,'creator_agent_id':a.c.get('agent_instance_id'),'creator_session_id':a.c.get('creator_session_id'),'creator_generation':a.c.get('creator_generation')}.items():
  if not v or meta.get(k)!=v:raise RuntimeError('CHECKPOINT_OWNER_CHANGED')
 project_binding(a,s,meta)
 state={'session_id':s,'config_hash':a.hash,'phase':'management_ready','attempt':a.attempt(live),'work_kind':'exec_only','actual_backend':'lima/nomad-exec/management-no-agent-turns'};atomic(file,state);return state

def remote(a,s,action,**fields):
 state=a.load(s);a.require_initiator(s);live=a.target()
 if a.attempt(live)!=state.get('attempt'):raise RuntimeError('CHECKPOINT_ATTEMPT_CHANGED')
 binding={'session_id':s,'attempt':state['attempt'],'creator_agent_id':a.c['agent_instance_id'],'creator_session_id':a.c['creator_session_id'],'creator_generation':a.c['creator_generation']}
 known=[protected(p)['operation_id']for p in a.local(s).glob('operation-*.json')]
 known_uploads=[protected(p)['operation_id']for p in a.local(s).glob('upload-*.json')]
 payload={'config':a.c['guest'],'session_id':s,'binding':binding,'known_operation_ids':sorted(known),'known_upload_ids':sorted(known_uploads),'action':action,**fields}
 program=pathlib.Path(__file__).with_name('checkpoint_guest.mjs').read_text()+"\nlet raw='';for await(const b of process.stdin){raw+=b;if(Buffer.byteLength(raw)>2097152)throw Error('CHECKPOINT_INPUT_BOUND')}try{console.log(JSON.stringify(run(JSON.parse(raw))))}catch(e){console.log(JSON.stringify({type:'error',code:/^[A-Z_]+$/.test(e.code??'')?e.code:'CHECKPOINT_REMOTE_ERROR'}));process.exitCode=1}\n"
 args=['alloc','exec','-namespace='+a.c['namespace'],'-task='+a.c['task'],'-i=true','-t=false',a.c['allocation'],a.c.get('guest_node','/usr/bin/node'),'--input-type=module','-e',program]
 result=a.nomad(args,input=json.dumps(payload),text=True,timeout=40)
 if len(result.stdout)>100000000:raise RuntimeError('CHECKPOINT_RESPONSE_BOUND')
 try:value=json.loads(result.stdout)
 except ValueError:raise RuntimeError('CHECKPOINT_READ_UNAVAILABLE')from None
 if value.get('type')=='error':raise RuntimeError(value['code'])
 if result.returncode:raise RuntimeError('CHECKPOINT_TRANSPORT_UNKNOWN_VM_RETAINED')
 if fields.get('machine_identity')and value.get('machine_identity')!=fields['machine_identity']:raise RuntimeError('CHECKPOINT_MACHINE_BOOT_CHANGED')
 return value

def collect(a,s,paths,snapshot):
 from workerctl import uid,atomic,canonical
 import activity
 snapshot=uid(snapshot)
 with a.lock(s):
  state=a.load(s)
  if state.get('phase')not in ('ready','management_ready'):raise RuntimeError('SESSION_NOT_READY_OR_FINISHING')
  rf=a.export_record(s,None,snapshot)
  if rf.exists():
   row=protected(rf)
   if row.get('export_verified'):
    manifest=a.result(s,None,snapshot)
    if manifest.get('checkpoint')!=activity.checkpoint(a,s,None,snapshot):raise RuntimeError('HISTORICAL_CHECKPOINT_USE_NEW_SNAPSHOT')
    return manifest
   activity.pending(a,s,exclude=rf)
  else:local_idle(a,s);row={'session_id':s,'request_id':None,'acpx_request_id':None,'terminal':None,'work_kind':'exec_only','phase':'terminal'}
  if row.get('work_kind')!='exec_only'or row.get('request_id')is not None:raise RuntimeError('CHECKPOINT_KIND_CHANGED')
  if not isinstance(paths,list)or len(paths)>64:raise ValueError('BOUNDED_ARTIFACT_LIST_REQUIRED')
  if row.get('declared_paths')is not None and row['declared_paths']!=paths:raise RuntimeError('EXPORT_DECLARATION_CHANGED')
  proof=remote(a,s,'probe',**({'machine_identity':row['machine_identity']}if row.get('machine_identity')else{}))
  if state.get('management_machine_identity')and state['management_machine_identity']!=proof['machine_identity']:raise RuntimeError('CHECKPOINT_MACHINE_BOOT_CHANGED')
  state['management_machine_identity']=proof['machine_identity'];state['management_checkpoint_mode']=True;atomic(a.local(s)/'session.json',state)
  cp=activity.checkpoint(a,s,None,snapshot)
  if row.get('checkpoint')is not None and row['checkpoint']!=cp:raise RuntimeError('EXPORT_REVISION_CHANGED_VM_RETAINED')
  row.update(machine_identity=proof['machine_identity'],checkpoint=cp,declared_paths=paths,phase='exporting',export_verified=False)
  if not row.get('export_staging_dir'):row['export_staging_dir']=pathlib.Path(tempfile.mkdtemp(prefix='management-',dir=a.local(s))).name
  row['manifest_dir']=row['export_staging_dir'];atomic(rf,row)
  try:return a._collect_locked(s,None,paths,rf,row)
  except Exception:row['phase']='export_failed_retryable';atomic(rf,row);raise
