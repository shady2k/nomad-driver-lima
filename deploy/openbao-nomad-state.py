"""Exact neutral OpenBao JWT trust resources; metadata-only check; no KV values."""
import datetime,json,os,pathlib,stat,sys,urllib.error,urllib.request,re
spec_file,mode=sys.argv[1:];spec=json.loads(pathlib.Path(spec_file).read_text());assert mode in ('check','apply')
assert spec['mount']=='nomad-workload' and spec['role']=='agent-worker-prime' and spec['policy']=='agent-worker-llm-read'
llm_path=spec['llm_data_path'];assert re.fullmatch('[A-Za-z0-9_-]+/data/[A-Za-z0-9_/-]+',llm_path)
policy_rules='path '+json.dumps(llm_path)+' { capabilities = ["read"] }\n'+'''path "auth/token/lookup-self" { capabilities = ["read"] }
path "auth/token/renew-self" { capabilities = ["update"] }
'''
previous_policy_rules=policy_rules
policy_rules+='path "auth/token/revoke-self" { capabilities = ["update"] }\n'
assert spec['policy_rules']==policy_rules,'Only LLM record and own-token lifecycle paths are permitted'
role=spec['role_payload'];assert role['role_type']=='jwt' and role['bound_audiences']==['vault.io'] and role['bound_claims']=={'nomad_namespace':'agent-workers','nomad_job_id':'worker-*','nomad_task':'worker'} and role['bound_claims_type']=='glob' and role['token_policies']==[spec['policy']] and role['token_no_default_policy'] is True
from urllib.parse import urlsplit
jwks=spec['jwt_config'];u=urlsplit(jwks['jwks_url']);assert set(jwks)=={'jwks_url','jwt_supported_algs'}and jwks['jwt_supported_algs']==['RS256']and u.scheme=='https'and u.hostname and u.path=='/.well-known/jwks.json'and not u.username and not u.password and not u.query and not u.fragment
if mode=='apply':assert spec.get('integration_contract_confirmed') is True,'core config/driver contract must be confirmed before apply'
p=pathlib.Path(spec['admin_token_file']);st=p.stat();assert st.st_uid==os.getuid() and stat.S_IMODE(st.st_mode)==0o600 and not p.is_symlink();token=p.read_text().strip()
def call(path,method='GET',value=None,missing=False):
 req=urllib.request.Request(spec['address']+'/v1/'+path,headers={'X-Vault-Token':token,'Content-Type':'application/json'},method=method,data=None if value is None else json.dumps(value).encode())
 try:
  with urllib.request.urlopen(req,timeout=15) as f:
   raw=f.read();return json.loads(raw) if raw else None
 except urllib.error.HTTPError as e:
  if missing and e.code==404:return None
  raise RuntimeError('OpenBao '+method+' '+path+' HTTP '+str(e.code)) from None
mounts=call('sys/auth')['data'];mount=mounts.get(spec['mount']+'/')
if mount:assert mount['type']=='jwt','Existing auth mount type differs; refuse modification'
policy=call('sys/policies/acl/'+spec['policy'],missing=True)
if policy:assert policy['data']['policy'] in (previous_policy_rules,policy_rules),'Existing neutral policy differs; review drift'
config=call('auth/'+spec['mount']+'/config',missing=True) if mount else None
current_role=call('auth/'+spec['mount']+'/role/'+spec['role'],missing=True) if mount else None
record=call(llm_path.replace('/data/','/metadata/',1),missing=True)
def matches(v,wanted):return bool(v) and all(v['data'].get(k)==x for k,x in wanted.items())
changes=[]
if not mount:changes.append('auth_mount')
if not matches(config,spec['jwt_config']):changes.append('jwks_trust')
if not policy or policy['data']['policy']!=policy_rules:changes.append('llm_policy')
if not matches(current_role,role):changes.append('bound_role')
if mode=='apply' and changes:
 root=pathlib.Path(spec['backup_dir']);root.mkdir(mode=0o700,parents=True,exist_ok=True)
 p=root/'resources.before.json'
 if not p.exists():p.write_text(json.dumps({'mount':mount,'config':config,'policy':policy,'role':current_role},indent=2)+'\n');p.chmod(0o600)
 if not mount:call('sys/auth/'+spec['mount'],'POST',{'type':'jwt','description':'Nomad allocation workload identity'})
 if 'jwks_trust' in changes:call('auth/'+spec['mount']+'/config','POST',spec['jwt_config'])
 if 'llm_policy' in changes:call('sys/policies/acl/'+spec['policy'],'PUT',{'policy':spec['policy_rules']})
 if 'bound_role' in changes:call('auth/'+spec['mount']+'/role/'+spec['role'],'POST',role)
 assert matches(call('auth/'+spec['mount']+'/config'),spec['jwt_config'])
 assert matches(call('auth/'+spec['mount']+'/role/'+spec['role']),role)
 assert call('sys/policies/acl/'+spec['policy'])['data']['policy']==spec['policy_rules']
result={'mode':mode,'changed':bool(changes),'changes':changes,'mount':spec['mount'],'role':spec['role'],'policy':spec['policy'],'llm_record_metadata_exists':record is not None,'business_secret_values_read':False,'tokens_created':False,'other_auth_mounts_unchanged':True,'readback_verified':mode=='apply'}
if mode=='apply':
 root=pathlib.Path(spec['backup_dir']);root.mkdir(mode=0o700,parents=True,exist_ok=True)
 p=root/'apply-receipt.json';p.write_text(json.dumps(result,indent=2)+'\n');p.chmod(0o600)
print(json.dumps(result))
