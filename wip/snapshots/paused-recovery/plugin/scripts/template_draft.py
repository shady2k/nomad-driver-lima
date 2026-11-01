"""Immutable review packet only. No image capture, sealing, stop or publication API."""
import hashlib,json,os,pathlib,re,stat,uuid,math
from secure_logs import Redactor
NEXT_STEP='Operator reviews this exact SHA. This agent command cannot approve, capture, stop or publish.'

def canonical(v):return json.dumps(v,sort_keys=True,separators=(',',':')).encode()
def sha(v):return hashlib.sha256(v).hexdigest()
def uuid_value(v):
 if str(uuid.UUID(v))!=v:raise ValueError('CANONICAL_UUID_REQUIRED')
 return v

def clean(v):
 if isinstance(v,str):
  if len(v)>8192 or '\0' in v or Redactor().text(v)!=v:raise ValueError('SECRET_OR_BEARER_URL_IN_DRAFT')
  if re.search(r'[A-Za-z][A-Za-z0-9+.-]*://[^\s/]*@',v) or re.search(r'(?i)(?:api[-_]?key|client[-_]?secret|access[-_]?token|password|authorization)\b',v):raise ValueError('CREDENTIAL_REFERENCE_IN_DRAFT')
  if re.search(r'(?i)(?:LLM_API_KEY|VAULT_TOKEN|NOMAD_TOKEN|BEGIN .*PRIVATE KEY|/\.ssh/|/\.aws/|/\.vault-token)',v):raise ValueError('CREDENTIAL_REFERENCE_IN_DRAFT')
 elif isinstance(v,list):
  if len(v)>128:raise ValueError('DRAFT_LIST_BOUND')
  for x in v:clean(x)
 elif isinstance(v,dict):
  if len(v)>64:raise ValueError('DRAFT_OBJECT_BOUND')
  for k,x in v.items():clean(k);clean(x)
 elif v is not None and type(v) not in (bool,int,float):raise ValueError('DRAFT_VALUE_TYPE')

def recipe(value):
 if not isinstance(value,dict) or set(value)!={'schema_version','purpose','packages','steps'} or type(value['schema_version'])!=int or value['schema_version']!=1:raise ValueError('DEPENDENCY_RECIPE_SCHEMA')
 if not re.fullmatch('[a-z0-9][a-z0-9-]{0,63}',value['purpose']):raise ValueError('PURPOSE_SLUG_REQUIRED')
 if not isinstance(value['packages'],list) or not value['packages']:raise ValueError('PINNED_PACKAGE_INVENTORY_REQUIRED')
 for p in value['packages']:
  if not isinstance(p,dict) or set(p)!={'manager','name','version'} or any(not isinstance(p[k],str) or not 1<=len(p[k])<=256 for k in p):raise ValueError('PACKAGE_SCHEMA')
  if p['version'].lower() in ('latest','*','main','master','head'):raise ValueError('UNPINNED_DEPENDENCY')
 if not isinstance(value['steps'],list) or not value['steps']:raise ValueError('RECIPE_STEPS_REQUIRED')
 for step in value['steps']:
  if not isinstance(step,dict) or set(step)!={'label','argv'} or not re.fullmatch('[a-z][a-z0-9-]{0,31}',step['label']):raise ValueError('RECIPE_STEP_SCHEMA')
  if not isinstance(step['argv'],list) or not step['argv'] or any(not isinstance(x,str) or len(x)>4096 for x in step['argv']):raise ValueError('RECIPE_ARGV_SCHEMA')
 clean(value);return value

def checks(value,request_id,terminal):
 if not isinstance(value,dict) or set(value)!={'schema_version','environment_ready','checks','model_smoke'} or type(value['schema_version'])!=int or value['schema_version']!=1 or type(value['environment_ready'])!=bool:raise ValueError('ENVIRONMENT_CHECKS_SCHEMA')
 if not isinstance(value['checks'],list) or not value['checks']:raise ValueError('ENVIRONMENT_CHECKS_REQUIRED')
 for item in value['checks']:
  if not isinstance(item,dict) or set(item)!={'name','status','version'} or item['status'] not in ('pass','blocked','fail') or not all(isinstance(item[k],str) and len(item[k])<=256 for k in item):raise ValueError('ENVIRONMENT_CHECK_SCHEMA')
 smoke=value['model_smoke']
 if not isinstance(smoke,dict) or set(smoke)!={'status','request_id','provider','model','usage'} or smoke['status'] not in ('pass','blocked','fail'):raise ValueError('MODEL_SMOKE_REPORT_SCHEMA')
 if smoke['request_id']!=request_id:raise ValueError('MODEL_SMOKE_RECEIPT_MISMATCH')
 if not all(isinstance(smoke[k],str) and 0<len(smoke[k])<=256 for k in ('provider','model')):raise ValueError('MODEL_SMOKE_METADATA_SCHEMA')
 if not isinstance(smoke['usage'],dict) or set(smoke['usage'])-{'input_tokens','output_tokens','cost_usd'} or any(type(x) not in (int,float) or not math.isfinite(x) or x<0 for x in smoke['usage'].values()):raise ValueError('MODEL_USAGE_SCHEMA')
 if smoke['status']=='pass' and terminal.get('stopReason')!='end_turn':raise ValueError('MODEL_SMOKE_NOT_TERMINAL_SUCCESS')
 clean(value)
 ready=value['environment_ready'] and all(x['status']=='pass' for x in value['checks']) and smoke['status']=='pass'
 return value,ready

def artifact(manifest,path,output):
 if not isinstance(path,str) or path.startswith('/') or any(x in ('','..') or x.startswith('.') for x in path.split('/')):raise ValueError('DECLARED_RECIPE_PATH_REQUIRED')
 selected=[x for x in manifest['files'] if x['path']==path and x.get('source')!='native_diagnostic']
 if len(selected)!=1:raise ValueError('DECLARED_RECIPE_ARTIFACT_UNAVAILABLE')
 item=selected[0];p=output/item['local_file']
 if p.is_symlink() or p.parent!=output or item['size']>65536:raise ValueError('DRAFT_INPUT_BOUND_OR_PATH')
 raw=p.read_bytes()
 if len(raw)!=item['size'] or sha(raw)!=item['sha256']:raise RuntimeError('DRAFT_INPUT_CHANGED')
 return json.loads(raw),{'path':path,'size':len(raw),'sha256':sha(raw)}

def own(a,s):
 state=a.load(s);a.require_initiator(s);row=a.target();meta=row['Job']['Meta']
 for k,v in {'creator_agent_id':a.c.get('agent_instance_id'),'creator_session_id':a.c.get('creator_session_id'),'creator_generation':a.c.get('creator_generation'),'worker_session_id':s}.items():
  if not v or meta.get(k)!=v:raise RuntimeError('TEMPLATE_DRAFT_OWNER_MISMATCH')
 if state.get('attempt')!=a.attempt(row):raise RuntimeError('TEMPLATE_DRAFT_ATTEMPT_CHANGED')
 return state,row

def draft_path(a,s,draft):
 uuid_value(draft);root=a.local(s)/'template-drafts';root.mkdir(mode=0o700,exist_ok=True);st=root.lstat()
 if not stat.S_ISDIR(st.st_mode) or st.st_uid!=os.getuid() or st.st_mode&0o077:raise ValueError('PRIVATE_DRAFT_ROOT_REQUIRED')
 return root/(draft+'.json')

def inspect_draft(a,s,draft):
 a.load(s);a.require_initiator(s);p=draft_path(a,s,draft);st=p.lstat()
 if not stat.S_ISREG(st.st_mode) or st.st_uid!=os.getuid() or st.st_mode&0o077:raise ValueError('PRIVATE_DRAFT_FILE_REQUIRED')
 doc=json.loads(p.read_text())
 if set(doc)!={'manifest','manifest_sha256','confirmation_required','next_step'} or doc['confirmation_required'] is not True or doc['next_step']!=NEXT_STEP:raise RuntimeError('DRAFT_CONFIRMATION_GATE_CHANGED')
 if sha(canonical(doc['manifest']))!=doc['manifest_sha256'] or doc['manifest']['source']['config_sha256']!=a.hash or doc['manifest']['draft_id']!=draft:raise RuntimeError('DRAFT_MANIFEST_CHANGED')
 return doc

def prepare(a,s,draft,template_id,version,request_id,recipe_path,checks_path,snapshot_id=None):
 uuid_value(draft);uuid_value(request_id)
 if not re.fullmatch('[a-z0-9][a-z0-9-]{0,63}',template_id) or not re.fullmatch('[A-Za-z0-9][A-Za-z0-9._-]{0,63}',version):raise ValueError('IMMUTABLE_TEMPLATE_NAME_VERSION_REQUIRED')
 with a.lock(s):
  state,row=own(a,s);m=a.result(s,request_id,snapshot_id);receipt=json.loads(a.export_record(s,request_id,snapshot_id).read_text());output=a.local(s)/receipt['manifest_dir']
  r,recipe_proof=artifact(m,recipe_path,output);report,check_proof=artifact(m,checks_path,output)
  r=recipe(r);report,ready=checks(report,request_id,m['terminal'])
  if m.get('actual_backend')=='s3' and any(m.get('export_binding',{}).get(k)!=v for k,v in state['attempt'].items()):raise RuntimeError('DRAFT_EXPORT_ATTEMPT_MISMATCH')
  tasks=[t for g in row['Job'].get('TaskGroups',[]) for t in g.get('Tasks',[]) if t.get('Name')==a.c['task']]
  if len(tasks)!=1:raise RuntimeError('DRAFT_TASK_AMBIGUOUS')
  cfg=tasks[0].get('Config',{});arch=cfg.get('architecture')
  if not arch:
   resolved=a.c.get('resolved_template')
   if not isinstance(resolved,dict) or state['attempt'].get('resolved_template')!=resolved:raise RuntimeError('DRAFT_RESOLVED_PLATFORM_UNBOUND')
   arch=resolved.get('architecture')
  if arch not in ('amd64','arm64'):raise RuntimeError('DRAFT_ARCHITECTURE_UNKNOWN')
  raw=pathlib.Path(a.c['catalog_file']).read_bytes()
  if len(raw)>4194304 or sha(raw)!=a.c['catalog_sha256']:raise RuntimeError('CATALOG_CHECKSUM_MISMATCH')
  catalog=json.loads(raw)
  if any((x['template_id'],x['version'],x['architecture'])==(template_id,version,arch) for x in catalog['templates']):raise ValueError('TEMPLATE_VERSION_ALREADY_EXISTS_NO_OVERWRITE')
  source={'session_id':s,'job':a.c['job'],'allocation':a.c['allocation'],'attempt':state['attempt'],'config_sha256':a.hash,'creator_agent_id':a.c['agent_instance_id'],'creator_session_id':a.c['creator_session_id'],'creator_generation':a.c['creator_generation'],'export_manifest_sha256':sha(canonical(m)),'receipt_backend':m.get('actual_backend','unknown'),'request_id':request_id,'recipe_artifact':recipe_proof,'checks_artifact':check_proof}
  intent=sha(canonical({'source':source,'template_id':template_id,'version':version,'architecture':arch}))
  p=draft_path(a,s,draft)
  if p.exists():
   old=inspect_draft(a,s,draft)
   if old['manifest']['intent_sha256']!=intent:raise RuntimeError('IMMUTABLE_DRAFT_CONTENT_CHANGED')
   return old
  manifest={'schema_version':1,'draft_id':draft,'intent_sha256':intent,'state':'awaiting_operator_confirmation' if ready else 'checks_incomplete_operator_review','proposed_template':{'template_id':template_id,'version':version,'architecture':arch,'guest_os':'linux','exported_disk_bytes':None,'disk_sha256':None,'minimum_disk_gib':None,'source_requested_disk_gib':cfg.get('disk_gib')},'source':source,'dependency_recipe':r,'environment_report':report,'evidence':'receipt_verified_agent_report_not_independent_image_acceptance','operator_pipeline':{'reuse':['base-build.yml','base-refresh.yml','base-seal.sh','base-artifact.yml'],'required_gates':['review_exact_draft_sha','build_clean_candidate_from_recipe','explicit_approval_before_any_source_vm_stop','seal_only_new_approved_disk','two_independent_boots_unique_identity','verify_candidate_size_digest_arch_minimum_disk','explicit_approval_before_exact_version_publication'],'exclude':['keys_tokens_credentials','project_repository_and_data','owner_and_session_identity','operation_journals_and_private_results']},'actions_performed':{'guest_commands':0,'model_requests':0,'vm_stops':0,'image_builds':0,'disk_seals':0,'s3_uploads':0,'catalog_writes':0}}
  doc={'manifest':manifest,'manifest_sha256':sha(canonical(manifest)),'confirmation_required':True,'next_step':NEXT_STEP}
  fd=os.open(p,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
  with os.fdopen(fd,'wb') as f:f.write(canonical(doc));f.flush();os.fsync(f.fileno())
  fd=os.open(p.parent,os.O_RDONLY)
  try:os.fsync(fd)
  finally:os.close(fd)
  return doc
