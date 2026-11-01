"""Operator-owned secret profile compiler. No authentication, token minting or I/O.

This contract stage does not enable secret operations or install profile mappings.
Runtime delivery must bind the catalogue to the Nomad task before using this API.
"""
import hashlib,json,re,urllib.parse

class ProfileError(ValueError):pass

def exact(value,keys,code):
 if not isinstance(value,dict)or set(value)!=set(keys):raise ProfileError(code)
def text(value,pattern,code):
 if not isinstance(value,str)or not re.fullmatch(pattern,value):raise ProfileError(code)
 return value
def absolute(value):
 text(value,r'/[^\x00-\x1f]{1,1023}','ABSOLUTE_PROFILE_PATH_REQUIRED')
 if any(p in ('.','..','')for p in value.split('/')[1:]):raise ProfileError('CANONICAL_PROFILE_PATH_REQUIRED')
 return value
def canonical(value):return json.dumps(value,sort_keys=True,separators=(',',':')).encode()
def digest(value):return hashlib.sha256(canonical(value)).hexdigest()

def validate(catalogue):
 exact(catalogue,('schema_version','profiles'),'SECRET_CATALOGUE_SHAPE')
 if catalogue['schema_version']!=1 or type(catalogue['schema_version'])!=int:raise ProfileError('SECRET_CATALOGUE_VERSION')
 profiles=catalogue['profiles']
 if not isinstance(profiles,list)or not 1<=len(profiles)<=32:raise ProfileError('SECRET_PROFILE_COUNT')
 ids=set()
 for p in profiles:
  exact(p,('id','version','description','scope','clients','bindings'),'SECRET_PROFILE_SHAPE')
  pid=text(p['id'],r'[a-z][a-z0-9-]{0,47}','SECRET_PROFILE_ID')
  if pid in ids:raise ProfileError('DUPLICATE_SECRET_PROFILE')
  ids.add(pid);text(p['version'],r'[A-Za-z0-9][A-Za-z0-9_.-]{0,63}','SECRET_PROFILE_VERSION')
  text(p['description'],r'[^\x00-\x1f]{1,256}','SECRET_PROFILE_DESCRIPTION')
  exact(p['scope'],('namespaces','tasks'),'SECRET_SCOPE_SHAPE')
  for values in p['scope'].values():
   if not isinstance(values,list)or not 1<=len(values)<=32:raise ProfileError('SECRET_SCOPE_BOUND')
   for v in values:text(v,r'[A-Za-z0-9][A-Za-z0-9_.-]{0,127}','EXACT_SECRET_SCOPE_REQUIRED')
  if not isinstance(p['clients'],dict)or not 1<=len(p['clients'])<=16:raise ProfileError('SECRET_CLIENT_BOUND')
  for name,client in p['clients'].items():
   text(name,r'[a-z][a-z0-9-]{0,47}','SECRET_CLIENT_ID');exact(client,('executable','work_root'),'SECRET_CLIENT_SHAPE');absolute(client['executable']);absolute(client['work_root'])
  if not isinstance(p['bindings'],list)or not 1<=len(p['bindings'])<=16:raise ProfileError('SECRET_BINDING_BOUND')
  envs=set()
  for b in p['bindings']:
   exact(b,('record','field','env'),'SECRET_BINDING_SHAPE')
   record=text(b['record'],r'[A-Za-z0-9_-]+(?:/[A-Za-z0-9_-]+){1,15}','EXACT_SECRET_RECORD_REQUIRED')
   text(b['field'],r'[A-Za-z_][A-Za-z0-9_]{0,63}','EXACT_SECRET_FIELD_REQUIRED')
   env=text(b['env'],r'[A-Z_][A-Z0-9_]{0,63}','SECRET_ENV_REQUIRED')
   if env in envs:raise ProfileError('DUPLICATE_SECRET_ENV')
   envs.add(env)
   if env in {'PATH','HOME','SHELL','ENV','BASH_ENV','PYTHONPATH','PYTHONHOME','NODE_OPTIONS','NODE_PATH','BAO_TOKEN','VAULT_TOKEN','NOMAD_TOKEN','LD_PRELOAD','LD_LIBRARY_PATH'}or env.startswith(('LD_','DYLD_','BAO_','VAULT_','NOMAD_')):raise ProfileError('RESERVED_SECRET_ENV')
 return catalogue

def select(catalogue,profile_id,expected_sha256,namespace,task,client_id,cwd,argv):
 validate(catalogue)
 if digest(catalogue)!=expected_sha256:raise ProfileError('SECRET_CATALOGUE_DIGEST_CHANGED')
 found=[p for p in catalogue['profiles']if p['id']==profile_id]
 if not found:raise ProfileError('SECRET_PROFILE_NOT_AUTHORIZED')
 p=found[0]
 if namespace not in p['scope']['namespaces']or task not in p['scope']['tasks']:raise ProfileError('SECRET_PROFILE_TASK_SCOPE_DENIED')
 client=p['clients'].get(client_id)
 if client is None:raise ProfileError('SECRET_CLIENT_NOT_AUTHORIZED')
 absolute(cwd);root=client['work_root']
 if cwd!=root and not cwd.startswith(root+'/'):raise ProfileError('SECRET_CLIENT_WORKSPACE_DENIED')
 if not isinstance(argv,list)or not argv or argv[0]!=client['executable']or any(not isinstance(x,str)or '\0'in x for x in argv)or sum(len(x)for x in argv)>65536:raise ProfileError('SECRET_CLIENT_ARGV_DENIED')
 return p

def compile_supervisor(profile,address,token_file,argv,internal_exec=False):
 """Validated operator-selected profile -> official Agent HCL, containing no values.

Caller must use select(), task-bound trusted input, private tmpfs and safe pipes.
Never call this with a caller-authored record mapping. No listener or token sink.
"""
 validate({'schema_version':1,'profiles':[profile]})
 u=urllib.parse.urlsplit(address)
 if u.scheme!='https'or not u.hostname or u.username or u.password or u.query or u.fragment or u.path not in ('','/'):raise ProfileError('BAO_HTTPS_ORIGIN_REQUIRED')
 absolute(token_file)
 if not isinstance(argv,list)or not argv or(not internal_exec and argv[0]not in {c['executable']for c in profile['clients'].values()})or any(not isinstance(x,str)or '\0'in x for x in argv)or sum(len(x)for x in argv)>65536:raise ProfileError('SECRET_CLIENT_ARGV_DENIED')
 q=json.dumps
 hcl='vault { address = '+q(address)+' }\nauto_auth { method "token_file" { config = { token_file_path = '+q(token_file)+' } } }\n'
 for b in profile['bindings']:
  template='{{ with secret "'+b['record']+'" }}{{ .Data.data.'+b['field']+' }}{{ end }}'
  hcl+='env_template '+q(b['env'])+' { contents = '+q(template)+'\n error_on_missing_key = true }\n'
 return hcl+'exec { command = '+q(argv)+'\n restart_on_secret_changes = "never" }\ntemplate_config { exit_on_retry_failure = true }\n'

def public_profiles(catalogue,namespace,task):
 validate(catalogue)
 return [{'id':p['id'],'version':p['version'],'description':p['description'],'clients':sorted(p['clients']),'delivery':'environment','catalogue_sha256':digest(catalogue)}for p in catalogue['profiles']if namespace in p['scope']['namespaces']and task in p['scope']['tasks']]
