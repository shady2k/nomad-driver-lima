"""Managed finite operations over native exec. No prompt retry/control service."""
import base64,hashlib,inspect,json,pathlib,re,time
from secure_logs import Redactor
from operation_policy import load as load_policy
TERMINAL={'completed','failed','cancelled','timed_out'}

def program():
 return 'import json,re,urllib.parse\n'+inspect.getsource(Redactor)+'\n'+(pathlib.Path(__file__).parents[1]/'guest/repository_exec.py').read_text()+'\n'+pathlib.Path(__file__).with_name('operation_guest.py').read_text()

def checked(a,s,write=False):
 state=a.load(s);a.require_initiator(s);row=a.target();meta=row['Job']['Meta']
 for k,v in {'creator_agent_id':a.c.get('agent_instance_id'),'creator_session_id':a.c.get('creator_session_id'),'creator_generation':a.c.get('creator_generation'),'worker_session_id':s}.items():
  if not v or meta.get(k)!=v:raise RuntimeError('OPERATION_OWNER_SESSION_GENERATION_MISMATCH')
 if a.attempt(row)!=state.get('attempt'):raise RuntimeError('OPERATION_ATTEMPT_CHANGED_NO_REPLAY')
 if row.get('ClientStatus')!='running' or row.get('TaskStates',{}).get(a.c['task'],{}).get('State')!='running':raise RuntimeError('OPERATION_TASK_NOT_RUNNING')
 if write:
  if state.get('phase')not in ('ready','management_ready'):raise RuntimeError('SESSION_NOT_READY_OR_FINISHING')
 return state

def remote(a,s,p):
 checked(a,s)
 args=['alloc','exec','-namespace='+a.c['namespace'],'-task='+a.c['task'],'-i=true','-t=false',a.c['allocation'],'/usr/bin/python3','-c',program()]
 try:
  r=a.nomad(args,input=json.dumps(p),text=True,timeout=25)
  if len(r.stdout)>2097152:raise ValueError('frame quota')
  v=json.loads(r.stdout)
  if not v.get('ok'):
   code=v.get('code','OPERATION_REMOTE_ERROR');raise RuntimeError(code if re.fullmatch('[A-Z_]{1,100}',code) else 'OPERATION_REMOTE_ERROR')
  if r.returncode:raise ValueError('transport exit')
  return v['result']
 except RuntimeError:raise
 except Exception:raise RuntimeError('OPERATION_TRANSPORT_OUTCOME_UNKNOWN_NO_REPLAY') from None

def base(a,s):
 result={'state_dir':a.c['guest']['state_dir'],'session_id':s,'guest_config':a.c['guest']}
 if a.load(s).get('phase')=='management_ready':result.update(allow_management_session=True,owner_binding={'session_id':s,'attempt':a.load(s)['attempt'],'creator_agent_id':a.c['agent_instance_id'],'creator_session_id':a.c['creator_session_id'],'creator_generation':a.c['creator_generation']})
 return result
def path(a,s,op):
 from workerctl import uid
 return a.local(s)/('operation-'+uid(op)+'.json')
def payload(a,s,row,action,**kw):return {**base(a,s),'operation_id':row['operation_id'],'binding':row['binding'],'machine_identity':row['machine_identity'],'command_sha256':row['command_sha256'],'action':action,**kw}
def save(a,s,op,row):
 from workerctl import atomic
 atomic(path(a,s,op),row)
def fresh(a,s,op):
 row=json.loads(path(a,s,op).read_text());v=remote(a,s,payload(a,s,row,'status'))
 if v['binding']!=row['binding'] or v['command_sha256']!=row['command_sha256']:raise RuntimeError('OPERATION_RECEIPT_BINDING_MISMATCH')
 row.update(state=v,phase=v['phase']);save(a,s,op,row);return row

def start(a,s,op,argv,label,timeout=None,cwd=None):
 from workerctl import uid,canonical,digest
 uid(op);policy=load_policy(a.operations_policy_file)
 timeout=policy['default_timeout_seconds'] if timeout is None else timeout
 if type(timeout)!=int or not 1<=timeout<=policy['max_timeout_seconds']:raise ValueError('OPERATION_DEADLINE_EXCEEDS_OPERATOR_POLICY')
 if not re.fullmatch('[a-z][a-z0-9-]{0,23}',label) or any(x in label for x in ['secret','token','password','bearer']):raise ValueError('SECRET_FREE_OPERATION_LABEL_REQUIRED')
 if not isinstance(argv,list) or not argv or any(not isinstance(v,str) or '\0' in v for v in argv) or sum(len(v) for v in argv)>65536:raise ValueError('BOUNDED_GUEST_ARGV_REQUIRED')
 cwd=cwd or a.c['guest']['workspace']
 if not isinstance(cwd,str) or not cwd.startswith('/') or '\0' in cwd:raise ValueError('ABSOLUTE_GUEST_CWD_REQUIRED')
 if policy['max_log_bytes']>a.c['guest']['max_artifact_bytes']:raise ValueError('OPERATION_LOG_BOUND_EXCEEDS_ARTIFACT_LIMIT')
 fingerprint=digest(canonical({'argv':argv,'label':label,'cwd':cwd,'timeout':timeout}))
 with a.lock(s):
  state=checked(a,s,True);f=path(a,s,op)
  if f.exists():
   row=json.loads(f.read_text())
   if row['command_sha256']!=fingerprint:raise RuntimeError('OPERATION_CONTENT_CHANGED_NO_REPLAY')
   return fresh(a,s,op) # Always read, never another start request.
  probe=remote(a,s,{**base(a,s),'action':'probe'})
  binding={'session_id':s,'attempt':state['attempt'],'creator_agent_id':a.c['agent_instance_id'],'creator_session_id':a.c['creator_session_id'],'creator_generation':a.c['creator_generation']}
  identity={k:probe[k] for k in ['machine_id','boot_id']}
  row={'operation_id':op,'work_kind':'managed_operation','label':label,'binding':binding,'machine_identity':identity,'command_sha256':fingerprint,'phase':'start_unknown','policy':policy,'native_log_schema':probe['native_log_schema'],'prompt_retry':False,'log_cursor':0}
  import activity
  # Only a new, owner-checked explicit command is an activity observation.
  # Polling/reconnect never proves the initiating agent alive or renews this lease.
  import lease
  observed=lease.create(a.local(s),a.c.get('agent_instance_id',a.c['owner_id']),s,state['attempt'])
  activity.record(a,s,path(a,s,op),row)
  lease.observe(a.local(s),observed['owner'],s,observed['attempt'],observed['generation'],observed['sequence']+1,op)
  try:
   v=remote(a,s,payload(a,s,row,'start',argv=argv,cwd=cwd,label=label,timeout=timeout,policy=policy,runner_source=program()));row.update(state=v,phase=v['phase']);save(a,s,op,row);return row
  except Exception:
   save(a,s,op,row);raise

def snapshot(a,s,op):
 """One bounded read when adapter transport has a deadline; no lock/wait/write."""
 from exchange import protected
 row=protected(path(a,s,op));value=remote(a,s,payload(a,s,row,'status'))
 if value.get('binding')!=row['binding']or value.get('command_sha256')!=row['command_sha256']:raise RuntimeError('OPERATION_RECEIPT_BINDING_MISMATCH')
 return value

def status(a,s,op):
 with a.lock(s):checked(a,s);return fresh(a,s,op)
def logs(a,s,op,cursor=None):
 with a.lock(s):
  checked(a,s);row=json.loads(path(a,s,op).read_text());cursor=row.get('log_cursor',0) if cursor is None else cursor
  out=remote(a,s,payload(a,s,row,'logs',cursor=cursor));raw=base64.b64decode(out['data'],validate=True)
  if out['cursor']!=cursor+len(raw):raise RuntimeError('OPERATION_LOG_CURSOR_MISMATCH')
  events=[];redactor=Redactor()
  for line in raw.splitlines():
   event=json.loads(line);event['text']=redactor.text(event['text']);events.append(event)
  row.update(state=out['state'],phase=out['state']['phase'],log_cursor=out['cursor']);save(a,s,op,row)
  return {'operation_id':op,'cursor':out['cursor'],'eof':out['eof'],'phase':row['phase'],'events':events,'state':out['state']}
def wait(a,s,op,seconds=30):
 if type(seconds)!=int or not 1<=seconds<=604800:raise ValueError('BOUNDED_WAIT_DEADLINE_REQUIRED')
 start=time.monotonic();end=start+seconds;row=status(a,s,op)
 if seconds>row['policy']['max_timeout_seconds']:raise ValueError('WAIT_DEADLINE_EXCEEDS_OPERATION_POLICY')
 while True:
  elapsed=max(0,time.monotonic()-start)
  if row['phase']in TERMINAL|{'unknown'}:
   yield {**row,'wait_status':'terminal'if row['phase']in TERMINAL else'unknown','wait_elapsed_seconds':elapsed};return
  if time.monotonic()>=end:
   yield {**row,'wait_status':'deadline_reached','wait_elapsed_seconds':elapsed,'operation_continues':True,'next_action':'operation-status'};return
  yield {**row,'wait_status':'observing','wait_elapsed_seconds':elapsed}
  time.sleep(min(2,max(0,end-time.monotonic())))
  row=status(a,s,op) # bounded read-only transport; never start/cancel/replay
def cancel(a,s,op):
 with a.lock(s):
  checked(a,s);row=json.loads(path(a,s,op).read_text());out=remote(a,s,payload(a,s,row,'cancel'));row.update(state=out,phase=out['phase']);save(a,s,op,row);return row

def verify_terminal(a,s):
 for p in sorted(a.local(s).glob('operation-*.json')):
  row=fresh(a,s,json.loads(p.read_text())['operation_id'])
  if row['phase'] not in TERMINAL:raise RuntimeError('RUNNING_OR_UNKNOWN_OPERATION_VM_RETAINED')
  if row['state'].get('output_truncated'):raise RuntimeError('INCOMPLETE_OPERATION_LOGS_VM_RETAINED')

def capture(a,s,output):
 import diagnostics
 items=[]
 for p in sorted(a.local(s).glob('operation-*.json')):
  row=json.loads(p.read_text());op=row['operation_id'];first=remote(a,s,payload(a,s,row,'status'))
  if first['phase'] not in TERMINAL or first.get('output_truncated'):raise RuntimeError('OPERATION_EXPORT_NOT_COMPLETE')
  raw=b'';cursor=0
  while True:
   page=remote(a,s,payload(a,s,row,'logs',cursor=cursor));data=base64.b64decode(page['data'],validate=True);raw+=data
   if len(raw)>row['policy']['max_log_bytes'] or page['cursor']!=cursor+len(data):raise RuntimeError('OPERATION_EXPORT_LOG_BOUND')
   cursor=page['cursor']
   if page['eof']:break
   if not data:raise RuntimeError('OPERATION_EXPORT_LOG_GAP')
  last=remote(a,s,payload(a,s,row,'status'))
  if last['log_sha256']!=first['log_sha256'] or hashlib.sha256(raw).hexdigest()!=last['log_sha256'] or len(raw)!=last['log_bytes']:raise RuntimeError('OPERATION_EXPORT_CHECKSUM_CHANGED')
  # Re-redact before export; never persist the command or general environment.
  safe=Redactor().text(raw.decode()).encode();items.append(diagnostics.write_file(output,'operation-'+op+'.log',safe))
  items.append(diagnostics.write_file(output,'operation-'+op+'.json',json.dumps(last,sort_keys=True).encode()))
 return items
