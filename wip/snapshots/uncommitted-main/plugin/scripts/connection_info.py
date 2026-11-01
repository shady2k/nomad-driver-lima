"""Read-only human command rendering from existing private access profiles."""
import json,pathlib,re,shlex
import exchange

def connection_info(adapter,session,profile_file=None):
 adapter.require_initiator(session);allocation=adapter.target();meta=allocation.get('Job',{}).get('Meta',{})
 for remote,local in (('creator_agent_id','agent_instance_id'),('creator_session_id','creator_session_id'),('creator_generation','creator_generation')):
  if adapter.c.get(local) is not None and meta.get(remote)!=adapter.c[local]:raise RuntimeError('CONNECTION_OWNER_BINDING_CHANGED')
 base={'session_id':session,'allocation_id':allocation['ID'],'node_id':allocation.get('NodeID'),'access_verified':False,'access_grants_changed':False,'ssh_executed':False,'command':None}
 def unavailable(fields,reason='access_profile_missing_or_incomplete'):return {**base,'status':'unavailable','reason':reason,'missing_fields':fields}
 if allocation.get('ClientStatus')!='running':return unavailable(['running_allocation'],'allocation_not_running_physical_vm_status_unknown')
 state_path=adapter.local(session)/'session.json'
 if state_path.exists():
  state=adapter.load(session)
  if state.get('attempt')!=adapter.attempt(allocation):raise RuntimeError('CONNECTION_ATTEMPT_CHANGED')
 else:
  intent_path=adapter.local(session)/'allocation-intent.json'
  if intent_path.is_symlink()or not intent_path.is_file():return unavailable(['session_or_allocation_intent'],'owner_attempt_evidence_missing')
  intent=json.loads(intent_path.read_text())
  if intent.get('session_id')!=session or intent.get('job')!=adapter.c['job']or intent.get('creator_agent_id')!=adapter.c.get('agent_instance_id')or intent.get('creator_session_id')!=adapter.c.get('creator_session_id'):raise RuntimeError('CONNECTION_INTENT_CHANGED')
 task=allocation.get('TaskStates',{}).get(adapter.c['task'],{});instances={e.get('Details',{}).get('instance')for e in task.get('Events',[])if isinstance(e,dict)and isinstance(e.get('Details'),dict)and e['Details'].get('instance')}
 if len(instances)!=1:return unavailable(['authoritative_driver_instance'],'driver_instance_missing_or_ambiguous')
 instance=next(iter(instances))
 if not isinstance(instance,str)or not re.fullmatch('[a-z0-9][a-z0-9-]{0,63}',instance):raise RuntimeError('INVALID_DRIVER_INSTANCE')
 path=profile_file or adapter.c.get('connection_profiles_file')
 if not path:return unavailable(['node_access_profile','human_ssh_alias','verified_runtime_shell_environment'])
 profiles=exchange.protected(path)
 if set(profiles)!={'schema_version','nodes'}or profiles['schema_version']!=1 or not isinstance(profiles['nodes'],dict):raise ValueError('CONNECTION_PROFILE_SCHEMA')
 profile=profiles['nodes'].get(base['node_id'])
 if not profile:return unavailable(['node_access_profile'])
 if not isinstance(profile,dict)or set(profile)-{'id','ssh_alias','shell_argv','environment_verified'}:raise ValueError('CONNECTION_PROFILE_SCHEMA')
 missing=[name for name in ('id','ssh_alias','shell_argv')if not profile.get(name)]
 if profile.get('environment_verified')is not True:missing.append('verified_runtime_shell_environment')
 if missing:return unavailable(missing)
 alias=profile['ssh_alias'];prefix=profile['shell_argv']
 if not isinstance(alias,str)or not re.fullmatch('[A-Za-z0-9][A-Za-z0-9._-]{0,63}',alias):raise ValueError('EXISTING_SSH_ALIAS_REQUIRED')
 if not isinstance(prefix,list)or not 1<=len(prefix)<=16 or prefix.count('{instance}')!=1 or any(not isinstance(x,str)or not x or len(x)>512 or any(ord(c)<32 for c in x)for x in prefix):raise ValueError('FIXED_NATIVE_SHELL_PROFILE_REQUIRED')
 # Profile is data only. No proxy/key flags, login, shell execution or validation
 # connection is performed; no local/private Lima key/config is fetched.
 if any(flag in x for x in prefix for flag in ('PRIVATE KEY','IdentityFile','VAULT_TOKEN','BAO_TOKEN','LLM_API_KEY')):raise ValueError('CREDENTIAL_FREE_CONNECTION_PROFILE_REQUIRED')
 remote=shlex.join([instance if x=='{instance}'else x for x in prefix]);argv=['ssh','-t',alias,remote]
 return {**base,'status':'available','transport':'existing-ssh-profile','profile_id':profile['id'],'instance':instance,'ssh_argv':argv,'command':shlex.join(argv),'verification':'existing profile configured; no login attempted'}
