"""One-shot native alloc exec; durable intent prevents replay after unknown outcomes."""
import json,subprocess,time
from secure_logs import Redactor
# This bounded one-shot process runs INSIDE the guest through native Nomad exec.
GUEST_RUNNER=r'''
import json,sys,subprocess,tempfile,os,signal
p=json.load(sys.stdin)
with tempfile.TemporaryFile() as out,tempfile.TemporaryFile() as err:
 child=subprocess.Popen(p['argv'],cwd=p['cwd'],stdin=subprocess.DEVNULL,stdout=out,stderr=err,start_new_session=True)
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
def execute(adapter,session,execution,argv,timeout=None,cwd=None):
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
