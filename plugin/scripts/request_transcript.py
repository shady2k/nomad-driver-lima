"""Read-only transcript pages and ownership-scoped ephemeral watch transport."""
import json,pathlib,subprocess,re

def program():
 return pathlib.Path(__file__).with_name('request_observer_guest.mjs').read_text()+'\nawait main();\n'

def checked(a,s):
 state=a.load(s)
 if a.c.get('creator_record'):a.require_initiator(s)
 row=a.target()
 if state.get('attempt') is None or a.attempt(row)!=state['attempt']:raise RuntimeError('REQUEST_OBSERVER_ATTEMPT_CHANGED_NO_REPLAY')
 if a.c.get('creator_record'):
  meta=row['Job'].get('Meta',{})
  for key,value in {'creator_agent_id':a.c.get('agent_instance_id'),'creator_session_id':a.c.get('creator_session_id'),'creator_generation':a.c.get('creator_generation'),'worker_session_id':s}.items():
   if not value or meta.get(key)!=value:raise RuntimeError('REQUEST_OBSERVER_OWNER_GENERATION_CHANGED')
 if row.get('ClientStatus')!='running' or row.get('TaskStates',{}).get(a.c['task'],{}).get('State')!='running':raise RuntimeError('REQUEST_OBSERVER_GUEST_UNAVAILABLE_USE_RETAINED_RESULTS')
 return state

def request(a,s,r):
 from workerctl import uid
 uid(s);uid(r)
 state=checked(a,s)
 local=json.loads((a.local(s)/(r+'.json')).read_text())
 if local.get('session_id')!=s or local.get('request_id')!=r:raise RuntimeError('REQUEST_OBSERVER_LOCAL_BINDING_CHANGED')
 prompt_hash=local.get('prompt_sha256')
 if not isinstance(prompt_hash,str) or not re.fullmatch('[a-f0-9]{64}',prompt_hash):raise RuntimeError('REQUEST_OBSERVER_PROMPT_RECEIPT_REQUIRED')
 native=local.get('acpx_request_id')
 if native is not None and (not isinstance(native,str) or not native or len(native)>256):raise RuntimeError('REQUEST_OBSERVER_LOCAL_BINDING_CHANGED')
 if not state.get('record_id'):raise RuntimeError('REQUEST_OBSERVER_SESSION_RECORD_UNKNOWN')
 return {'config':a.c['guest'],'session_id':s,'request_id':r,'record_id':state.get('record_bindings',{}).get(r,state['record_id']),'acpx_request_id':native,'prompt_sha256':prompt_hash},local

def validate_submit_ack(frame,s,r,prompt_hash):
 if not isinstance(frame,dict) or frame.get('type')!='submitted' or frame.get('request_id')!=r or frame.get('prompt_sha256')!=prompt_hash:raise RuntimeError('SUBMIT_ACK_BINDING_CHANGED_OUTCOME_UNKNOWN')
 if 'session_id' in frame and frame['session_id']!=s:raise RuntimeError('SUBMIT_ACK_BINDING_CHANGED_OUTCOME_UNKNOWN')
 native=frame.get('acpx_request_id')
 if frame.get('phase') not in ('queued','terminal') or not isinstance(native,str) or not native or len(native)>256:raise RuntimeError('SUBMIT_ACK_BINDING_CHANGED_OUTCOME_UNKNOWN')

def args(a):
 return ['alloc','exec','-namespace='+a.c['namespace'],'-task='+a.c['task'],'-i=true','-t=false',a.c['allocation'],a.c['guest_node'],'--input-type=module','-e',program()]

def terminal_summary(result):
 import re
 if not isinstance(result,dict):return None
 def enum(value):return value if isinstance(value,str) and re.fullmatch('[a-zA-Z0-9_.-]{1,80}',value) else 'unknown'
 return {'status':enum(result.get('status',result.get('kind'))),'stop_reason':enum(result.get('stopReason',result.get('stop_reason')))}

def read(a,s,r,cursor=None,limit=50,max_bytes=65536):
 if type(limit)!=int or not 1<=limit<=100 or type(max_bytes)!=int or not 2048<=max_bytes<=262144:raise ValueError('TRANSCRIPT_PAGE_BOUND_INVALID')
 if cursor is not None and (not isinstance(cursor,str) or len(cursor)>4096):raise ValueError('TRANSCRIPT_CURSOR_INVALID')
 payload,_=request(a,s,r)
 payload.update(action='transcript',cursor=cursor,limit=limit,max_bytes=max_bytes)
 try:
  response=a.nomad(args(a),input=json.dumps(payload)+'\n',text=True,timeout=15)
  if len(response.stdout)>1048576:raise RuntimeError('TRANSCRIPT_RESPONSE_BOUND_EXCEEDED')
  value=json.loads(response.stdout)
  if value.get('type')=='error':
   code=value.get('code','TRANSCRIPT_READ_UNAVAILABLE');raise RuntimeError(code if isinstance(code,str) and re.fullmatch('[A-Z_]{1,100}',code) else 'TRANSCRIPT_READ_UNAVAILABLE')
  if response.returncode or value.get('type')!='request_transcript' or value.get('session_id')!=s or value.get('request_id')!=r or value.get('record_id')!=payload['record_id'] or value.get('read_only') is not True or value.get('prompt_sha256')!=payload['prompt_sha256'] or (payload.get('acpx_request_id') is not None and value.get('acpx_request_id')!=payload['acpx_request_id']):raise RuntimeError('TRANSCRIPT_RESPONSE_BINDING_CHANGED')
  return value
 except (ValueError,subprocess.TimeoutExpired,OSError):raise RuntimeError('TRANSCRIPT_READ_UNAVAILABLE_NO_PROMPT_RETRY') from None

def close_transport(process):
 # Only our Popen handle is signalled. EOF first reaches the guest's own
 # sessions-watch observer; queue owner, ACP session and daemon are untouched.
 try:
  if process.stdin and not process.stdin.closed:process.stdin.close()
 except (BrokenPipeError,OSError):pass
 try:process.wait(timeout=2)
 except subprocess.TimeoutExpired:
  process.terminate()
  try:process.wait(timeout=1)
  except subprocess.TimeoutExpired:process.kill();process.wait(timeout=1)
 finally:
  if process.stdout:process.stdout.close()

def watch(a,s,r,cursor=None):
 payload,_=request(a,s,r)
 payload.update(action='watch',cursor=cursor)
 process=subprocess.Popen([a.c.get('nomad_bin','nomad'),*args(a)],stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,text=True)
 terminal=False
 try:
  process.stdin.write(json.dumps(payload)+'\n');process.stdin.flush() # Held open until this exact caller disconnects.
  for line in process.stdout:
   if len(line)>3000000:raise RuntimeError('WATCH_FRAME_BOUND_OUTCOME_UNKNOWN')
   frame=json.loads(line)
   if frame.get('type')=='error':raise RuntimeError(frame.get('code','WATCH_OUTCOME_UNKNOWN'))
   if frame.get('type')!='watch' or frame.get('request_id')!=r:raise RuntimeError('WATCH_RESPONSE_BINDING_CHANGED')
   terminal=frame.get('event',{}).get('type')=='turn_result'
   yield frame
   if terminal:break
  if not terminal:raise RuntimeError('WATCH_OBSERVER_CLOSED_OUTCOME_UNKNOWN')
 finally:close_transport(process)
