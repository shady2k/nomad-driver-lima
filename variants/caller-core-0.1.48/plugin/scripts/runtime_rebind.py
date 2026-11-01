"""Finite operator/agent rebind after confirmed owned cleanup; never sends prompts."""
import json,pathlib,re
from exchange import protected
import request_transcript as observer

def execute(a,s,operation,cleanup_receipt,retry_pre_effect=False):
 from workerctl import atomic,uid
 uid(operation)
 if not re.fullmatch(r'[A-Za-z0-9-]+\.json',cleanup_receipt):raise ValueError('RECOVERY_CLEANUP_RECEIPT_INVALID')
 with a.lock(s):
  state=observer.checked(a,s);a.require_initiator(s)
  if state.get('phase')!='ready':raise RuntimeError('RECOVERY_SESSION_FINISHING')
  import activity
  for _,row in activity.records(a,s):
   if row.get('operation_id')!=operation and row.get('old_record_id')and(row.get('phase')!='ready'or row.get('session_ready')is not True):raise RuntimeError('RECOVERY_ACTIVE_OR_UNKNOWN_OPERATION')
   if row.get('operation_id')!=operation and any(k in row for k in ('operation_id','execution_id'))and row.get('phase')in activity.UNKNOWN:raise RuntimeError('RECOVERY_ACTIVE_OR_UNKNOWN_OPERATION')
  file=a.local(s)/('runtime-rebind-'+operation+'.json')
  prior=protected(file)if file.exists()or file.is_symlink()else None
  if prior:
   if prior['attempt']!=state['attempt']or prior['cleanup_receipt']!=cleanup_receipt:raise RuntimeError('RECOVERY_OPERATION_CHANGED')
   old=prior['old_record_id']
  else:
   old=state['record_id'];atomic(file,{'type':'runtime_rebind','session_ready':False,'phase':'rebind_unknown','attempt':state['attempt'],'old_record_id':old,'operation_id':operation,'cleanup_receipt':cleanup_receipt})
  program=pathlib.Path(__file__).with_name('runtime_rebind_guest.mjs').read_text()+'\nawait main();\n'
  args=['alloc','exec','-namespace='+a.c['namespace'],'-task='+a.c['task'],'-i=true','-t=false',a.c['allocation'],a.c['guest_node'],'--input-type=module','-e',program]
  payload={'config':a.c['guest'],'session_id':s,'record_id':old,'operation_id':operation,'cleanup_receipt':cleanup_receipt,'lookup_only':bool(prior),'retry_pre_effect':bool(prior)and retry_pre_effect}
  try:
   r=a.nomad(args,input=json.dumps(payload)+'\n',text=True,timeout=45)
   if len(r.stdout)>1048576:raise RuntimeError('RECOVERY_TRANSPORT_OUTCOME_UNKNOWN')
   v=json.loads(r.stdout)
   if v.get('type')=='error':raise RuntimeError(v['code']if re.fullmatch('[A-Z_]+',str(v.get('code','')))else'RECOVERY_REJECTED')
   if r.returncode:raise RuntimeError('RECOVERY_TRANSPORT_OUTCOME_UNKNOWN')
   if v.get('type')!='runtime_rebind'or v.get('session_id')!=s or v.get('operation_id')!=operation or v.get('old_record_id')!=old or v.get('prompt_replayed')is not False:raise RuntimeError('RECOVERY_RESPONSE_BINDING_CHANGED')
   if v.get('phase')=='ready'and v.get('session_ready')is True:
    uid(v['new_record_id']);bindings=v['record_bindings']
    if not isinstance(bindings,dict)or len(bindings)>4096:raise RuntimeError('RECOVERY_HISTORY_BINDING_INVALID')
    for key,value in bindings.items():uid(key);uid(value)
    if state['record_id']not in (old,v['new_record_id']):raise RuntimeError('RECOVERY_CONCURRENT_LOCAL_BINDING_CHANGED')
    atomic(a.local(s)/'session.json',{**state,'record_id':v['new_record_id'],'record_bindings':bindings,'record_history':list(dict.fromkeys([*state.get('record_history',[]),old])),'runtime_recovery':operation})
   else:v={**v,'phase':'rebind_unknown','session_ready':False}
   atomic(file,{**v,'attempt':state['attempt'],'cleanup_receipt':cleanup_receipt});return v
  except Exception as error:
   # The guest owns an exclusive operation intent. Later calls are lookup-only.
   code=str(error)if isinstance(error,RuntimeError)and re.fullmatch('[A-Z_]{1,120}',str(error))else'RECOVERY_TRANSPORT_OUTCOME_UNKNOWN'
   result={'type':'runtime_rebind','phase':'rebind_unknown','operation_id':operation,'session_id':s,'old_record_id':old,'attempt':state['attempt'],'cleanup_receipt':cleanup_receipt,'error_code':code,'session_ready':False,'prompt_replayed':False,'next_action':'read_same_operation_no_new_prompt_or_new_operation'}
   atomic(file,result);return result
