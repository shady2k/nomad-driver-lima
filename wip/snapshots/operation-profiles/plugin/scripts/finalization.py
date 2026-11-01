"""Caller-verified S3 final snapshot bound through native allocation exec."""
import hashlib,json
from catalog_cli import canonical,uid
CONTROL='/__nomad_driver_lima/finalize-v1'

def policy(retain_for):
 if retain_for is None:return {'mode':'destroy'}
 if retain_for not in ('resume','template','debug'):raise ValueError('RETAIN_FOR_MUST_BE_RESUME_TEMPLATE_OR_DEBUG')
 return {'mode':'retain','reason':retain_for}

def control(adapter,session,request,snapshot,manifest,revision,action):
 adapter.require_initiator(session);state=adapter.load(session);allocation=adapter.target()
 if adapter.attempt(allocation)!=state.get('attempt'):raise RuntimeError('FINALIZATION_ATTEMPT_CHANGED_VM_RETAINED')
 if manifest.get('actual_backend')!='s3' or not isinstance(manifest.get('receipt'),dict):raise RuntimeError('DURABLE_S3_EXPORT_REQUIRED_FOR_DESTROY_USE_EXPLICIT_RETAIN')
 p={'action':action,'allocation':adapter.c['allocation'],'session':uid(session),'creator_agent_id':adapter.c['agent_instance_id'],'creator_session_id':adapter.c['creator_session_id'],'creator_generation':adapter.c['creator_generation'],'request_id':uid(request),'snapshot_id':uid(snapshot),'manifest_sha256':hashlib.sha256(canonical(manifest)).hexdigest(),'s3_receipt_sha256':hashlib.sha256(canonical(manifest['receipt'])).hexdigest(),'backend':'s3','activity_revision':revision}
 args=['alloc','exec','-namespace='+adapter.c['namespace'],'-task='+adapter.c['task'],'-i=false','-t=false',adapter.c['allocation'],CONTROL,canonical(p).decode()]
 try:
  result=adapter.nomad(args,text=True,timeout=30)
  if result.returncode:raise RuntimeError('FINALIZATION_DRIVER_CONTROL_UNAVAILABLE_OR_OUTCOME_UNKNOWN_VM_RETAINED')
  if len(result.stdout)>8192:raise RuntimeError('FINALIZATION_DRIVER_RESPONSE_BOUND')
  proof=json.loads(result.stdout)
 except (ValueError,TimeoutError):raise RuntimeError('FINALIZATION_DRIVER_OUTCOME_UNKNOWN_VM_RETAINED')from None
 if proof.get('protocol')!='finish-v1'or proof.get('allocation')!=p['allocation']or proof.get('session')!=session or proof.get('manifest_sha256')!=p['manifest_sha256']or proof.get('armed')is not(action=='arm'):raise RuntimeError('FINALIZATION_DRIVER_PROOF_MISMATCH_VM_RETAINED')
 return proof
