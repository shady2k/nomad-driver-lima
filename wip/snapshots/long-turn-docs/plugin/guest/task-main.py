#!/usr/bin/env python3
"""Allocation main workload and one-shot ownership cleanup; no transport service."""
import argparse,hashlib,json,os,pathlib,subprocess,time,uuid,stat,signal,urllib.parse,fcntl,contextlib,re

# Future image schema; existing v3 guests keep v1 stage/counter compatibility.
OPERATION_LOG_SCHEMA = 2
class Redactor:
 def __init__(self,values=()):self.values=[];self.add(*values)
 def add(self,*values):
  for value in values:
   if isinstance(value,str) and value:
    for candidate in [value,json.dumps(value)[1:-1],urllib.parse.quote(value,safe=''),urllib.parse.quote_plus(value)]:
     if candidate not in self.values:self.values.append(candidate)
  self.values.sort(key=len,reverse=True)
 def text(self,text):
  for value in self.values:text=text.replace(value,'[REDACTED]')
  text=re.sub(r'(?i)(\b(?:authorization|x-vault-token|x-nomad-token)\s*[:=]\s*)(?:Bearer\s+|Basic\s+)?[^\s,;"\'}]+',r'\1[REDACTED]',text)
  text=re.sub(r'(?i)([?&](?:token|access_token|api_key|key|secret|client_secret|password)=)[^&#\s"\']+',r'\1[REDACTED]',text)
  text=re.sub(r'(https?://)[^/@\s:]+:[^/@\s]+@',r'\1[REDACTED]@',text)
  text=re.sub(r'(?i)([\"\']?\b[A-Z_]*(?:TOKEN|API_KEY|PASSWORD|PASSWD|SECRET|SEED)[\"\']?\s*[:=]\s*)([\"\'])(.*?)\2',r'\1\2[REDACTED]\2',text)
  text=re.sub(r'(?i)(\b[A-Z_]*(?:TOKEN|API_KEY|PASSWORD|PASSWD|SECRET|SEED)\s*=\s*)[^\s,;&?#]+',r'\1[REDACTED]',text)
  text=re.sub(r'\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\b','[REDACTED_JWT]',text)
  # Presigned object URLs are bearer capabilities: remove the whole query,
  # including X-Amz-* fields, not just generic token/key parameter names.
  def safe_url(match):
   value=match.group(0)
   try:
    u=urllib.parse.urlsplit(value)
    if u.query:return urllib.parse.urlunsplit((u.scheme,u.netloc,u.path,'[REDACTED]',''))
   except ValueError:return '[REDACTED_URL]'
   return value
  text=re.sub(r'https?://[^\s\"\'<>]+',safe_url,text)
  return text

def lifecycle_event(event):
 allowed=('component','stage','session_id','request_id','time','bytes','total_bytes','duration_ms')
 out={k:v for k,v in event.items() if k in allowed}
 if event.get('component')!='agent-operation':return out
 operation=event.get('operation_id');label=event.get('label')
 if str(uuid.UUID(operation))!=operation or not isinstance(label,str) or not re.fullmatch('[a-z][a-z0-9-]{0,23}',label):raise ValueError('Invalid operation metadata')
 if event.get('state') not in ('launch_intent','launching','running','completed','failed','cancelled','timed_out','unknown'):raise ValueError('Invalid operation state')
 out.update(operation_id=operation,label=label,state=event['state'])
 if type(event.get('exit_code'))==int:out['exit_code']=event['exit_code']
 if event.get('stream') in ('stdout','stderr') and isinstance(event.get('text'),str):
  out.update(stream=event['stream'],text=Redactor().text(event['text'])[:2048])
 return out

def atomic(path,value):
 path=pathlib.Path(path);tmp=path.with_name(path.name+'.pending-'+str(os.getpid()))
 fd=os.open(tmp,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
 try:
  with os.fdopen(fd,'w') as f:json.dump(value,f);f.flush();os.fsync(f.fileno())
  os.replace(tmp,path)
 finally:
  if tmp.exists():tmp.unlink()
def identity(pid):
 try:
  parts=pathlib.Path('/proc/'+str(int(pid))+'/stat').read_bytes().rsplit(b') ',1)[1].split()
  ticks=parts[19].decode('ascii')
  if not ticks.isdigit():return None
  return None if parts[0]==b'Z' else {'pid':int(pid),'ticks':ticks}
 except (FileNotFoundError,ProcessLookupError,IndexError,ValueError,UnicodeDecodeError):return None
def alive(row):return identity(row['pid'])==row
def private(path):
 path=pathlib.Path(path);path.mkdir(parents=True,exist_ok=True,mode=0o700)
 s=path.lstat()
 if not stat.S_ISDIR(s.st_mode) or s.st_uid!=os.getuid() or stat.S_IMODE(s.st_mode)!=0o700:raise RuntimeError('PRIVATE_RUNTIME_DIRECTORY_REQUIRED')
 return path
def token_root(session):
 if str(uuid.UUID(session))!=session:raise RuntimeError('CANONICAL_SESSION_REQUIRED')
 return private(private('/dev/shm/agent-worker-'+str(os.getuid()))/session)
def process_parent(pid):
 try:return int(pathlib.Path('/proc/'+str(int(pid))+'/stat').read_bytes().rsplit(b') ',1)[1].split()[1])
 except (FileNotFoundError,ProcessLookupError,IndexError,ValueError):return None

def descendants(rows):
 found={r['pid']:r for r in rows if alive(r)};todo=list(found)
 while todo:
  pid=todo.pop();parent=found[pid]
  if not alive(parent):continue
  try:threads=list(pathlib.Path('/proc/'+str(pid)+'/task').iterdir())
  except FileNotFoundError:continue
  if len(threads)>1024:raise RuntimeError('OWNED_THREAD_TREE_LIMIT')
  for thread in threads:
   if not thread.name.isdigit():continue
   try:children=(thread/'children').read_text().split()
   except FileNotFoundError:continue
   for child in children:
    row=identity(int(child))
    # Linux children is per TID. Verify the child's parent TGID and both
    # process identities before accepting it; a reused PID is never owned.
    if row and row['pid'] not in found and alive(parent) and process_parent(row['pid'])==pid and alive(row):
     if len(found)>=256:raise RuntimeError('OWNED_PROCESS_TREE_LIMIT')
     found[row['pid']]=row;todo.append(row['pid'])
 return list(found.values())

def listening_socket_inodes(path):
 try:
  st=path.lstat()
  if not stat.S_ISSOCK(st.st_mode) or st.st_uid!=os.getuid():raise RuntimeError('PRIVATE_DAEMON_SOCKET_REQUIRED')
 except FileNotFoundError:return set()
 inodes=set()
 for line in pathlib.Path('/proc/net/unix').read_text().splitlines()[1:]:
  fields=line.split(maxsplit=7)
  if len(fields)==8 and fields[7]==str(path) and fields[4]=='0001' and int(fields[3],16)&0x10000:inodes.add(fields[6])
 return inodes

def socket_owned_by(row,path):
 if not row or not alive(row):return False
 try:
  # Filesystem inode and kernel socket inode differ. Match the listening
  # /proc/net/unix record to an FD held by this exact daemon process.
  inodes=listening_socket_inodes(path)
  if not inodes:return False
  for fd in pathlib.Path('/proc/'+str(row['pid'])+'/fd').iterdir():
   try:target=os.readlink(fd)
   except FileNotFoundError:continue
   if target in {'socket:['+x+']' for x in inodes}:return alive(row)
 except (FileNotFoundError,ProcessLookupError):return False
 return False

def readiness_failure(directory,session,root,row,daemon,supervisor,code):
 # Allowlisted booleans/counters only. Never argv, environment, tokens,
 # provider URL, supervisor output or raw exception text.
 def state(path,kind):
  try:
   st=path.lstat();return {'present':True,'owner_matches':st.st_uid==os.getuid(),'kind_matches':kind(st.st_mode),'private':stat.S_IMODE(st.st_mode)&0o077==0}
  except FileNotFoundError:return {'present':False}
 try:owned=len(descendants([row]))
 except Exception:owned=None
 flags={'code':code,'supervisor_alive':alive(row),'supervisor_exit_code':supervisor.poll(),'owned_process_count':owned,'daemon_observed':daemon is not None,'daemon_alive':bool(daemon and alive(daemon)),'token':state(root/'vault_token',stat.S_ISREG),'socket':state(root/'prime.sock',stat.S_ISSOCK)}
 try:flags['socket_owned_by_daemon']=socket_owned_by(daemon,root/'prime.sock')
 except Exception:flags['socket_owned_by_daemon']=False
 atomic(directory/'runtime-readiness-diagnostics.json',flags)
 print(json.dumps({'component':'agent-task','stage':'prime_daemon_readiness_failed','session_id':session,'time':time.time(),**flags}),flush=True)

def _stop_runtime(root):
 root=private(root);owner=root/'runtime-owner.json';intent=root/'runtime-start-intent.json'
 if not owner.exists():
  if intent.exists():raise RuntimeError('RUNTIME_START_OUTCOME_UNKNOWN')
  rows=[]
 else:
  state=json.loads(owner.read_text());rows=descendants(state['processes'])
 # Preserve exact identities for evidence; never signal a reused PID.
 for row in rows:
  if alive(row):
   try:os.kill(row['pid'],signal.SIGTERM)
   except ProcessLookupError:pass
 deadline=time.monotonic()+15
 while (any(alive(row) for row in rows) or listening_socket_inodes(root/'prime.sock')) and time.monotonic()<deadline:time.sleep(.05)
 if any(alive(row) for row in rows) or listening_socket_inodes(root/'prime.sock'):raise RuntimeError('OWNED_RUNTIME_STOP_UNCONFIRMED')
 atomic(root/'runtime-stopped.json',{'stopped':True,'processes':rows})
 return rows

def native_model(provider):
 # Prime 0.10.1 custom.rs ModelDefinition; deliberately no endpoint/auth/header overrides.
 model=provider['model'];budget=provider['max_tokens'];context=provider['context_window']
 for n in (budget,context):
  if type(n)!=int or not 1<=n<=2000000:raise RuntimeError('MODEL_BUDGET_REQUIRED')
 d=provider.get('model_definition')
 metadata={'id':model,'name':model,'api':'openai-completions','reasoning':False,'input':['text'],'contextWindow':context,'maxTokens':budget}
 if d is None:return metadata,None
 allowed={'id','name','api','reasoning','input','contextWindow','maxTokens','thinkingLevelMap','compat'}
 if not isinstance(d,dict) or set(d)-allowed:raise RuntimeError('NATIVE_MODEL_FIELD_UNSUPPORTED')
 if d.get('id')!=model or d.get('api')!='openai-completions' or d.get('input')!=['text']:raise RuntimeError('NATIVE_MODEL_IDENTITY_MISMATCH')
 if type(d.get('reasoning'))!=bool:raise RuntimeError('NATIVE_MODEL_REASONING_REQUIRED')
 if not isinstance(d.get('name',model),str) or not 0<len(d.get('name',model))<256:raise RuntimeError('NATIVE_MODEL_NAME_INVALID')
 for key,value in (('maxTokens',budget),('contextWindow',context)):
  ceiling=d.get(key)
  if type(ceiling)!=int or not value<=ceiling<=2000000:raise RuntimeError('TASK_BUDGET_EXCEEDS_MODEL_CAPABILITY')
 levels=d.get('thinkingLevelMap')
 if levels is not None:
  if not isinstance(levels,dict) or not levels or set(levels)-{'off','minimal','low','medium','high','xhigh','max'}:raise RuntimeError('NATIVE_THINKING_MAP_INVALID')
  if any(v is not None and (not isinstance(v,str) or not v or len(v)>64 or any(ord(c)<32 for c in v)) for v in levels.values()):raise RuntimeError('NATIVE_THINKING_MAP_INVALID')
  metadata['thinkingLevelMap']=dict(levels)
 compat=d.get('compat')
 if compat is not None:
  booleans={'zaiToolStream','sendSessionAffinityHeaders','supportsStore','supportsDeveloperRole','supportsReasoningEffort','supportsUsageInStreaming','requiresToolResultName','requiresAssistantAfterToolResult','requiresThinkingAsText','requiresReasoningContentOnAssistantMessages','supportsStrictMode','supportsLongCacheRetention'}
  enums={'maxTokensField':{'max_completion_tokens','max_tokens'},'thinkingFormat':{'openai','openrouter','deepseek','zai','qwen','qwen-chat-template'},'cacheControlFormat':{'anthropic'}}
  if not isinstance(compat,dict) or set(compat)-booleans-set(enums):raise RuntimeError('NATIVE_COMPAT_FIELD_UNSUPPORTED')
  for k,v in compat.items():
   if (k in booleans and type(v)!=bool) or (k in enums and (not isinstance(v,str) or v not in enums[k])):raise RuntimeError('NATIVE_COMPAT_VALUE_INVALID')
 metadata.update(name=d.get('name',model),reasoning=d['reasoning'])
 # custom.rs builds new custom models from PROVIDER compat, not model_def.compat.
 return metadata,dict(compat) if compat is not None else None

def native_gateway(provider,session):
 # Official models.json supports provider-level compat for custom definitions.
 # One gateway/key; deterministic internal groups preserve each model's controls.
 models=provider.get('model_definitions')
 if models is None:
  metadata,compat=native_model(provider)
  p={'baseUrl':provider['base_url'],'apiKey':'LLM_API_KEY','api':'openai-completions','headers':{'x-opencode-session':session},'models':[metadata]}
  if compat is not None:p['compat']=compat
  return {'providers':{provider['name']:p}},{'provider':provider['name'],'model':provider['model']}
 if not isinstance(models,list) or not 1<=len(models)<=64 or 'model_definition' in provider:raise RuntimeError('NATIVE_CATALOG_SHAPE')
 providers={};selected=None;seen=set()
 for definition in models:
  if not isinstance(definition,dict) or not isinstance(definition.get('id'),str) or not definition['id'] or definition['id'] in seen:raise RuntimeError('NATIVE_CATALOG_DUPLICATE_OR_ID')
  seen.add(definition['id']);candidate=dict(provider,model=definition['id'],model_definition=definition,max_tokens=definition.get('maxTokens'),context_window=definition.get('contextWindow'));candidate.pop('model_definitions',None)
  metadata,compat=native_model(candidate);compat=compat or {}
  group=provider['name'][:80]+'-compat-'+hashlib.sha256(json.dumps(compat,sort_keys=True,separators=(',',':')).encode()).hexdigest()[:12]
  p=providers.setdefault(group,{'baseUrl':provider['base_url'],'apiKey':'LLM_API_KEY','api':'openai-completions','headers':{'x-opencode-session':session},'compat':compat,'models':[]})
  if p['compat']!=compat:raise RuntimeError('NATIVE_COMPAT_COLLISION')
  p['models'].append(metadata)
  if definition['id']==provider['model']:
   if provider['max_tokens']!=metadata['maxTokens'] or provider['context_window']!=metadata['contextWindow']:raise RuntimeError('MODEL_CAPABILITY_SUBSTITUTION_NOT_ALLOWED')
   selected={'provider':group,'model':definition['id']}
 if selected is None:raise RuntimeError('COORDINATOR_MODEL_NOT_IN_CATALOG')
 return {'providers':providers},selected

def _start_runtime(directory,session,request):
 c=request['config'];v=c['prime_vault'];root=token_root(session)
 if set(v)!={'address','secret_path','bao_bin','kernel_python','provider'}:raise RuntimeError('PRIME_VAULT_CONFIG_SHAPE')
 if set(v['provider']) not in ({'name','model','base_url','max_tokens','context_window'},{'name','model','base_url','max_tokens','context_window','model_definition'},{'name','model','base_url','max_tokens','context_window','model_definitions'}):raise RuntimeError('PROVIDER_CONFIG_SHAPE')
 if (root/'runtime-stopped.json').exists() or (root/'sealed').exists():raise RuntimeError('RUNTIME_FINALIZED_NO_RESTART')
 if (root/'runtime-start-intent.json').exists():raise RuntimeError('RUNTIME_START_NO_REPLAY')
 u=urllib.parse.urlsplit(v['address'])
 if u.scheme!='https' or not u.hostname or u.username or u.password or u.query or u.fragment or u.path not in ('','/'):raise RuntimeError('BAO_HTTPS_ORIGIN_REQUIRED')
 secret=v['secret_path']
 import re
 if not isinstance(secret,str) or not re.fullmatch('[-A-Za-z0-9_/]{1,255}',secret) or '..' in secret:raise RuntimeError('EXACT_BAO_SECRET_PATH_REQUIRED')
 prime=c['agent_argv'][0];bao=v['bao_bin'];kernel=v['kernel_python']
 if any(not pathlib.Path(x).is_absolute() for x in (prime,bao,kernel)):raise RuntimeError('ABSOLUTE_RUNTIME_BINARIES_REQUIRED')
 token=root/'vault_token';s=token.lstat()
 if not stat.S_ISREG(s.st_mode) or s.st_uid!=os.getuid() or stat.S_IMODE(s.st_mode)!=0o600:raise RuntimeError('PRIVATE_TASK_TOKEN_REQUIRED')
 provider=v['provider'];name=provider['name'];model=provider['model']
 if not re.fullmatch('[-A-Za-z0-9_.]{1,128}',name) or not isinstance(model,str) or not 0<len(model)<256:raise RuntimeError('PROVIDER_MODEL_REQUIRED')
 pu=urllib.parse.urlsplit(provider['base_url'])
 if pu.scheme!='https' and not (pu.scheme=='http' and pu.hostname in ('127.0.0.1','::1')):raise RuntimeError('PROVIDER_HTTPS_REQUIRED')
 if pu.username or pu.password or pu.query or pu.fragment:raise RuntimeError('PROVIDER_URL_INVALID')
 for key in ('max_tokens','context_window'):
  if type(provider[key])!=int or not 1<=provider[key]<=2000000:raise RuntimeError('MODEL_BUDGET_REQUIRED')
 private(c['workspace']);configdir=private(directory/'prime-config');sessions=private(directory/'prime-sessions');socket=root/'prime.sock'
 native_config,coordinator=native_gateway(provider,session)
 atomic(configdir/'models.json',native_config)
 models_sha256=hashlib.sha256((configdir/'models.json').read_bytes()).hexdigest()
 env={'PATH':'/usr/local/bin:/usr/bin:/bin','HOME':str(directory),'PI_OFFLINE':'1','DO_NOT_TRACK':'1','PRIME_AGENT_CODING_AGENT_DIR':str(configdir),'PRIME_AGENT_SESSION_DIR':str(sessions),'PRIME_AGENT_KERNEL_PYTHON':kernel,'PRIME_AGENT_DAEMON_SOCKET':str(socket)}
 q=json.dumps;template='{{ with secret "'+secret+'" }}{{ .Data.data.LLM_API_KEY }}{{ end }}'
 hcl='vault { address = '+q(v['address'])+' }\nauto_auth { method "token_file" { config = { token_file_path = '+q(str(token))+' } } }\nenv_template "LLM_API_KEY" { contents = '+q(template)+'\n error_on_missing_key = true }\nexec { command = '+q([prime,'--mode','daemon','--daemon-socket',str(socket)])+'\n restart_on_secret_changes = "never" }\ntemplate_config { exit_on_retry_failure = true }\n'
 hclpath=root/'prime-agent.hcl';fd=os.open(hclpath,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
 with os.fdopen(fd,'w') as f:f.write(hcl)
 atomic(root/'runtime-start-intent.json',{'config_hash':request['config_hash']})
 supervisor=subprocess.Popen([bao,'agent','-config='+str(hclpath),'-log-level=error'],stdin=subprocess.DEVNULL,stdout=None,stderr=None,env=env,start_new_session=True)
 row=identity(supervisor.pid)
 if row is None:raise RuntimeError('BAO_SUPERVISOR_DIED')
 atomic(root/'runtime-owner.json',{'processes':[row],'config_hash':request['config_hash']})
 deadline=time.monotonic()+30;daemon=None
 try:
  while time.monotonic()<deadline:
   if supervisor.poll() is not None:raise RuntimeError('BAO_SUPERVISOR_DIED')
   daemon=None
   for candidate in descendants([row]):
    try:
     exe=os.path.realpath('/proc/'+str(candidate['pid'])+'/exe');argv=pathlib.Path('/proc/'+str(candidate['pid'])+'/cmdline').read_bytes().split(b'\0')
     if exe==os.path.realpath(prime) and b'--mode' in argv and b'daemon' in argv and str(socket).encode() in argv and alive(candidate):daemon=candidate;break
    except FileNotFoundError:continue
   if daemon and socket_owned_by(daemon,socket):break
   time.sleep(.05)
  if not daemon or not socket_owned_by(daemon,socket):raise RuntimeError('PRIME_DAEMON_READINESS_TIMEOUT')
  ss=socket.lstat()
  if not stat.S_ISSOCK(ss.st_mode) or ss.st_uid!=os.getuid() or not alive(row) or not alive(daemon):raise RuntimeError('PRIVATE_DAEMON_SOCKET_REQUIRED')
 except Exception as e:
  code=str(e) if re_safe(str(e)) else 'READINESS_INSPECTION_FAILED'
  readiness_failure(directory,session,root,row,daemon,supervisor,code)
  raise
 ready={'config_hash':request['config_hash'],'processes':[row,daemon],'socket':str(socket),'socket_dev':ss.st_dev,'socket_ino':ss.st_ino,'env':env,'controller':identity(os.getpid()),'phase':'ready','coordinator':coordinator,'models_sha256':models_sha256}
 atomic(root/'runtime-owner.json',ready);atomic(directory/'runtime-ready.json',ready)
 return root

@contextlib.contextmanager
def runtime_lock(root):
 root=private(root)
 fd=os.open(root/'runtime.lock',os.O_RDWR|os.O_CREAT|os.O_NOFOLLOW,0o600)
 with os.fdopen(fd,'r+') as lock:
  fcntl.flock(lock,fcntl.LOCK_EX)
  yield

def stop_runtime(root):
 with runtime_lock(root):return _stop_runtime(root)
def start_runtime(directory,session,request):
 with runtime_lock(token_root(session)):return _start_runtime(directory,session,request)

def main():
 p=argparse.ArgumentParser();p.add_argument('--state-dir',required=True);p.add_argument('--session',required=True);p.add_argument('--runtime-check',default='/usr/local/libexec/agent-base-check');a=p.parse_args()
 assert str(uuid.UUID(a.session))==a.session
 root=private(a.state_dir);d=private(root/a.session)
 def emit(stage,**fields):print(json.dumps({'component':'agent-task','stage':stage,'session_id':a.session,'time':time.time(),**fields}),flush=True)
 emit('runtime_check_started');r=subprocess.run([a.runtime_check],stdin=subprocess.DEVNULL)
 if r.returncode:emit('runtime_check_failed',exit_code=r.returncode);return r.returncode
 emit('guest_runtime_ready',acp_session_ready=False)
 cancelled=False
 def cancel(*args):
  nonlocal cancelled
  cancelled=True
 signal.signal(signal.SIGTERM,cancel);signal.signal(signal.SIGINT,cancel)
 pos=0;log=d/'lifecycle.jsonl';finish=d/'finish.json';started=False;failed=False
 try:
  while not cancelled:
   request=d/'runtime-request.json'
   if request.exists() and not started:
    emit('bao_prime_runtime_starting');start_runtime(d,a.session,json.loads(request.read_text()));started=True;emit('prime_daemon_ready',acp_session_ready=False)
   if started:
    ready=json.loads((d/'runtime-ready.json').read_text())
    if not all(alive(x) for x in ready['processes']):raise RuntimeError('OWNED_PRIME_DAEMON_DIED_NO_RESTART')
    ss=pathlib.Path(ready['socket']).lstat()
    if (ss.st_dev,ss.st_ino)!=(ready['socket_dev'],ready['socket_ino']):raise RuntimeError('DAEMON_SOCKET_REPLACED_NO_RESTART')
   if log.exists():
    with log.open() as f:
     f.seek(pos)
     for line in f:
      try:
       event=json.loads(line);stage=event['stage']
       if not isinstance(stage,str) or not stage.replace('_','').isalnum():raise ValueError()
       print(json.dumps(lifecycle_event(event)),flush=True)
      except (ValueError,KeyError):emit('lifecycle_record_invalid')
     pos=f.tell()
   if finish.exists():
    result=json.loads(finish.read_text())
    if result.get('session_id')!=a.session or result.get('export_verified') is not True:raise RuntimeError('FINISH_RECEIPT_REQUIRED')
    emit('finish_acknowledged',manifest_sha256=result['manifest_sha256']);break
   time.sleep(.1)
 except Exception as e:
  failed=True;atomic(d/'runtime-failed.json',{'code':str(e) if re_safe(str(e)) else 'RUNTIME_FAILURE','exception_type':type(e).__name__});emit('prime_runtime_failed')
 finally:
  try:
   volatile=pathlib.Path('/dev/shm/agent-worker-'+str(os.getuid()))/a.session
   if volatile.exists():stop_runtime(volatile);emit('owned_prime_bao_stopped')
  except Exception:failed=True;emit('owned_runtime_stop_unconfirmed')
 return 1 if failed else (143 if cancelled else 0)
def re_safe(value):
 import re
 return re.fullmatch('[A-Z_]{1,128}',value) is not None
if __name__=='__main__':raise SystemExit(main())
