"""Same-ID allocation registration reconciliation. Never replays model work."""
import hashlib,json,re,time,urllib.parse,os
from exchange import protected

def subset(expected,actual):
 if isinstance(expected,dict):return isinstance(actual,dict)and all(k in actual and subset(v,actual[k])for k,v in expected.items())
 if isinstance(expected,list):return isinstance(actual,list)and len(expected)==len(actual)and all(subset(x,y)for x,y in zip(expected,actual))
 return expected==actual

def lookup(c,jobid):
 result=c.run(['operator','api','-X','GET','/v1/job/'+urllib.parse.quote(jobid,safe='')+'?namespace='+urllib.parse.quote(c.c['namespace'],safe='')])
 if result.returncode:raise RuntimeError('NOMAD_LOOKUP_UNAVAILABLE_KEEP_SAME_VM_ID')
 raw=result.stdout
 # Native operator api can return HTTP 404 text with exit zero. No other text,
 # empty body, ACL error, invalid JSON or transport failure proves absence.
 if raw.strip()==b'job not found':return None
 try:job=json.loads(raw)
 except (ValueError,UnicodeError):raise RuntimeError('NOMAD_LOOKUP_INVALID_RESPONSE_KEEP_SAME_VM_ID')from None
 if not isinstance(job,dict)or job.get('ID')!=jobid or job.get('Namespace')!=c.c['namespace']:raise RuntimeError('NOMAD_JOB_IDENTITY_CONFLICT')
 return job

def observation(c,session,intent,job):
 from secure_logs import Redactor
 out={'vm_id':session,'registration':'absent'if job is None else'accepted','observed_unix':int(time.time()),'snapshot_not_reservation':True,'prompt_replay':False}
 if job is None:return out
 expected=intent.get('job_spec')
 if expected is not None:
  from catalog_cli import canonical
  if hashlib.sha256(canonical(expected)).hexdigest()!=intent.get('job_spec_sha256')or not subset(expected,job):raise RuntimeError('NOMAD_EXISTING_JOB_PAYLOAD_CONFLICT_NO_OVERWRITE')
 else:
  meta=job.get('Meta')or{}
  for k,v in {'nomad_workers_owner':c.c['owner_id'],'worker_session_id':session,'creator_agent_id':intent.get('creator_agent_id'),'creator_session_id':intent.get('creator_session_id'),'creator_generation':intent.get('creator_generation')}.items():
   if v is not None and meta.get(k)!=v:raise RuntimeError('NOMAD_EXISTING_JOB_OWNER_CONFLICT')
 out.update(job_status=job.get('Status'),job_stopped=bool(job.get('Stop')),evaluations=[])
 path='/v1/job/'+urllib.parse.quote(job['ID'],safe='')+'/evaluations?namespace='+urllib.parse.quote(c.c['namespace'],safe='')
 try:
  rows=c.api(path)
  if not isinstance(rows,list)or len(rows)>256:raise RuntimeError('NOMAD_EVALUATION_RESPONSE_BOUND')
  for row in rows:
   failures=[]
   for group,metric in (row.get('FailedTGAllocs')or{}).items():
    failures.append({'group':Redactor().text(str(group))[:128],'nodes_evaluated':metric.get('NodesEvaluated'),'nodes_exhausted':metric.get('NodesExhausted'),'constraint_filtered':{Redactor().text(str(k))[:256]:v for k,v in (metric.get('ConstraintFiltered')or{}).items()},'dimension_exhausted':metric.get('DimensionExhausted')})
   out['evaluations'].append({'id':row.get('ID'),'status':row.get('Status'),'description':Redactor().text(str(row.get('StatusDescription','')))[:1024],'blocked_eval':row.get('BlockedEval'),'failed_task_groups':failures})
 except RuntimeError:out['evaluation_observation']='unavailable_not_empty'
 return out

def start_dispatch(c,path,intent):
 # fsync the rename and newly created session directory before any network
 # side effect. An unsynced prepared marker must not reappear after a crash.
 from catalog_cli import atomic
 intent.update(phase='submit_intent',registration_stage='dispatch_started');atomic(path,intent)
 folder=path.parent
 while True:
  fd=os.open(folder,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW)
  try:os.fsync(fd)
  finally:os.close(fd)
  if folder==c.root:break
  if folder.parent==folder:raise RuntimeError('ALLOCATION_DISPATCH_ROOT_MISMATCH')
  folder=folder.parent

def reconcile(c,session,allow_register=False):
 from catalog_cli import atomic,canonical
 path=c.root/session/'allocation-intent.json';intent=protected(path);jobid='worker-'+session
 if intent.get('session_id')!=session or intent.get('job')!=jobid:raise RuntimeError('ALLOCATION_INTENT_BINDING_CHANGED')
 job=lookup(c,jobid)
 if job is not None:
  result=observation(c,session,intent,job)
  if allow_register:
   intent.update(phase='submitted',registration_confirmed=True);atomic(path,intent)
  return result
 if intent.get('phase')=='submitted'or intent.get('registration_confirmed'):
  return {**observation(c,session,intent,None),'state':'previously_accepted_job_absent_no_recreate'}
 if intent.get('registration_stage')!='prepared_not_dispatched' or intent.get('phase')!='submit_intent':
  return {**observation(c,session,intent,None),'registration':'unknown_after_dispatch_or_legacy','state':'job_absent_dispatch_not_excludable_lookup_only','observed_job':'absent','automatic_registration_allowed':False}
 if not allow_register:return {**observation(c,session,intent,None),'durable_proof':'prepared_not_dispatched','automatic_registration_allowed':True}
 spec=intent.get('job_spec')
 if spec is None:return {**observation(c,session,intent,None),'state':'legacy_intent_missing_frozen_job_no_automatic_register'}
 if hashlib.sha256(canonical(spec)).hexdigest()!=intent.get('job_spec_sha256')or spec.get('ID')!=jobid or spec.get('Namespace')!=c.c['namespace']:raise RuntimeError('ALLOCATION_FROZEN_JOB_CHANGED')
 meta=spec.get('Meta')or{}
 for k,v in {'nomad_workers_owner':c.c['owner_id'],'worker_session_id':session,'creator_agent_id':intent.get('creator_agent_id'),'creator_session_id':intent.get('creator_session_id'),'creator_generation':intent.get('creator_generation')}.items():
  if v is not None and meta.get(k)!=v:raise RuntimeError('ALLOCATION_FROZEN_OWNER_CHANGED')
 if not intent.get('creator_record'):raise RuntimeError('ALLOCATION_CURRENT_CREATOR_REQUIRED')
 native=protected(intent['creator_record'])
 if native.get('ended')or (native.get('agent_instance_id'),native.get('native_session_id'),native.get('generation'))!=(intent.get('creator_agent_id'),intent.get('creator_session_id'),intent.get('creator_generation')):raise RuntimeError('ALLOCATION_CURRENT_CREATOR_CHANGED')
 start_dispatch(c,path,intent)
 try:
  result=c.run(['job','run','-check-index=0','-detach','-json','-'],input=canonical({'Job':spec}))
  intent['phase']='submitted'if result.returncode==0 else'submit_unknown';atomic(path,intent)
 except Exception:
  intent['phase']='submit_unknown';atomic(path,intent)
 # A CAS loser/lost ACK is reconciled, never used to change job ID or payload.
 job=lookup(c,jobid)
 out=observation(c,session,intent,job)
 if job is not None:
  intent.update(phase='submitted',registration_confirmed=True);atomic(path,intent)
 return {**out,'state':'registered_or_existing'if job else'job_absent_after_dispatch_lookup_only','registration_attempted_with_create_only_cas':True,'automatic_registration_allowed':False}
