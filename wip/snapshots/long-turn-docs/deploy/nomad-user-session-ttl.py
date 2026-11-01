"""Change only one existing OIDC MaxTokenTTL through a partial native API request.
No token enumeration, login, secret output, provider/role/config replacement.
"""
import argparse,json,pathlib,subprocess,os,tempfile,re,urllib.parse
p=argparse.ArgumentParser();p.add_argument('--cli',required=True);p.add_argument('--method',required=True);p.add_argument('--ttl',default='720h');p.add_argument('--backup-dir',required=True);a=p.parse_args()
assert pathlib.Path(a.cli).is_absolute() and pathlib.Path(a.backup_dir).is_absolute()
assert a.ttl=='720h'
assert re.fullmatch('[A-Za-z0-9-]{1,128}',a.method)
template='{{.Name}}|{{.Type}}|{{.MaxTokenTTL}}|{{.Default}}|{{.TokenLocality}}'
def inspect():
 r=subprocess.run([a.cli,'acl','auth-method','info','-t',template,a.method],capture_output=True,text=True,timeout=15)
 if r.returncode:raise RuntimeError('OIDC_METHOD_METADATA_UNAVAILABLE')
 parts=r.stdout.strip().split('|');assert len(parts)==5 and parts[0]==a.method and parts[1]=='OIDC'
 return dict(zip(['name','type','max_token_ttl','default','locality'],parts))
before=inspect();changed=before['max_token_ttl']!='720h0m0s'
if changed:
 root=pathlib.Path(a.backup_dir);root.mkdir(parents=True,exist_ok=True,mode=0o700)
 assert not root.is_symlink() and root.stat().st_uid==os.getuid() and root.stat().st_mode&0o077==0
 backup=root/'auth-method-before.redacted.json'
 if not backup.exists():
  fd=os.open(backup,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
  with os.fdopen(fd,'w') as f:json.dump(before,f);f.flush();os.fsync(f.fileno())
 # Server ACLAuthMethod.Merge retains existing Config only when omitted.
 # Default is NOT merged by Nomad: preserve that safe metadata field explicitly.
 assert before['default'] in ('true','false')
 payload={'Name':a.method,'MaxTokenTTL':a.ttl,'Default':before['default']=='true'}
 assert set(payload)=={'Name','MaxTokenTTL','Default'} and 'Config' not in payload
 result=subprocess.run([a.cli,'operator','api','-X','PUT','/v1/acl/auth-method/'+urllib.parse.quote(a.method,safe='')],input=json.dumps(payload),text=True,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=20)
 if result.returncode:raise RuntimeError('OIDC_TTL_UPDATE_REJECTED_OR_UNKNOWN_RECHECK_METHOD_NO_GLOBAL_FALLBACK')
after=inspect()
assert after['max_token_ttl']=='720h0m0s'
assert {k:v for k,v in before.items() if k!='max_token_ttl'}=={k:v for k,v in after.items() if k!='max_token_ttl'}
print(json.dumps({'changed':changed,'method':a.method,'before':before,'after':after,'update_contract':'partial_api_Name_MaxTokenTTL_preserved_Default_no_Config','binding_rules_modified':False,'provider_configuration_replaced':False,'existing_sessions_extended':False,'openbao_workload_modified':False}))
