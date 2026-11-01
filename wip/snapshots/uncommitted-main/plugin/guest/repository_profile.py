#!/usr/bin/env python3
"""Optional guest-only anonymous repository configuration. No downloads or login.
All endpoints come from a pinned operator profile; no host-global configuration.
"""
import hashlib,json,os,pathlib,re,stat,sys,tempfile,urllib.parse

class ProfileError(RuntimeError):pass

def canonical(value):return json.dumps(value,sort_keys=True,separators=(',',':')).encode()
def digest(value):return hashlib.sha256(canonical(value)).hexdigest()
def shape(value,required,optional=()):
 if not isinstance(value,dict)or not set(required)<=set(value)or set(value)-set(required)-set(optional):raise ProfileError('REPOSITORY_PROFILE_SHAPE')
def endpoint(value):
 if not isinstance(value,str)or len(value)>2048 or not value.isascii()or any(ord(c)<33 or ord(c)==127 or c.isspace()or c in '\\"\'${},|'for c in value):raise ProfileError('REPOSITORY_HTTPS_ENDPOINT_REQUIRED')
 try:p=urllib.parse.urlsplit(value);port=p.port
 except ValueError:raise ProfileError('REPOSITORY_HTTPS_ENDPOINT_REQUIRED')from None
 if p.scheme!='https'or not p.hostname or p.username is not None or p.password is not None or p.query or p.fragment:raise ProfileError('REPOSITORY_HTTPS_ENDPOINT_REQUIRED')
 return value.rstrip('/')+'/'

def validate(profile,platform):
 if profile is None:return None
 shape(profile,('schema_version','id','version'),('apt','npm','go'))
 if type(profile['schema_version'])is not int or profile['schema_version']!=1:raise ProfileError('REPOSITORY_PROFILE_VERSION')
 if not isinstance(profile['id'],str)or not re.fullmatch('[a-z0-9][a-z0-9-]{0,63}',profile['id'])or not isinstance(profile['version'],str)or not re.fullmatch('[A-Za-z0-9][A-Za-z0-9._-]{0,79}',profile['version']):raise ProfileError('REPOSITORY_PROFILE_IDENTITY')
 if not isinstance(platform,dict)or (platform.get('os'),platform.get('distribution'),platform.get('version'))!=('linux','ubuntu','24.04')or platform.get('architecture')not in ('amd64','arm64'):raise ProfileError('REPOSITORY_PROFILE_UNSUPPORTED_GUEST')
 if not set(profile)&{'apt','npm','go'}:raise ProfileError('EMPTY_REPOSITORY_PROFILE')
 out={k:profile[k]for k in ('schema_version','id','version')}
 if 'apt'in profile:
  apt=profile['apt'];shape(apt,('suite_uris',));mapping=apt['suite_uris']
  if not isinstance(mapping,dict)or not mapping or set(mapping)-{'noble','noble-updates','noble-backports','noble-security'}:raise ProfileError('REPOSITORY_APT_SUITE_MAPPING')
  out['apt']={'suite_uris':{suite:endpoint(url)for suite,url in sorted(mapping.items())}}
 if 'npm'in profile:
  shape(profile['npm'],('registry',));out['npm']={'registry':endpoint(profile['npm']['registry'])}
 if 'go'in profile:
  shape(profile['go'],('proxy',),('distribution_base_url',));out['go']={'proxy':endpoint(profile['go']['proxy']).rstrip('/')}
  if 'distribution_base_url'in profile['go']:out['go']['distribution_base_url']=endpoint(profile['go']['distribution_base_url'])
 return out

def fields(paragraph):
 result={};order=[];last=None
 for line in paragraph.splitlines():
  if not line.strip()or line.lstrip().startswith('#'):continue
  if line[:1].isspace():
   if last is None:raise ProfileError('UNSUPPORTED_APT_SOURCE_FORMAT')
   result[last]+='\n'+line;continue
  match=re.fullmatch('([A-Za-z][A-Za-z0-9-]*):[ \t]*(.*)',line)
  if not match:raise ProfileError('UNSUPPORTED_APT_SOURCE_FORMAT')
  key=match[1].lower()
  if key in result:raise ProfileError('DUPLICATE_APT_SOURCE_FIELD')
  result[key]=match[2];order.append((key,match[1]));last=key
 return result,order

def map_sources(text,mapping):
 # Only the existing official Ubuntu archive keyring qualifies for URI mapping.
 # Other sources, suites and signature options remain unchanged.
 chunks=[];seen=set()
 for paragraph in re.split(r'\n[ \t]*\n',text.strip()):
  row,order=fields(paragraph)
  suites=row.get('suites','').split();matched=set(suites)&set(mapping)
  if not matched:chunks.append(paragraph);continue
  if row.get('signed-by')!='/usr/share/keyrings/ubuntu-archive-keyring.gpg':raise ProfileError('UBUNTU_ARCHIVE_SIGNATURE_REQUIRED')
  if row.get('trusted','no').lower()not in ('no','false')or row.get('allow-insecure','no').lower()not in ('no','false')or row.get('check-valid-until','yes').lower()not in ('yes','true'):raise ProfileError('APT_VERIFICATION_MUST_REMAIN_ENABLED')
  if not row.get('uris')or not row.get('types')or not row.get('components'):raise ProfileError('UNSUPPORTED_APT_SOURCE_FORMAT')
  groups=[];unmapped=[s for s in suites if s not in mapping]
  if unmapped:groups.append((unmapped,row['uris']))
  groups.extend(([suite],mapping[suite])for suite in suites if suite in mapping)
  for group,uri in groups:
   changed=dict(row,uris=uri,suites=' '.join(group));chunks.append('\n'.join(label+': '+changed[key]for key,label in order))
  seen.update(matched)
 return '\n\n'.join(chunks)+'\n',seen

def render(profile,platform,source_parts=None,main_source=''):
 profile=validate(profile,platform)
 if profile is None:return {'enabled':False,'files':{},'directories':[],'environment':{}}
 sha=digest(profile);base='/run/nomad-worker-repository-profiles/'+sha;cache='/tmp/nomad-workers-repository-cache/'+sha
 files={};environment={};directories=[]
 if 'apt'in profile:
  if not isinstance(source_parts,dict)or not source_parts:raise ProfileError('EXISTING_UBUNTU_APT_SOURCES_REQUIRED')
  mapping=profile['apt']['suite_uris'];seen=set()
  for name,text in sorted(source_parts.items()):
   if not re.fullmatch(r'[A-Za-z0-9_-][A-Za-z0-9_.-]*\.(sources|list)',name)or not isinstance(text,str)or len(text)>1048576:raise ProfileError('UNSAFE_APT_SOURCE_FILE')
   if name=='ubuntu.sources':text,found=map_sources(text,mapping);seen.update(found)
   files[base+'/apt/sources.list.d/'+name]=text
  if seen!=set(mapping):raise ProfileError('APT_MAPPED_SUITE_NOT_PRESENT')
  # Main source is copied unchanged; mapping legacy .list syntax is unsupported.
  if not isinstance(main_source,str)or len(main_source)>1048576:raise ProfileError('UNSAFE_APT_SOURCE_FILE')
  files[base+'/apt/sources.list']=main_source
  files[base+'/apt/apt.conf']='\n'.join((
   'Dir::Etc::sourcelist "'+base+'/apt/sources.list";',
   'Dir::Etc::sourceparts "'+base+'/apt/sources.list.d";',
   'Dir::State::lists "'+cache+'/apt/lists";',
   'Acquire::AllowInsecureRepositories "false";',
   'Acquire::AllowDowngradeToInsecureRepositories "false";',
   'Acquire::https::Verify-Peer "true";',
   'Acquire::https::Verify-Host "true";',
   'APT::Get::AllowUnauthenticated "false";',''))
  environment['APT_CONFIG']=base+'/apt/apt.conf';directories.append(cache+'/apt/lists')
 if 'npm'in profile:
  files[base+'/npmrc']='registry='+profile['npm']['registry']+'\nstrict-ssl=true\n'
  environment.update(NPM_CONFIG_USERCONFIG=base+'/npmrc',NPM_CONFIG_REGISTRY=profile['npm']['registry'],NPM_CONFIG_STRICT_SSL='true')
 if 'go'in profile:environment['GOPROXY']=profile['go']['proxy']
 return {'enabled':True,'profile':{k:profile[k]for k in ('id','version')},'profile_sha256':sha,'base':base,'cache':cache,'files':files,'directories':directories,'environment':environment}

def distribution_url(profile,platform,version):
 profile=validate(profile,platform)
 if not profile or not profile.get('go',{}).get('distribution_base_url'):raise ProfileError('GO_DISTRIBUTION_MIRROR_NOT_CONFIGURED')
 if not isinstance(version,str)or not re.fullmatch(r'1\.[0-9]+\.[0-9]+',version):raise ProfileError('EXACT_GO_VERSION_REQUIRED')
 # A recipe must still supply its separately pinned official archive checksum.
 return profile['go']['distribution_base_url']+'go'+version+'.linux-'+platform['architecture']+'.tar.gz'

def safe_path(root,absolute):
 pure=pathlib.PurePosixPath(absolute)
 if not pure.is_absolute()or '..'in pure.parts:raise ProfileError('REPOSITORY_PATH_UNSAFE')
 path=root.joinpath(*pure.parts[1:]);current=root
 for index,part in enumerate(pure.parts[1:]):
  current=current/part
  if current.is_symlink():raise ProfileError('REPOSITORY_PATH_SYMLINK')
  if current.exists()and current.stat().st_uid!=os.geteuid():raise ProfileError('REPOSITORY_PATH_OWNER')
  if current.exists()and index<len(pure.parts)-2 and not current.is_dir():raise ProfileError('REPOSITORY_PARENT_NOT_DIRECTORY')
 return path

def mkdirs(root,path):
 current=root
 for part in path.relative_to(root).parts:
  current=current/part
  if not current.exists():current.mkdir(mode=0o755);current.chmod(0o755)

def write_plan(plan,filesystem_root=pathlib.Path('/')):
 if not plan['enabled']:return {'enabled':False,'changed_files':0,'environment_file':None}
 root=pathlib.Path(filesystem_root)
 if root.is_symlink():raise ProfileError('REPOSITORY_PATH_SYMLINK')
 payloads={**plan['files'],plan['base']+'/environment.json':canonical(plan['environment']).decode()};changes=[]
 # Preflight all paths and existing contents before writing any configuration.
 for absolute,text in payloads.items():
  path=safe_path(root,absolute)
  if path.exists()and (not path.is_file()or path.stat().st_mode&0o222 or path.read_bytes()!=text.encode()):raise ProfileError('REPOSITORY_CONFIG_CHANGED')
 for absolute in [*payloads,*plan['directories']]:safe_path(root,absolute)
 for absolute,text in payloads.items():
  path=safe_path(root,absolute);mkdirs(root,path.parent)
  if not path.exists():
   fd=os.open(path,os.O_CREAT|os.O_EXCL|os.O_WRONLY|os.O_NOFOLLOW,0o600)
   with os.fdopen(fd,'wb')as stream:stream.write(text.encode());stream.flush();os.fsync(stream.fileno())
   path.chmod(0o444);changes.append(absolute)
 for absolute in plan['directories']:mkdirs(root,safe_path(root,absolute))
 return {'enabled':True,'profile':plan['profile'],'profile_sha256':plan['profile_sha256'],'changed_files':len(changes),'environment_file':plan['base']+'/environment.json','environment_sha256':digest(plan['environment'])}

def execution_environment(reference,inherited=None,filesystem_root=pathlib.Path('/'),owner_uid=0):
 # Intended for the existing owned operation launcher immediately before Popen.
 # Only an immutable two-digest reference crosses task/caller configuration.
 if reference is None:return inherited
 shape(reference,('profile_sha256','environment_sha256'))
 if any(not isinstance(v,str)or not re.fullmatch('[a-f0-9]{64}',v)for v in reference.values()):raise ProfileError('REPOSITORY_EXECUTION_REFERENCE')
 base='/run/nomad-worker-repository-profiles/'+reference['profile_sha256'];root=pathlib.Path(filesystem_root);path=root/(base+'/environment.json').lstrip('/')
 current=root
 for part in path.relative_to(root).parts:
  current=current/part
  if current.is_symlink():raise ProfileError('REPOSITORY_PATH_SYMLINK')
 fd=os.open(path,os.O_RDONLY|os.O_NOFOLLOW)
 with os.fdopen(fd,'rb')as stream:
  st=os.fstat(stream.fileno())
  if not stat.S_ISREG(st.st_mode)or st.st_uid!=owner_uid or st.st_mode&0o222 or st.st_size>16384:raise ProfileError('REPOSITORY_ENVIRONMENT_FILE_UNSAFE')
  raw=stream.read(16385)
 if hashlib.sha256(raw).hexdigest()!=reference['environment_sha256']:raise ProfileError('REPOSITORY_ENVIRONMENT_DIGEST_MISMATCH')
 values=json.loads(raw)
 if not isinstance(values,dict)or set(values)-{'APT_CONFIG','NPM_CONFIG_USERCONFIG','NPM_CONFIG_REGISTRY','NPM_CONFIG_STRICT_SSL','GOPROXY'}or any(not isinstance(v,str)or '\x00'in v for v in values.values()):raise ProfileError('REPOSITORY_ENVIRONMENT_SHAPE')
 for name,suffix in (('APT_CONFIG','/apt/apt.conf'),('NPM_CONFIG_USERCONFIG','/npmrc')):
  if name in values and values[name]!=base+suffix:raise ProfileError('REPOSITORY_ENVIRONMENT_PATH')
 if values.get('NPM_CONFIG_STRICT_SSL','true')!='true':raise ProfileError('REPOSITORY_TLS_VERIFICATION_REQUIRED')
 for name in ('NPM_CONFIG_REGISTRY','GOPROXY'):
  if name in values:endpoint(values[name])
 result=dict(os.environ if inherited is None else inherited);result.update(values);return result

def apply_payload(payload):
 profile=payload.get('profile')
 if profile is None:return {'enabled':False,'changed_files':0,'environment_file':None}
 if os.geteuid()!=0:raise ProfileError('GUEST_ROOT_APPLY_REQUIRED')
 identity=payload.get('expected_guest_identity',{})
 if set(identity)!={'machine_id','boot_id'}or identity['machine_id']!=pathlib.Path('/etc/machine-id').read_text().strip()or identity['boot_id']!=pathlib.Path('/proc/sys/kernel/random/boot_id').read_text().strip():raise ProfileError('EXPECTED_GUEST_IDENTITY_REQUIRED')
 mountinfo=pathlib.Path('/proc/self/mountinfo').read_text().splitlines()
 if not any(line.split()[4]=='/run'and line.split(' - ',1)[1].split()[0]=='tmpfs'for line in mountinfo if ' - 'in line):raise ProfileError('GUEST_RUN_TMPFS_REQUIRED')
 release={}
 for line in pathlib.Path('/etc/os-release').read_text().splitlines():
  if re.fullmatch('[A-Z_]+=.*',line):key,value=line.split('=',1);release[key]=value.strip('"')
 arch={'x86_64':'amd64','aarch64':'arm64'}.get(os.uname().machine)
 platform={'os':'linux','distribution':release.get('ID'),'version':release.get('VERSION_ID'),'architecture':arch}
 normalized=validate(profile,platform)
 if digest(normalized)!=payload.get('profile_sha256'):raise ProfileError('REPOSITORY_PROFILE_DIGEST_MISMATCH')
 parts={};main=''
 if 'apt'in normalized:
  folder=pathlib.Path('/etc/apt/sources.list.d')
  if folder.is_symlink():raise ProfileError('APT_SOURCE_SYMLINK')
  for path in folder.iterdir():
   if path.suffix not in ('.sources','.list'):continue
   if path.is_symlink()or not path.is_file():raise ProfileError('APT_SOURCE_SYMLINK')
   parts[path.name]=path.read_text()
  path=pathlib.Path('/etc/apt/sources.list')
  if path.is_symlink():raise ProfileError('APT_SOURCE_SYMLINK')
  if path.exists():main=path.read_text()
 plan=render(normalized,platform,parts,main);result=write_plan(plan)
 return result

if __name__=='__main__':
 try:print(json.dumps(apply_payload(json.load(sys.stdin))))
 except Exception as error:
  # No endpoint, source content, payload or authentication value in diagnostics.
  print(json.dumps({'error':str(error)if isinstance(error,ProfileError)else type(error).__name__}),file=sys.stderr);sys.exit(1)
