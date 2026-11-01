"""Confirmed one-shot operator orchestration of the existing image pipeline.
No daemon, grants, credential delivery or implicit source-disk deletion.
"""
import hashlib,json,os,pathlib,re,time,uuid,fcntl,stat,subprocess
from secure_logs import Redactor
from workerctl import atomic,canonical
STAGES=('source_finish','clone_candidate','seal','export','boot','check','catalog','publish','catalog_commit')
ACTIONS=('finish_source_retain_disk','clone_stopped_source_disk','seal_export_accept_candidate','publish_new_immutable_version')
def sha(v):return hashlib.sha256(canonical(v)).hexdigest()
def validate_operation(packet):
 p=packet['plan'];operation=p.get('operation_id')
 if not isinstance(operation,str) or str(uuid.UUID(operation))!=operation:raise ValueError('CANONICAL_OPERATION_UUID_REQUIRED')
 for field in ('template_id','version'):
  value=p.get('template',{}).get(field)
  if value is not None and (not isinstance(value,str) or not re.fullmatch('[A-Za-z0-9][A-Za-z0-9._-]{0,79}',value)):raise ValueError('SAFE_TEMPLATE_IDENTITY_REQUIRED')
 return p

def recovery_contract(receipt,source):
 if not isinstance(receipt,dict) or receipt.get('allocation')!=source['allocation'] or receipt.get('session')!=source['session_id'] or receipt.get('vm_status')!='Stopped' or receipt.get('disk_retained')is not True or receipt.get('ledger_unchanged')is not True or receipt.get('revocation')!='unconfirmed' or not receipt.get('approval_reference'):raise ValueError('EXACT_STOPPED_RECOVERY_RECEIPT_REQUIRED')
 runtime=receipt.get('runtime_stop',{})
 if runtime.get('runtime_stopped')is not True or runtime.get('revocation')!='not_attempted':raise ValueError('TRUTHFUL_OWNED_RUNTIME_STOP_REQUIRED')
 for key in ('disk_sha256','ledger_sha256'):
  if not re.fullmatch('[a-f0-9]{64}',receipt.get(key,'')):raise ValueError('PINNED_RECOVERY_IDENTITIES_REQUIRED')
 return receipt

def pipeline_stages(packet):
 return STAGES[:2]+('offline_cleanup',)+STAGES[2:] if packet['plan'].get('source_recovery') else STAGES

def composition_contract(value):
 if not isinstance(value,dict) or set(value)!={'schema_version','retain','remove','description'} or value['schema_version']!=1:raise ValueError('EXPLICIT_DATA_COMPOSITION_REQUIRED')
 if not isinstance(value['description'],str) or not 1<=len(value['description'])<=2048:raise ValueError('DATA_COMPOSITION_DESCRIPTION_REQUIRED')
 for key in ('retain','remove'):
  paths=value[key]
  if not isinstance(paths,list) or len(paths)>64 or any(not isinstance(p,str) or not p.startswith('/') or p=='/' or '..' in p.split('/') or '\x00' in p for p in paths):raise ValueError('DATA_COMPOSITION_PATHS_REQUIRED')
  if len(paths)!=len(set(paths)):raise ValueError('DATA_COMPOSITION_DUPLICATES')
 for keep in value['retain']:
  for drop in value['remove']:
   if keep==drop or keep.startswith(drop.rstrip('/')+'/') or drop.startswith(keep.rstrip('/')+'/'):raise ValueError('DATA_COMPOSITION_OVERLAP')
 if Redactor().text(json.dumps(value))!=json.dumps(value):raise ValueError('SECRET_IN_DATA_COMPOSITION')
 return value

def plan(adapter,session,draft,request,snapshot,operation,data_composition=None,recovery_receipt=None):
 import activity,template_draft
 composition=composition_contract(data_composition)
 if str(uuid.UUID(operation))!=operation:raise ValueError('CANONICAL_OPERATION_UUID_REQUIRED')
 with adapter.lock(session):
  packet=template_draft.inspect_draft(adapter,session,draft);manifest=packet['manifest']
  if manifest['state']!='awaiting_operator_confirmation':raise RuntimeError('DRAFT_CHECKS_INCOMPLETE')
  current=adapter.result(session,request,snapshot);activity.require_current(adapter,session,current,request,snapshot or request)
  if sha(current)!=manifest['source']['export_manifest_sha256']:raise RuntimeError('DRAFT_NOT_CURRENT_VERIFIED_SNAPSHOT')
  activity.pending(adapter,session);state,allocation=template_draft.own(adapter,session)
  if state.get('phase')!='ready':raise RuntimeError('SOURCE_NOT_READY_NO_NEW_SAVE')
  source={'session_id':session,'allocation':adapter.c['allocation'],'job':adapter.c['job'],'task':adapter.c['task'],'node_id':allocation.get('NodeID'),'config_sha256':adapter.hash,'attempt':adapter.attempt(allocation),'creator_agent_id':adapter.c['agent_instance_id'],'creator_session_id':adapter.c['creator_session_id'],'creator_generation':adapter.c['creator_generation']}
  if not source['node_id']:raise RuntimeError('SOURCE_NODE_UNKNOWN')
  recovery=recovery_contract(recovery_receipt,source) if recovery_receipt is not None else None
  doc={'schema_version':1,'operation_id':operation,'draft_sha256':packet['manifest_sha256'],'source':source,'template':manifest['proposed_template'],'dependency_recipe':manifest['dependency_recipe'],'snapshot':{'request_id':request,'snapshot_id':snapshot or request,'manifest_sha256':sha(current)},'method':'official_stopped_disk_clone_then_existing_seal_export_pipeline','data_composition':composition,'source_lifecycle':'finish_after_verified_results; Prime/Bao stop and task-token revoke; source VM stays stopped with disk retained','resume':'new session/VM from published template plus preserved results; no old ACP prompt replay','no_source_sealing_or_disk_purge':True,'actions':list(ACTIONS),'stages':list(STAGES),'activation':'publication does not change configured node/catalog pins; coordinated activation required'}
  if recovery is not None:
   doc['source_recovery']={'receipt':recovery,'receipt_sha256':sha(recovery),'cleanup_before_candidate_boot':True}
   doc['source_lifecycle']='already stopped by explicit user approval; source disk retained; revocation UNCONFIRMED; no finish/revoke replay'
   doc['stages']=list(STAGES[:2]+('offline_cleanup',)+STAGES[2:])
  path=adapter.local(session)/('template-save-'+operation+'.plan.json');result={'plan':doc,'plan_sha256':sha(doc),'confirmation_required':True}
  if path.exists():
   old=json.loads(path.read_text())
   if old!=result:raise RuntimeError('IMMUTABLE_SAVE_PLAN_CHANGED')
   return old
  atomic(path,result);return result

def confirmation(packet,approval,now=None):
 now=time.time() if now is None else now;p=validate_operation(packet)
 if p.get('method')!='official_stopped_disk_clone_then_existing_seal_export_pipeline':raise RuntimeError('DISK_CLONE_SAVE_PLAN_REQUIRED_NO_REBUILD')
 composition_contract(p.get('data_composition'))
 if sha(p)!=packet['plan_sha256'] or approval.get('plan_sha256')!=packet['plan_sha256'] or approval.get('operation_id')!=p['operation_id'] or approval.get('source_allocation')!=p['source']['allocation'] or approval.get('template_id')!=p['template']['template_id'] or approval.get('version')!=p['template']['version'] or approval.get('actions')!=list(ACTIONS):raise RuntimeError('EXACT_SAVE_CONFIRMATION_REQUIRED')
 expires=approval.get('expires_unix')
 if type(expires)not in (int,float) or not now<expires<=now+3600:raise RuntimeError('SAVE_CONFIRMATION_EXPIRED_OR_UNBOUNDED')
 # This binds reviewed intent, NOT cryptographic proof of a human approval.
 # Host/S3 authority comes solely from the existing trusted operator executor.
 return True

def verify_stage(stage,result,packet):
 validate_operation(packet)
 if result.get('operation_id')!=packet['plan']['operation_id'] or result.get('plan_sha256')!=packet['plan_sha256']:raise RuntimeError('STALE_OR_UNBOUND_STAGE_RECEIPT')
 if stage=='source_finish' and packet['plan'].get('source_recovery'):
  required=('source_owner_verified','results_receipt_verified','source_recovery_receipt_verified','source_vm_stopped','source_disk_retained','revocation_unconfirmed')
  if result.get('task_token_revoked')is True or result.get('finish_replayed')is not False:raise RuntimeError('RECOVERY_MUST_NOT_FABRICATE_REVOKE_OR_REPLAY_FINISH')
  if any(result.get(k)is not True for k in required):raise RuntimeError('RECOVERY_SOURCE_PROOF_INCOMPLETE')
  return result
 if stage=='offline_cleanup':
  if not packet['plan'].get('source_recovery')or any(result.get(k)is not True for k in ('offline_cleanup_before_boot','candidate_stopped','source_disk_unchanged','runtime_state_removed')):raise RuntimeError('OFFLINE_CANDIDATE_CLEANUP_REQUIRED')
  return result
 required={'source_finish':('source_owner_verified','results_receipt_verified','prime_bao_stopped','task_token_revoked','source_vm_stopped','source_disk_retained'), 'clone_candidate':('source_stopped_verified','source_identity_bound','candidate_distinct','source_disk_unchanged','clone_stopped','no_rebuild'), 'seal':('runtime_credentials_absent','approved_data_composition_verified','identity_sealed','candidate_stopped','source_disk_unchanged'), 'export':('standalone_disk','checksum_verified'), 'boot':('two_independent_boots',), 'check':('unique_identities','dependency_versions_verified','base_checksum_unchanged'), 'catalog':('new_version_no_overwrite',), 'publish':('all_objects_readback_verified',), 'catalog_commit':('atomic_conditional_append','new_version_no_overwrite')}[stage]
 if any(result.get(k)is not True for k in required):raise RuntimeError('PIPELINE_PROOF_INCOMPLETE_'+stage.upper())
 if stage in ('export','publish','catalog_commit') and not re.fullmatch('[a-f0-9]{64}',result.get('disk_sha256','')):raise RuntimeError('EXPORTED_DISK_IDENTITY_REQUIRED')
 return result

def execute(packet,approval,journal,backend):
 journal=pathlib.Path(journal)
 if journal.parent.is_symlink() or journal.parent.stat().st_mode&0o077:raise RuntimeError('PRIVATE_OPERATOR_JOURNAL_REQUIRED')
 fd=os.open(str(journal)+'.lock',os.O_RDWR|os.O_CREAT|os.O_NOFOLLOW,0o600)
 with os.fdopen(fd,'a') as lock:
  fcntl.flock(lock,fcntl.LOCK_EX)
  return _execute_locked(packet,approval,journal,backend)

def _execute_locked(packet,approval,journal,backend):
 confirmation(packet,approval);journal=pathlib.Path(journal)
 if journal.is_symlink():raise RuntimeError('SAVE_JOURNAL_SYMLINK')
 if journal.exists():
  state=json.loads(journal.read_text())
  if state['plan_sha256']!=packet['plan_sha256']:raise RuntimeError('SAVE_OPERATION_ID_CHANGED')
  if state['phase']=='published_pending_activation':return state
  if state.get('inflight'):raise RuntimeError('SAVE_STAGE_OUTCOME_UNKNOWN_RECONCILE_NO_REPLAY')
 else:state={'operation_id':packet['plan']['operation_id'],'plan_sha256':packet['plan_sha256'],'phase':'confirmed','receipts':{}}
 # Verify binding/authority/conditional-write support before stopping source.
 backend.preflight(packet)
 disk=None
 for stage in pipeline_stages(packet):
  if stage in state['receipts']:
   result=verify_stage(stage,state['receipts'][stage],packet)
  else:
   state.update(phase=stage,inflight=stage);atomic(journal,state)
   try:
    result=verify_stage(stage,backend.stage(stage,packet,state['receipts']),packet)
    if result.get('disk_sha256') and disk is not None and disk!=result['disk_sha256']:raise RuntimeError('PIPELINE_DISK_IDENTITY_CHANGED')
   except Exception:
    state['phase']='outcome_unknown';atomic(journal,state);raise RuntimeError('SAVE_STAGE_OUTCOME_UNKNOWN_'+stage.upper()+'_DATA_RETAINED') from None
   state['receipts'][stage]=result;state.pop('inflight',None);atomic(journal,state)
  if result.get('disk_sha256'):
   if disk is not None and disk!=result['disk_sha256']:raise RuntimeError('PIPELINE_DISK_IDENTITY_CHANGED')
   disk=result['disk_sha256']
 state.update(phase='published_pending_activation',activation_required=True,source_disk_retained=True,source_automatic_restart=False);atomic(journal,state);return state

def status(journal):return json.loads(pathlib.Path(journal).read_text())

class OperatorPipeline:
 """One-shot approved operator commands; no implicit credentials or remote grants.
 Commands are fixed in PRIVATE operator config, never taken from the agent plan.
 Each command wraps an existing pipeline stage and returns a bound JSON receipt.
 """
 def __init__(self,policy_path):
  import exchange
  self.policy=exchange.protected(policy_path)
  if self.policy.get('role')!='template-publisher' or self.policy.get('backend')!='existing-ansible-image-pipeline':raise RuntimeError('TRUSTED_TEMPLATE_OPERATOR_BINDING_REQUIRED')
  if set(self.policy.get('commands',{})) not in (set(STAGES)|{'preflight'},set(STAGES)|{'preflight','offline_cleanup'}):raise RuntimeError('EXISTING_PIPELINE_STAGE_BINDINGS_REQUIRED')
 def call(self,stage,packet,receipts):
  argv=self.policy['commands'][stage]
  if not isinstance(argv,list) or not argv or not os.path.isabs(argv[0]) or any(not isinstance(x,str) or '\0' in x for x in argv):raise RuntimeError('FIXED_OPERATOR_ARGV_REQUIRED')
  # This does not confer host/S3 authority: the executing operator must already
  # have those rights. No keys or tokens are transported to the guest or journal.
  result=subprocess.run(argv,input=json.dumps({'stage':stage,'packet':packet,'receipts':receipts}),text=True,capture_output=True,timeout=self.policy.get('stage_timeout_seconds',3600))
  if result.returncode or len(result.stdout.encode())>8388608:raise RuntimeError('OPERATOR_STAGE_FAILED_OUTCOME_UNCERTAIN')
  if Redactor().text(result.stdout)!=result.stdout:raise RuntimeError('SECRET_IN_OPERATOR_RESULT_REJECTED')
  return json.loads(result.stdout)
 def preflight(self,packet):
  if self.policy.get('conditional_catalog_writes_verified')is not True:raise RuntimeError('ATOMIC_CATALOG_BACKEND_NOT_VERIFIED')
  row=self.call('preflight',packet,{})
  if row.get('plan_sha256')!=packet['plan_sha256'] or row.get('operation_id')!=packet['plan']['operation_id'] or not all(row.get(k)is True for k in ('source_owner_current','existing_pipeline_bound','isolated_builder_capacity_available','scoped_s3_writer_available','catalog_cas_verified')):raise RuntimeError('TEMPLATE_PIPELINE_PREFLIGHT_FAILED_NO_SOURCE_STOP')
 def stage(self,name,packet,receipts):return self.call(name,packet,receipts)
