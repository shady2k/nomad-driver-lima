"""One exact ACP request cancellation intent; observation never re-dispatches it."""
import json,pathlib,re
import request_transcript as observer
from exchange import protected

def context(a,s,r):
 if not a.c.get('creator_record'):raise RuntimeError('REQUEST_CANCEL_NATIVE_OWNER_REQUIRED')
 a.require_initiator(s)
 payload,turn=observer.request(a,s,r)
 return payload,turn

def rpc(a,payload,action,expected=None):
 payload={**payload,'action':action}
 if expected:payload['expected_identity']=expected
 response=a.nomad(observer.args(a),input=json.dumps(payload)+'\n',text=True,timeout=15)
 if len(response.stdout)>1048576:raise RuntimeError('REQUEST_CANCEL_OBSERVATION_UNAVAILABLE_NO_RETRY')
 try:value=json.loads(response.stdout)
 except ValueError:raise RuntimeError('REQUEST_CANCEL_OBSERVATION_UNAVAILABLE_NO_RETRY')from None
 if value.get('type')=='error':
  code=value.get('code');raise RuntimeError(code if isinstance(code,str)and re.fullmatch('[A-Z_]{1,100}',code)else'REQUEST_CANCEL_GUEST_REJECTED_NO_RETRY')
 if response.returncode:raise RuntimeError('REQUEST_CANCEL_OBSERVATION_UNAVAILABLE_NO_RETRY')
 wanted='request_cancel_status'if action=='cancel-status'else'request_cancel'
 if value.get('type')!=wanted or any(value.get(k)!=payload[k]for k in ['session_id','request_id','record_id']):raise RuntimeError('REQUEST_CANCEL_RESPONSE_BINDING_CHANGED')
 native=value.get('acpx_request_id')
 if not isinstance(native,str)or not native or len(native)>256 or(payload.get('acpx_request_id')is not None and native!=payload['acpx_request_id']):raise RuntimeError('REQUEST_CANCEL_RESPONSE_BINDING_CHANGED')
 ident=value.get('identity')
 if not isinstance(ident,dict)or set(ident)!={'machine_id','boot_id'}or not re.fullmatch('[a-f0-9]{32}',str(ident.get('machine_id','')))or not re.fullmatch('[a-f0-9-]{36}',str(ident.get('boot_id','')))or(expected and ident!=expected):raise RuntimeError('REQUEST_CANCEL_MACHINE_CHANGED')
 # Never forward untrusted error bodies or arbitrary guest record fields.
 phase=value.get('phase')
 if phase not in ('not_requested','cancel_unknown','cancel_accepted','target_not_cancelled','owner_unavailable','terminal_observed'):raise RuntimeError('REQUEST_CANCEL_STATUS_INVALID')
 terminal=observer.terminal_summary(value.get('terminal'))
 if phase=='terminal_observed'and(terminal is None or terminal['status']not in ('completed','cancelled','failed')):raise RuntimeError('REQUEST_CANCEL_TERMINAL_PROOF_MISSING')
 return {'session_id':payload['session_id'],'request_id':payload['request_id'],'record_id':payload['record_id'],'acpx_request_id':native,'identity':ident,'phase':phase,'terminal':terminal,**({'error_code':value['error_code']}if isinstance(value.get('error_code'),str)and re.fullmatch('[A-Z_]{1,100}',value['error_code'])else{}),'cancel_is_terminal_proof':False,'prompt_retry':False,'next_action':'read_existing_response'if terminal else'watch_same_request_or_read_cancel_status_no_redispatch'}

def path(a,s,r,create=True):
 from workerctl import uid
 uid(r);folder=a.local(s)/'request-cancellations'
 if create:folder.mkdir(mode=0o700,exist_ok=True)
 if not folder.exists()and not folder.is_symlink():return folder/(r+'.json')
 st=folder.lstat()
 import os,stat
 if not stat.S_ISDIR(st.st_mode)or folder.is_symlink()or st.st_uid!=os.getuid()or st.st_mode&0o077:raise RuntimeError('REQUEST_CANCEL_PRIVATE_STATE_REQUIRED')
 return folder/(r+'.json')

def status(a,s,r):
 payload,_=context(a,s,r)
 # Read-only: do not create the private intent directory or resolve unknown by
 # dispatching another cancellation. Guest status reads only its existing intent.
 file=path(a,s,r,False);prior=protected(file)if file.exists()or file.is_symlink()else None
 if prior and prior.get('attempt')!=a.load(s).get('attempt'):raise RuntimeError('REQUEST_CANCEL_ATTEMPT_CHANGED')
 return rpc(a,payload,'cancel-status',prior.get('identity')if prior else None)

def cancel(a,s,r):
 from workerctl import atomic
 payload,turn=context(a,s,r)
 with a.lock(s):
  if a.load(s).get('phase')!='ready':raise RuntimeError('REQUEST_CANCEL_SESSION_FINISHING')
  file=path(a,s,r)
  if file.exists()or file.is_symlink():
   prior=protected(file)
   if prior.get('attempt')!=a.load(s).get('attempt')or prior.get('session_id')!=s or prior.get('request_id')!=r or prior.get('record_id')!=payload['record_id']:raise RuntimeError('REQUEST_CANCEL_INTENT_CHANGED')
   # Same request is lookup-only, even under a new native tool call.
   return rpc(a,payload,'cancel-status',prior['identity'])
  before=rpc(a,payload,'cancel-status')
  if turn.get('phase')=='terminal':
   terminal=observer.terminal_summary(turn.get('terminal'))
   if not terminal or terminal['status']not in ('completed','cancelled','failed'):raise RuntimeError('REQUEST_CANCEL_TERMINAL_PROOF_INVALID')
   return {**before,'phase':'terminal_observed','terminal':terminal,'next_action':'read_existing_response_no_cancel_needed'}
  if before['terminal']or before['phase']!='not_requested':return before
  intent={**before,'attempt':a.load(s)['attempt'],'phase':'cancel_unknown'}
  atomic(file,intent) # durable BEFORE the mutating transport; never auto replay.
  try:
   result=rpc(a,payload,'cancel',before['identity']);atomic(file,{**result,'attempt':intent['attempt']});return result
  except Exception as error:
   code=str(error)if isinstance(error,RuntimeError)and re.fullmatch('[A-Z_]{1,100}',str(error))else'REQUEST_CANCEL_TRANSPORT_OUTCOME_UNKNOWN'
   return {**intent,'error_code':code,'next_action':'request_cancel_status_or_watch_same_request_no_redispatch','cancel_outcome_unknown':True}
