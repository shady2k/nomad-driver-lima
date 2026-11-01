#!/usr/bin/env python3
"""Existing operator node-side clone binding; lock actual driver ledger throughout.
No ledger edits, ACL changes, tokens, or persistent service. Context is private
operator installation config, never agent-supplied paths/users/commands.
"""
import fcntl,hashlib,importlib.util,json,os,pathlib,pwd,re,stat,subprocess,sys
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'plugin/scripts'))
import template_save as save
spec=importlib.util.spec_from_file_location('template_clone',pathlib.Path(__file__).with_name('template-clone.py'));clone=importlib.util.module_from_spec(spec);spec.loader.exec_module(clone)
def read_private(path):
 fd=os.open(path,os.O_RDONLY|os.O_NOFOLLOW)
 with os.fdopen(fd) as f:
  st=os.fstat(f.fileno())
  if not stat.S_ISREG(st.st_mode) or st.st_uid!=os.getuid() or st.st_mode&0o077:raise RuntimeError('PRIVATE_OPERATOR_METADATA_REQUIRED')
  raw=f.read();return json.loads(raw),hashlib.sha256(raw.encode()).hexdigest()
def bound_clone(packet,receipts,context,invoke):
 p=packet['plan'];source=p['source'];key=context['ledger_key']
 if not re.fullmatch('[a-f0-9]{64}',key):raise RuntimeError('EXACT_LEDGER_KEY_REQUIRED')
 root=pathlib.Path(context['state_dir'])
 if root.is_symlink() or not root.is_absolute() or root.stat().st_uid!=os.getuid() or root.stat().st_mode&0o077:raise RuntimeError('PRIVATE_LEDGER_ROOT_REQUIRED')
 lockfd=os.open(root/(key+'.lock'),os.O_CREAT|os.O_RDWR|os.O_NOFOLLOW,0o600)
 with os.fdopen(lockfd,'a') as lock:
  fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
  state,digest=read_private(root/(key+'.json'))
  for field,expected in {'Key':key,'AllocID':source['allocation'],'TaskName':source['task'],'WorkerSessionID':source['session_id'],'CreatorAgentID':source['creator_agent_id'],'CreatorSessionID':source['creator_session_id']}.items():
   if state.get(field)!=expected:raise RuntimeError('SOURCE_LEDGER_IDENTITY_CHANGED')
  owner=(root/'owner').read_text()
  if state.get('Owner')!=owner or state.get('Phase')not in ('stopped','destroyed_disk_retained') or not state.get('MachineID') or not state.get('BootID'):raise RuntimeError('SOURCE_LEDGER_NOT_CONFIRMED_RETAINED')
  finish=save.verify_stage('source_finish',receipts['source_finish'],packet)
  if finish.get('source_allocation')!=source['allocation'] or finish.get('source_attempt')!=source['attempt']:raise RuntimeError('SOURCE_FINISH_ATTEMPT_CHANGED')
  # VaultFinalizing is a durable no-redelivery fence, not an in-progress flag.
  # A terminal retained phase plus exact confirmed stop/revoke receipt is required.
  generation=source.get('creator_generation')
  if generation:
   # Deployed State stores agent/session, not generation. The immutable plan
   # was derived from the caller config and its successful finish receipt is
   # bound to that exact plan (including generation). Never invent a ledger
   # field or ignore a conflicting generation if a newer schema supplies one.
   if 'CreatorGeneration'in state and state['CreatorGeneration']!=generation:raise RuntimeError('SOURCE_LEDGER_GENERATION_CHANGED')
   identity=finish.get('source_creator_identity')
   expected_identity={'creator_agent_id':source['creator_agent_id'],'creator_session_id':source['creator_session_id'],'creator_generation':generation,'config_sha256':source.get('config_sha256')}
   if identity is not None and identity!=expected_identity:raise RuntimeError('SOURCE_FINISH_GENERATION_CHANGED')
   if not isinstance(generation,str)or not generation or not re.fullmatch('[a-f0-9]{64}',source.get('config_sha256','')):raise RuntimeError('BOUND_SOURCE_GENERATION_PROOF_REQUIRED')
  home=pathlib.Path(context['lima_home']);instance=state.get('Instance')
  if not isinstance(instance,str) or not re.fullmatch('[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}',instance):raise RuntimeError('EXACT_LEDGER_INSTANCE_REQUIRED')
  enriched={**finish,'source_instance':instance,'source_disk_identity':clone.disk_identity(home/instance)}
  prior=receipts.get('clone_pre_effect_proof')
  if prior and (prior.get('operation_id')!=p['operation_id']or prior.get('plan_sha256')!=packet['plan_sha256']or prior.get('source_ledger_sha256')!=digest or prior.get('source_disk_identity')!=enriched['source_disk_identity']or prior.get('source_instance')!=instance):raise RuntimeError('SOURCE_CHANGED_AFTER_CLONE_PRE_EFFECT_PROOF')
  result=invoke(packet,{**receipts,'source_finish':enriched},context)
  # Re-read before releasing the actual driver's lock, including source identity.
  if read_private(root/(key+'.json'))[1]!=digest:raise RuntimeError('SOURCE_LEDGER_CHANGED_DATA_RETAINED')
  result['source_ledger_sha256']=digest;return result

def invoke(packet,receipts,context,check_only=False):
 user=context['runtime_user'];record=pwd.getpwnam(user)
 if record.pw_uid==0:raise RuntimeError('ROOT_LIMA_RUNTIME_UNSUPPORTED')
 script=str(pathlib.Path(__file__).with_name('template-clone.py'))
 args=['/usr/sbin/runuser','-u',user,'--','/usr/bin/env','-i','HOME='+record.pw_dir,'PATH=/usr/local/bin:/usr/bin:/bin',context['python'],script,context['lima_home'],context['limactl']]
 proc=subprocess.run(args,input=json.dumps({'packet':packet,'receipts':receipts,'check_only':check_only}),text=True,capture_output=True,timeout=1900)
 if proc.returncode:raise RuntimeError('NODE_CLONE_OUTCOME_UNKNOWN_RETAIN_SOURCE_AND_CANDIDATE')
 return json.loads(proc.stdout)if check_only else save.verify_stage('clone_candidate',json.loads(proc.stdout),packet)
if __name__=='__main__':
 try:
  context,_=read_private(sys.argv[1]);payload=json.load(sys.stdin)
  if payload.get('stage')not in (None,'clone_candidate','clone_candidate_pre_effect'):raise RuntimeError('EXACT_CLONE_STAGE_REQUIRED')
  check_only=payload.get('stage')=='clone_candidate_pre_effect'
  print(json.dumps(bound_clone(payload['packet'],payload['receipts'],context,lambda p,r,c:invoke(p,r,c,check_only))))
 except Exception as e:
  print(json.dumps({'error':str(e) if re.fullmatch('[A-Z_]{1,100}',str(e)) else type(e).__name__}),file=sys.stderr);sys.exit(1)
