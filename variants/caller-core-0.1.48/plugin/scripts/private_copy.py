"""Operator-pinned one-off recovery copies, never shared catalog templates."""
import hashlib,json,pathlib,re,uuid
from catalog_cli import canonical,uid

def load(config):
 p=pathlib.Path(config['private_copies_file']);st=p.lstat()
 if p.is_symlink()or not p.is_file()or st.st_mode&0o077 or st.st_uid!=__import__('os').getuid()or st.st_size>1048576:raise ValueError('PRIVATE_COPY_MANIFEST_PRIVATE_FILE_REQUIRED')
 raw=p.read_bytes()
 if hashlib.sha256(raw).hexdigest()!=config['private_copies_sha256']:raise ValueError('PRIVATE_COPY_MANIFEST_CHANGED')
 m=json.loads(raw)
 if m.get('schema_version')!=1 or type(m.get('max_reserved_disk_gib'))!=int or m['max_reserved_disk_gib']<1 or not 1<=len(m.get('inputs',{}))<=8:raise ValueError('PRIVATE_COPY_MANIFEST_INVALID')
 return m

def plan(config,m,identifier,session,native,cpus,memory,nodes):
 uid(session);v=m['inputs'][identifier]
 if v['id']!=identifier or v['operator_approved']is not True:raise ValueError('PRIVATE_COPY_NOT_APPROVED')
 if native.get('ended')or native.get('evidence')!='native_hook_observation_not_authenticated_liveness':raise ValueError('INITIATOR_RECORD_NOT_ACTIVE')
 for field,key in [('agent_instance_id','creator_agent_id'),('native_session_id','creator_session_id'),('generation','creator_generation')]:
  if native.get(field)!=v[key]:raise ValueError('PRIVATE_COPY_CREATOR_MISMATCH')
 if config['namespace']!=v['namespace']or session==v['source_session']:raise ValueError('PRIVATE_COPY_NEW_SESSION_OR_NAMESPACE_REQUIRED')
 for k in ('node_id','creator_agent_id','creator_generation','source_allocation','source_session'):uid(v[k])
 if not re.fullmatch('[a-f0-9]{64}',v['copy_sha256'])or not re.fullmatch('[a-f0-9]{64}',v['source_key']):raise ValueError('PRIVATE_COPY_DIGEST_REQUIRED')
 if any(type(x)!=int or x<1 for x in (cpus,memory,v['virtual_size_bytes']))or v['virtual_size_bytes']%(1<<30):raise ValueError('PRIVATE_COPY_RESOURCES_INVALID')
 disk=v['virtual_size_bytes']//(1<<30)
 for k,n in [('vcpus',cpus),('memory_mib',memory),('disk_gib',disk)]:
  if config.get('request_limits',{}).get(k,0)>0 and n>config['request_limits'][k]:raise ValueError('REQUEST_EXCEEDS_OPERATOR_POLICY')
 eligible=[n for n in nodes if n['node_id']==v['node_id'] and n.get('placement_eligible') and n.get('architecture')==v['architecture'] and n.get('host_os')=='linux']
 if len(eligible)!=1:raise ValueError('PRIVATE_COPY_NODE_UNAVAILABLE')
 n=eligible[0];per=n['cpu_mhz_per_vcpu'];over=n['memory_overhead_mib_per_vm'];free=n['available_for_one_new_vm']
 from capacity_view import fits_snapshot
 if any(type(x)!=int or x<1 for x in (per,over))or not fits_snapshot(free,cpus,memory,disk):raise ValueError('PRIVATE_COPY_CAPACITY_UNAVAILABLE')
 command=config['guest_bootstrap_command']
 if not isinstance(command,list)or not command or not all(isinstance(x,str)for x in command):raise ValueError('OPERATOR_BOOTSTRAP_COMMAND_REQUIRED')
 vault=config.get('vault',{})
 if vault!={'role':'agent-worker-prime'}or not config.get('guest',{}).get('prime_vault'):raise ValueError('TASK_SCOPED_VAULT_REQUIRED')
 cfg={'command':[x.replace('{session_id}',session)for x in command],'guest_cpus':cpus,'memory_mib':memory,'disk_gib':disk,'architecture':v['architecture'],'guest_os':'linux','worker_session_id':session,'creator_agent_id':v['creator_agent_id'],'creator_session_id':v['creator_session_id'],'private_copy_id':identifier,'private_copy_owner_generation':v['creator_generation'],'purpose':'recover-work','project':identifier[:31],'vault_required':True}
 meta={k:str(cfg[k])for k in ('creator_agent_id','creator_session_id','worker_session_id','purpose','project')}
 meta.update(nomad_workers_owner=config['owner_id'],creator_generation=v['creator_generation'],private_copy_id=identifier,recovery_source_allocation=v['source_allocation'],recovery_source_session=v['source_session'],recovery_source_key=v['source_key'],recovery_copy_sha256=v['copy_sha256'],recovery_provenance='operator_pinned_private_copy_not_catalog_acceptance',source_task_title='Continue preserved workspace; no automatic task replay')
 task={'Name':config['task'],'Driver':'lima','Config':cfg,'Resources':{'CPU':per*cpus,'MemoryMB':memory+over},'Vault':{'Role':vault['role'],'Env':False,'DisableFile':False,'ChangeMode':'noop'},'LogConfig':{'MaxFiles':10,'MaxFileSizeMB':10}}
 return {'ID':'worker-'+session,'Name':'recover-'+identifier+'-'+session[:8],'Namespace':config['namespace'],'Type':'batch','Datacenters':config['datacenters'],'Meta':meta,'Constraints':[{'LTarget':'${node.unique.id}','Operand':'=','RTarget':v['node_id']}],'TaskGroups':[{'Name':'worker','Count':1,'Disconnect':__import__('disconnect_policy').group_strategy(config),'RestartPolicy':{'Attempts':0,'Mode':'fail'},'ReschedulePolicy':{'Attempts':0,'Unlimited':False},'Tasks':[task]}]}
