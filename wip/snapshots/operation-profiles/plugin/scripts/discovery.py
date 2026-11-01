"""Read-only local discovery; verified lineage is descriptive, never authority."""
import fcntl,hashlib,json,os,pathlib,re,uuid,urllib.parse
from catalog_cli import atomic,canonical,uid
from exchange import protected
from secure_logs import Redactor

def clean(value):
 return Redactor().text(str(value))[:512]if value is not None else None

def local(client,session):
 root=client.root/uid(session)
 if root.is_symlink()or not root.is_dir()or root.stat().st_mode&0o077:raise ValueError('DISCOVERY_UNSAFE_SESSION_DIRECTORY')
 def read(name):
  p=root/name
  if p.lstat().st_size>4194304:raise ValueError('DISCOVERY_RECORD_TOO_LARGE')
  return protected(p)
 intent=read('allocation-intent.json');binding=read('target-config.json')
 if intent.get('session_id')!=session or binding.get('owner_id')!=client.c['owner_id']or binding.get('namespace')!=client.c['namespace']or binding.get('job')!=intent.get('job'):raise ValueError('DISCOVERY_LOCAL_BINDING_MISMATCH')
 owner={'agent_id':intent.get('creator_agent_id'),'native_session_id':intent.get('creator_session_id'),'generation':intent.get('creator_generation')}
 if any(binding.get(k)!=owner[v]for k,v in [('agent_instance_id','agent_id'),('creator_session_id','native_session_id'),('creator_generation','generation')]):raise ValueError('DISCOVERY_CREATOR_BINDING_MISMATCH')
 return intent,binding,owner

def observe(client,session):
 intent,binding,owner=local(client,session);allocation=uid(binding['allocation']);ns=urllib.parse.quote(client.c['namespace'],safe='')
 full=client.api('/v1/allocation/'+allocation+'?namespace='+ns);job=full.get('Job',{});meta=job.get('Meta',{})
 if full.get('ID')!=allocation or job.get('ID')!=binding['job']or (full.get('Namespace')or job.get('Namespace'))!=client.c['namespace']:raise ValueError('DISCOVERY_REMOTE_BINDING_MISMATCH')
 expected={'nomad_workers_owner':client.c['owner_id'],'creator_agent_id':owner['agent_id'],'creator_session_id':owner['native_session_id'],'creator_generation':owner['generation'],'worker_session_id':session}
 if any(not v or meta.get(k)!=v for k,v in expected.items()):raise ValueError('DISCOVERY_REMOTE_OWNER_MISMATCH')
 task=full.get('TaskStates',{}).get(client.c['task'],{})
 return intent,binding,owner,full,meta,task

def parent(intent):
 if intent.get('recovery_parent'):
  p=intent['recovery_parent'];return 'private_copy',uid(p['source_session']),uid(p['source_allocation'])
 if intent.get('resumed_from')and intent.get('resumed_from_session'):return 'resume',uid(intent['resumed_from_session']),uid(intent['resumed_from'])
 return None

def index(client,session):
 """Explicit maintenance write or future request-status hook, never find/list."""
 intent,binding,owner,full,meta,task=observe(client,session);p=parent(intent)
 if not p:return {'indexed':False,'reason':'no_parent'}
 kind,previous,allocation=p
 if previous==session:raise ValueError('LINEAGE_SELF_LINK_REFUSED')
 if full.get('ClientStatus')!='running'or task.get('State')!='running'or not task.get('StartedAt')or task['StartedAt'].startswith('0001-'):raise ValueError('LINEAGE_RUNNING_TARGET_REQUIRED')
 source,old,old_owner=local(client,previous)
 if old['allocation']!=allocation:raise ValueError('LINEAGE_SOURCE_ALLOCATION_MISMATCH')
 if kind=='private_copy':
  proof=intent['recovery_parent'];expected={'recovery_source_allocation':allocation,'recovery_source_session':previous,'recovery_source_key':proof['source_key'],'recovery_copy_sha256':proof['copy_sha256'],'private_copy_id':intent['private_copy_id']}
 else:expected={'resumed_from':allocation}
 if any(meta.get(k)!=v for k,v in expected.items()):raise ValueError('LINEAGE_REMOTE_PROVENANCE_MISMATCH')
 taskcfg=[t.get('Config',{})for g in full['Job'].get('TaskGroups',[])for t in g.get('Tasks',[])if t.get('Name')==client.c['task']and t.get('Driver')=='lima']
 if len(taskcfg)!=1 or taskcfg[0].get('worker_session_id')!=session:raise ValueError('LINEAGE_REMOTE_TASK_MISMATCH')
 if kind=='private_copy'and taskcfg[0].get('private_copy_id')!=intent['private_copy_id']:raise ValueError('LINEAGE_PRIVATE_INPUT_MISMATCH')
 # Project is descriptive. Prefer original source task's verified cached description
 # only when available, otherwise preserve explicit missing status.
 original=intent.get('descriptive_provenance',{});project=meta.get('project','');title=meta.get('source_task_title')or original.get('source_task_title')
 try:
  _,_,_,oldfull,oldmeta,_=observe(client,previous)
  if kind=='resume':
   import resume
   expected_task=resume.plan(client.c,oldfull,session)['TaskGroups'][0]['Tasks'][0]['Config']
   if any(taskcfg[0].get(k)!=expected_task.get(k)for k in ('resume_from_key','resume_generation','resume_node_id','disk_gib','architecture')):raise ValueError('LINEAGE_RESUME_RECEIPT_MISMATCH')
  project=oldmeta.get('project')or project;title=oldmeta.get('source_task_title')or title
 except (RuntimeError,ValueError):
  if kind=='resume':raise ValueError('LINEAGE_RESUME_SOURCE_RECEIPT_UNAVAILABLE_OR_MISMATCH')from None
 record={'schema_version':1,'kind':kind,'previous_session_id':previous,'previous_allocation_id':allocation,'current_session_id':session,'current_allocation_id':full['ID'],'owner':owner,'project':clean(project),'task_title':clean(title),'target_config_sha256':hashlib.sha256(canonical(binding)).hexdigest(),'evidence':'verified_running_nomad_target_and_bound_parent_not_acp_liveness','aliases_historical_only':True}
 folder=client.root/'lineage'
 if folder.is_symlink():raise ValueError('UNSAFE_LINEAGE_DIRECTORY')
 folder.mkdir(exist_ok=True,mode=0o700)
 if folder.stat().st_mode&0o077 or folder.stat().st_uid!=os.getuid():raise ValueError('UNSAFE_LINEAGE_DIRECTORY')
 fd=os.open(folder/(previous+'.lock'),os.O_CREAT|os.O_RDWR|os.O_NOFOLLOW,0o600)
 with os.fdopen(fd,'a')as lock:
  fcntl.flock(lock,fcntl.LOCK_EX);path=folder/(previous+'.json')
  doc=protected(path)if path.exists()or path.is_symlink()else {'schema_version':1,'previous_session_id':previous,'targets':[]}
  if doc.get('schema_version')!=1 or doc.get('previous_session_id')!=previous:raise ValueError('LINEAGE_RECORD_CHANGED')
  rows=doc['targets'];matches=[r for r in rows if r.get('current_session_id')==session]
  if matches and matches!=[record]:raise ValueError('LINEAGE_TARGET_BINDING_CHANGED')
  if not matches:
   rows.append(record);atomic(path,doc)
   for directory in (folder,client.root):
    directory_fd=os.open(directory,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW)
    try:os.fsync(directory_fd)
    finally:os.close(directory_fd)
 return {'indexed':True,'ambiguous':len(rows)!=1,'record':record,'automatic_alias_adoption':False}

def find(client,project,creator_record,all_owned=False):
 if project is not None and not re.fullmatch('[a-z0-9][a-z0-9-]{0,31}',project):raise ValueError('PROJECT_SLUG_REQUIRED')
 native=protected(creator_record)
 if native.get('ended')or native.get('evidence')!='native_hook_observation_not_authenticated_liveness':raise ValueError('CURRENT_NATIVE_CREATOR_REQUIRED')
 mine={'agent_id':native['agent_instance_id'],'native_session_id':native['native_session_id'],'generation':native['generation']}
 uid(mine['agent_id']);uid(mine['generation']);rows=[];unavailable=[];links=[]
 candidates=[p for p in client.root.iterdir()if re.fullmatch('[a-f0-9-]{36}',p.name)]
 if len(candidates)>1024:raise ValueError('DISCOVERY_SESSION_LIMIT')
 for path in sorted(candidates):
  owner=None
  try:
   intent,binding,owner=local(client,path.name)
   if not all_owned and owner!=mine:continue
   intent,binding,owner,full,meta,task=observe(client,path.name)
   row={'session_id':path.name,'allocation_id':full['ID'],'job_id':binding['job'],'target_config':str(path/'target-config.json'),'owner':owner,'mine':owner==mine,'project':clean(meta.get('project')),'task_title':clean(meta.get('source_task_title')),'purpose':clean(meta.get('purpose')),'initiator':{'name':clean(meta.get('source_agent_name')),'type':clean(meta.get('source_agent_type')),'native_session_id':owner['native_session_id']},'state':{'allocation':full.get('ClientStatus'),'desired':full.get('DesiredStatus'),'task':task.get('State'),'session_readiness':'not_probed'},'previous_session_id':parent(intent)[1]if parent(intent)else None,'current_session_id':path.name,'resumed_to':[],'historical_only':False}
   row['task_title_status']='provided'if row['task_title']else'not_provided';row['task_link']=clean(meta.get('task_link'))or None;row['task_link_status']='provided'if row['task_link']else'not_provided'
   row['initiator']['name_status']='observed_or_configured'if row['initiator']['name']else'not_observed_or_configured'
   rows.append(row)
  except (FileNotFoundError,RuntimeError,ValueError,KeyError,OSError):
   if all_owned or owner==mine:unavailable.append({'session_id':path.name,'state':'unavailable_or_unverified','not_proof_of_stopped':True})
 folder=client.root/'lineage'
 if folder.exists()or folder.is_symlink():
  if folder.is_symlink()or folder.stat().st_mode&0o077 or folder.stat().st_uid!=os.getuid():raise ValueError('UNSAFE_LINEAGE_DIRECTORY')
  paths=list(folder.glob('*.json'))
  if len(paths)>1024:raise ValueError('DISCOVERY_LINEAGE_LIMIT')
  for path in paths:
   doc=protected(path)
   if doc.get('schema_version')!=1 or doc.get('previous_session_id')!=path.stem:raise ValueError('LINEAGE_RECORD_CHANGED')
   links.extend(doc['targets'])
 by={r['session_id']:r for r in rows}
 for link in links:
  current=by.get(link['current_session_id'])
  if not current or link.get('owner')!=current['owner']or link.get('current_allocation_id')!=current['allocation_id']:continue
  _,binding,_=local(client,current['session_id'])
  if hashlib.sha256(canonical(binding)).hexdigest()!=link.get('target_config_sha256'):continue
  current['project']=link.get('project')or current['project'];current['task_title']=link.get('task_title')or current['task_title'];current['task_title_status']='provided'if current['task_title']else'not_provided'
  old=by.get(link['previous_session_id'])
  if old:
   old['resumed_to'].append(current['session_id']);old['historical_only']=True
   old['current_session_id']=current['session_id']if len(old['resumed_to'])==1 else None
 for row in rows:
  node=row;seen=set()
  while node['historical_only']and len(node['resumed_to'])==1:
   if node['session_id']in seen:row['current_session_id']=None;break
   seen.add(node['session_id']);node=by[node['resumed_to'][0]];row['current_session_id']=node['current_session_id']
 for row in rows:
  current=by.get(row['current_session_id']);row['current_target_config']=current.get('target_config')if current else None
  if row['historical_only']:row['historical_target_config']=row.pop('target_config')
 rows=[r for r in rows if project is None or r['project']==project]
 choices=[r for r in rows if not r['historical_only']]
 return {'scope':'all_owned'if all_owned else'mine_exact_native_instance_session_generation','project':project,'candidates':rows,'unverified':unavailable,'current_candidate_count':len(choices),'ambiguous':len(choices)>1,'automatic_binding':False,'next_action':'choose_candidate_then_status'if choices else'no_verified_local_candidate','project_is_identity':False,'lookup_is_read_only':True}
