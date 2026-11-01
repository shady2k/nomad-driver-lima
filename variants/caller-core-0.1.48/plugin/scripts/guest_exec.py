"""One-shot native alloc exec; durable intent prevents replay after unknown outcomes."""
import json,subprocess,time,pathlib
from secure_logs import Redactor
# This bounded one-shot process runs INSIDE the guest through native Nomad exec.
GUEST_RUNNER=(pathlib.Path(__file__).parents[1]/'guest/repository_exec.py').read_text()+r'''
import json,sys,subprocess,tempfile,os,signal
p=json.load(sys.stdin)
with tempfile.TemporaryFile() as out,tempfile.TemporaryFile() as err:
 child=subprocess.Popen(repository_command(p['argv']),cwd=p['cwd'],stdin=subprocess.DEVNULL,stdout=out,stderr=err,start_new_session=True)
 timed=False
 try:child.wait(timeout=p['timeout'])
 except subprocess.TimeoutExpired:
  timed=True;os.killpg(child.pid,signal.SIGTERM)
  try:child.wait(timeout=3)
  except subprocess.TimeoutExpired:os.killpg(child.pid,signal.SIGKILL);child.wait()
 result={'version':1,'outcome':'timeout_effects_unknown' if timed else 'completed','exit_code':child.returncode}
 for key,f in [('stdout',out),('stderr',err)]:
  f.seek(0);raw=f.read(1048577);result[key]=raw[:1048576].decode('utf-8','replace');result[key+'_truncated']=len(raw)>1048576
 print(json.dumps(result))
'''
def legacy_execute(adapter,session,execution,argv,timeout=None,cwd=None):
 from workerctl import uid,canonical,digest,atomic
 uid(execution)
 if not isinstance(argv,list) or not argv or any(not isinstance(v,str) or '\0' in v for v in argv) or sum(len(v) for v in argv)>65536:raise ValueError('BOUNDED_GUEST_ARGV_REQUIRED')
 from operation_policy import load
 policy=load(adapter.operations_policy_file)
 timeout=policy['foreground_timeout_seconds'] if timeout is None else timeout
 if type(timeout)!=int or not 1<=timeout<=policy['max_timeout_seconds']:raise ValueError('EXEC_DEADLINE_EXCEEDS_OPERATOR_POLICY')
 cwd=cwd or adapter.c['guest']['workspace']
 if not isinstance(cwd,str) or not cwd.startswith('/') or '\0' in cwd:raise ValueError('ABSOLUTE_GUEST_CWD_REQUIRED')
 with adapter.lock(session):
  state=adapter.load(session);adapter.require_initiator(session)
  if state.get('phase')!='ready':raise RuntimeError('SESSION_NOT_READY_OR_FINISHING')
  allocation=adapter.target();meta=allocation['Job']['Meta']
  required={'creator_agent_id':adapter.c.get('agent_instance_id'),'creator_session_id':adapter.c.get('creator_session_id'),'creator_generation':adapter.c.get('creator_generation'),'worker_session_id':session}
  if any(not v or meta.get(k)!=v for k,v in required.items()):raise RuntimeError('EXEC_SESSION_OWNER_MISMATCH')
  if adapter.attempt(allocation)!=state.get('attempt'):raise RuntimeError('TARGET_ATTEMPT_CHANGED_NO_REPLAY')
  task=allocation.get('TaskStates',{}).get(adapter.c['task'],{})
  if allocation.get('ClientStatus')!='running' or task.get('State')!='running':raise RuntimeError('EXEC_TASK_NOT_RUNNING')
  path=adapter.local(session)/('exec-'+execution+'.json');fingerprint=digest(canonical({'argv':argv,'cwd':cwd,'timeout':timeout}))
  if path.exists():
   old=json.loads(path.read_text())
   if old['command_sha256']!=fingerprint:raise RuntimeError('EXEC_CONTENT_CHANGED')
   return old # Even intent/unknown is lookup-only: NEVER replay a command.
  receipt={'execution_id':execution,'session_id':session,'command_sha256':fingerprint,'attempt':state['attempt'],'phase':'exec_unknown','prompt_retry':False,'started_at':time.time()}
  import activity
  activity.record(adapter,session,path,receipt)
  adapter.event(session,'guest_exec_started')
  args=['alloc','exec','-namespace='+adapter.c['namespace'],'-task='+adapter.c['task'],'-i=true','-t=false',adapter.c['allocation'],'/usr/bin/python3','-c',GUEST_RUNNER]
  try:
   result=adapter.nomad(args,input=json.dumps({'argv':argv,'cwd':cwd,'timeout':timeout}),text=True,timeout=timeout+20)
   if result.returncode:raise RuntimeError('EXEC_TRANSPORT_OUTCOME_UNKNOWN_NO_RETRY')
   if len(result.stdout)>8388608:raise ValueError('execution frame too large')
   frame=json.loads(result.stdout)
   if frame.get('version')!=1 or frame.get('outcome') not in ('completed','timeout_effects_unknown') or type(frame.get('exit_code'))!=int:raise ValueError('invalid execution frame')
   redactor=Redactor()
   for name in ('stdout','stderr'):
    if not isinstance(frame.get(name),str) or len(frame[name].encode())>4194304:raise ValueError('invalid bounded output')
    frame[name]=redactor.text(frame[name])
   receipt.update(frame,phase='exec_completed' if frame['outcome']=='completed' else 'exec_unknown',finished_at=time.time());atomic(path,receipt)
  except Exception:
   try:adapter.event(session,'guest_exec_unknown')
   except Exception:pass
   raise RuntimeError('EXEC_OUTCOME_UNKNOWN_NO_RETRY_RECEIPT_RETAINED') from None
  try:adapter.event(session,'guest_exec_completed' if receipt['phase']=='exec_completed' else 'guest_exec_unknown')
  except Exception:receipt['native_stage_log_unconfirmed']=True;atomic(path,receipt)
  return receipt

def execute(adapter,session,execution,argv,timeout=None,cwd=None):
 """Foreground convenience over the SAME durable managed-operation protocol."""
 from workerctl import uid
 import operations
 uid(execution)
 legacy=adapter.local(session)/('exec-'+execution+'.json')
 if legacy.exists() or legacy.is_symlink():
  # Historical IDs retain their exact old fingerprint and lookup-only semantics.
  return legacy_execute(adapter,session,execution,argv,timeout,cwd)
 row=operations.start(adapter,session,execution,argv,'guest-command',timeout,cwd)
 policy=row['policy'];seconds=min(policy['foreground_timeout_seconds'],policy['max_timeout_seconds'])
 for row in operations.wait(adapter,session,execution,seconds):pass
 result={**row,'execution_id':execution,'protocol':'managed-operation','next_action':'operation-status','command_replayed':False}
 if row['phase'] not in operations.TERMINAL:
  result.update(operation_continues=True,outcome='running' if row['phase'] in ('running','launching','launch_intent') else 'unknown')
  return result
 # Log reads are cursor-based and independently repeatable; never relaunch work.
 output={'stdout':[],'stderr':[]};cursor=0
 while True:
  page=operations.logs(adapter,session,execution,cursor)
  for event in page['events']:
   if event.get('stream') in output:output[event['stream']].append(event['text'])
  if page['eof']:break
  if page['cursor']<=cursor:raise RuntimeError('OPERATION_LOG_CURSOR_NO_PROGRESS')
  cursor=page['cursor']
 result.update({k:''.join(v)for k,v in output.items()})
 result.update(exit_code=row['state'].get('exit_code'),outcome=row['phase'],output_truncated=row['state'].get('output_truncated',False),operation_continues=False,next_action='operation-logs')
 return result

# Legacy exec protocol has no durable guest result/PID receipt. This explicit
# operator acknowledgement does not pretend to recover or cancel that command.
def _owned_ready(adapter,session):
 adapter.require_initiator(session);state=adapter.load(session)
 if state.get('phase')!='ready' or state.get('closing_candidate'):raise RuntimeError('EXEC_ABANDON_SESSION_NOT_READY')
 allocation=adapter.target();meta=allocation['Job']['Meta']
 required={'creator_agent_id':adapter.c.get('agent_instance_id'),'creator_session_id':adapter.c.get('creator_session_id'),'creator_generation':adapter.c.get('creator_generation'),'worker_session_id':session}
 if any(not v or meta.get(k)!=v for k,v in required.items()):raise RuntimeError('EXEC_SESSION_OWNER_MISMATCH')
 if adapter.attempt(allocation)!=state.get('attempt'):raise RuntimeError('TARGET_ATTEMPT_CHANGED_NO_REPLAY')
 if allocation.get('ClientStatus')!='running'or allocation.get('TaskStates',{}).get(adapter.c['task'],{}).get('State')!='running':raise RuntimeError('EXEC_TASK_NOT_RUNNING')
 return state,required

def _receipt(adapter,session,execution):
 from workerctl import uid
 import exchange
 uid(execution);path=adapter.local(session)/('exec-'+execution+'.json');row=exchange.protected(path)
 if row.get('session_id')!=session or row.get('execution_id')!=execution:raise RuntimeError('EXEC_RECEIPT_IDENTITY_MISMATCH')
 return path,row

def _identity(adapter,session):
 import operations
 value=operations.remote(adapter,session,{**operations.base(adapter,session),'action':'probe'})
 identity={k:value.get(k)for k in ('machine_id','boot_id')}
 if any(not isinstance(v,str)or not v or len(v)>128 for v in identity.values()):raise RuntimeError('EXEC_CURRENT_GUEST_IDENTITY_UNAVAILABLE')
 return identity

def status(adapter,session,execution):
 from workerctl import canonical,digest
 adapter.require_initiator(session);adapter.load(session)
 if not (adapter.local(session)/('exec-'+execution+'.json')).exists():
  import operations
  return {**operations.status(adapter,session,execution),'execution_id':execution,'protocol':'managed-operation','command_replayed':False}
 path,row=_receipt(adapter,session,execution)
 return {**row,'receipt_sha256':digest(canonical(row)),'legacy_guest_result_available':False,'legacy_cancel_supported':False,'command_replayed':False}

def abandon_plan(adapter,session,execution):
 from workerctl import canonical,digest
 with adapter.lock(session):
  state,creator=_owned_ready(adapter,session);path,row=_receipt(adapter,session,execution)
  if row.get('phase')!='exec_unknown'or row.get('attempt')!=state.get('attempt'):raise RuntimeError('EXEC_UNKNOWN_CURRENT_ATTEMPT_REQUIRED')
  now=time.time()
  return {'schema_version':1,'action':'acknowledge_legacy_read_only_exec_unknown','session_id':session,'execution_id':execution,'allocation_id':adapter.c['allocation'],'attempt':state['attempt'],'creator':creator,'command_sha256':row['command_sha256'],'receipt_sha256':digest(canonical(row)),'guest_identity':dict(_identity(adapter,session)),'observed_unix':now,'expires_unix':now+300,'read_only_command':False,'no_related_processes_observed':False,'acknowledge_unknown_outcome':False,'allow_new_activity':False,'command_replay_allowed':False,'original_guest_identity_recorded':False}

def abandon(adapter,session,execution,approval_file):
 from workerctl import canonical,digest,atomic
 import activity,exchange,re
 approval=exchange.protected(approval_file)
 with adapter.lock(session):
  state,creator=_owned_ready(adapter,session);path,row=_receipt(adapter,session,execution);audit_path=adapter.local(session)/('exec-'+execution+'.abandon.json')
  # An acknowledged action is lookup-only, even if its original approval expired.
  if row.get('phase')=='exec_abandoned':
   audit=exchange.protected(audit_path)
   if row.get('abandon_audit_sha256')!=digest(canonical(audit))or audit.get('execution_id')!=execution or audit.get('session_id')!=session:raise RuntimeError('EXEC_ABANDON_AUDIT_MISMATCH')
   return row
  if row.get('phase')!='exec_unknown'or row.get('attempt')!=state.get('attempt'):raise RuntimeError('EXEC_UNKNOWN_CURRENT_ATTEMPT_REQUIRED')
  expected={'schema_version':1,'action':'acknowledge_legacy_read_only_exec_unknown','session_id':session,'execution_id':execution,'allocation_id':adapter.c['allocation'],'attempt':state['attempt'],'creator':creator,'command_sha256':row.get('command_sha256'),'receipt_sha256':digest(canonical(row)),'read_only_command':True,'no_related_processes_observed':True,'acknowledge_unknown_outcome':True,'allow_new_activity':True,'command_replay_allowed':False,'original_guest_identity_recorded':False}
  if set(approval)!=set(expected)|{'guest_identity','observed_unix','expires_unix'}or any(approval.get(k)!=v for k,v in expected.items()):raise RuntimeError('EXACT_EXEC_ABANDON_APPROVAL_REQUIRED')
  now=time.time();observed=approval.get('observed_unix');expires=approval.get('expires_unix')
  if type(observed)not in (int,float)or type(expires)not in (int,float)or not now-300<=observed<=now+2 or not now<expires<=now+600:raise RuntimeError('EXEC_ABANDON_APPROVAL_EXPIRED')
  identity=_identity(adapter,session)
  if approval['guest_identity']!=identity:raise RuntimeError('EXEC_ABANDON_MACHINE_BOOT_CHANGED')
  approval_sha=digest(canonical(approval));original_sha=digest(canonical(row))
  if audit_path.exists()or audit_path.is_symlink():
   audit=exchange.protected(audit_path)
   if audit.get('original_receipt_sha256')!=original_sha or audit.get('approval_sha256')!=approval_sha or audit.get('attempt')!=state['attempt']:raise RuntimeError('EXEC_ABANDON_AUDIT_CONFLICT')
  else:
   audit={'schema_version':1,'event':'legacy_read_only_exec_unknown_acknowledged','phase':'exec_abandon_audit','session_id':session,'execution_id':execution,'attempt':state['attempt'],'creator':creator,'command_sha256':row['command_sha256'],'original_receipt_sha256':original_sha,'original_receipt':row,'approval_sha256':approval_sha,'approval':approval,'guest_identity':identity,'activity_revision':activity.revision(adapter,session)+1,'acknowledged_at':now,'command_outcome':'unknown','command_replayed':False,'command_cancelled':False}
   atomic(audit_path,audit)
  updated={**row,'phase':'exec_abandoned','outcome':'unknown_acknowledged_not_success','abandon_audit_sha256':digest(canonical(audit)),'original_receipt_sha256':original_sha,'command_outcome':'unknown','result_recovered':False,'command_replayed':False,'command_cancelled':False,'prompt_retry':False}
  atomic(path,updated)
  state['activity_revision']=max(state.get('activity_revision',0),audit['activity_revision']);state.pop('last_checkpoint',None);atomic(adapter.local(session)/'session.json',state)
  try:adapter.event(session,'guest_exec_unknown_acknowledged_no_replay',execution)
  except Exception:pass
  return updated
