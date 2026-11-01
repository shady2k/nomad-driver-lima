"""Finite per-operation guest workload runner, not a service or RPC daemon.
Executed only through existing owned native Nomad exec. No environment snapshots.
"""
import os,sys,json,pathlib,stat,tempfile,fcntl,contextlib,hashlib,time,uuid,subprocess,selectors,signal,base64,re,urllib.parse

def sha(raw):return hashlib.sha256(raw).hexdigest()
def encoded(v):return json.dumps(v,sort_keys=True,separators=(',',':')).encode()
def private(p):
 p=pathlib.Path(p)
 if not p.exists():p.mkdir(mode=0o700)
 st=p.lstat()
 if not stat.S_ISDIR(st.st_mode) or st.st_uid!=os.getuid() or stat.S_IMODE(st.st_mode)!=0o700:raise RuntimeError('UNSAFE_OPERATION_DIRECTORY')
 return p

def read(p):
 fd=os.open(p,os.O_RDONLY|os.O_NOFOLLOW)
 with os.fdopen(fd) as f:
  st=os.fstat(f.fileno())
  if not stat.S_ISREG(st.st_mode) or st.st_uid!=os.getuid() or st.st_mode&0o077:raise RuntimeError('UNSAFE_OPERATION_FILE')
  return json.load(f)

def atomic(p,v):
 fd,tmp=tempfile.mkstemp(prefix='.op-',dir=p.parent)
 try:
  with os.fdopen(fd,'wb') as f:os.fchmod(f.fileno(),0o600);f.write(encoded(v));f.flush();os.fsync(f.fileno())
  os.replace(tmp,p);fd=os.open(p.parent,os.O_RDONLY)
  try:os.fsync(fd)
  finally:os.close(fd)
 finally:
  if os.path.exists(tmp):os.unlink(tmp)

@contextlib.contextmanager
def lock(p,blocking=True):
 fd=os.open(p,os.O_CREAT|os.O_RDWR|os.O_NOFOLLOW,0o600)
 try:
  fcntl.flock(fd,fcntl.LOCK_EX|(0 if blocking else fcntl.LOCK_NB));yield
 finally:os.close(fd)

def identity():return {'machine_id':pathlib.Path('/etc/machine-id').read_text().strip(),'boot_id':pathlib.Path('/proc/sys/kernel/random/boot_id').read_text().strip()}
def process(pid):
 try:
  p=pathlib.Path('/proc')/str(pid);s=(p/'stat').read_text().rsplit(') ',1)[1].split()
  return {'pid':pid,'ticks':s[19],'uid':p.stat().st_uid,'state':s[0]}
 except (OSError,IndexError):return None

def alive(row):
 if not row:return False
 p=process(row['pid']);return p is not None and p['state']!='Z' and all(p.get(k)==row.get(k) for k in ('pid','ticks','uid'))
def guarded(p):
 if str(uuid.UUID(p['session_id']))!=p['session_id']:raise RuntimeError('INVALID_SESSION')
 root=private(p['state_dir']);session=private(root/p['session_id'])
 s=read(session/'session.json')
 if s.get('config')!=p['guest_config'] or s.get('phase')!='ready':raise RuntimeError('GUEST_SESSION_CONFIG_OR_PHASE_CHANGED')
 return session

def bound(p):
 session=guarded(p)
 if identity()!=p['machine_identity']:raise RuntimeError('MACHINE_BOOT_CHANGED_OPERATION_UNKNOWN')
 operation=p['operation_id']
 if str(uuid.UUID(operation))!=operation:raise RuntimeError('INVALID_OPERATION')
 root=private(session/'operations');op=root/operation
 return session,root,op

def verify(p,s):
 if s['binding']!=p['binding'] or s['command_sha256']!=p['command_sha256']:raise RuntimeError('OPERATION_BINDING_MISMATCH')

def status(p,op):
 s=read(op/'state.json');verify(p,s)
 if s['phase'] not in ('completed','failed','cancelled','timed_out') and not (s['phase']=='launch_intent' and time.time()-s['created_at']<10) and not alive(s.get('supervisor')):s={**s,'phase':'unknown','reason':'SUPERVISOR_UNAVAILABLE_NO_REPLAY'}
 return s

def native(session,s,phase,**extra):
 # v1 consumers keep compatible stage/id/duration/byte counters; v2 adds text.
 label=s['label'].replace('-','_')[:20];op=s['operation_id'];suffix=phase
 stage='operation_'+label+'_'+op.replace('-','')[:8]+'_'+suffix
 event={'component':'agent-operation','session_id':s['binding']['session_id'],'request_id':op,'operation_id':op,'label':s['label'],'stage':stage,'state':s['phase'],'time':time.time(),'duration_ms':max(0,int((time.monotonic()-s['_monotonic'])*1000)),'bytes':s.get('log_bytes',0),**extra}
 fd=os.open(session/'lifecycle.jsonl',os.O_WRONLY|os.O_APPEND|os.O_CREAT|os.O_NOFOLLOW,0o600)
 try:os.write(fd,encoded(event)+b'\n');os.fsync(fd)
 finally:os.close(fd)

def progress_line(s,text,now):
 """Recipe-reported progress, never inferred from diagnostic-log size."""
 start=re.fullmatch(r'@@worker-step-start ([a-z][a-z0-9-]{0,23})\s*',text)
 end=re.fullmatch(r'@@worker-step-end ([a-z][a-z0-9-]{0,23}) (-?[0-9]{1,6})\s*',text)
 download=re.fullmatch(r'@@worker-download(?:-final)? ([0-9]{1,19}) (unknown|[0-9]{1,19})\s*',text)
 if start:
  label=start[1]
  if any(x in label for x in ('secret','token','password','bearer')):return '[PROGRESS_MARKER_REJECTED]\n','progress_warning',True
  s.update(current_step=label,step_started_monotonic=now);s.pop('download',None);s.pop('download_last_emit',None)
  return 'Step '+label+' started.\n','step_'+label.replace('-','_')+'_started',True
 if end:
  label=end[1]
  if s.get('current_step')!=label:return '[PROGRESS_MARKER_REJECTED: no matching active step]\n','progress_warning',True
  elapsed=max(0,now-s['step_started_monotonic']);code=int(end[2]);s['last_step']={'label':label,'exit_code':code,'duration_seconds':elapsed};s.pop('current_step',None)
  return 'Step '+label+' finished: exit='+str(code)+', elapsed='+format(elapsed,'.1f')+'s.\n','step_'+label.replace('-','_')+'_finished',True
 if download:
  count=int(download[1]);total=None if download[2]=='unknown'else int(download[2]);previous=s.get('download',{})
  if count>2**63-1 or (total is not None and (total<count or total>2**63-1))or count<previous.get('bytes',0):return '[PROGRESS_MARKER_REJECTED: invalid byte counters]\n','progress_warning',True
  s['download']={'bytes':count,'total_bytes':total};changed=previous.get('bytes')!=count or previous.get('total_bytes')!=total
  final=text.startswith('@@worker-download-final ')
  emit=final or(changed and ('download_last_emit'not in s or now-s['download_last_emit']>=5 or(total is not None and count==total)))
  if emit:s['download_last_emit']=now
  return 'Download '+(s.get('current_step')or s['label'])+': '+str(count)+' bytes / '+('unknown total'if total is None else str(total)+' bytes')+(' (final byte count; see step result)'if final else'')+'.\n','download_progress',emit
 return text,None,True

def quiet_text(s,now,last_output):
 step=s.get('current_step');elapsed=max(0,now-s.get('step_started_monotonic',s['_monotonic']))
 return 'Still running; step='+str(step or 'unlabelled')+'; step elapsed='+format(elapsed,'.1f')+'s; no complete output for '+format(max(0,now-last_output),'.1f')+'s. No percentage inferred.\n'

def supervise(p):
 session,root,op=bound(p)
 with lock(op/'runner.lock',False):
  with lock(op/'state.lock'):
   s=read(op/'state.json');verify(p,s)
   if s['phase']!='launch_intent':return
   s.update(phase='launching',supervisor=process(os.getpid()),started_at=time.time(),_monotonic=time.monotonic(),log_bytes=0,output_truncated=False,native_output_bytes=0,native_output_truncated=False);atomic(op/'state.json',s)
  # argv stays in this finite process's memory/stdin; never stored in state/logs.
  child=None;logfd=None;secret=None
  redactor=Redactor();native_used=0;pending={'stdout':b'','stderr':b''};discard={'stdout':False,'stderr':False};seq=0;reason=None
  try:
   logfd=os.open(op/'output.jsonl',os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
   command=repository_command(p['argv'])if os.environ.get('NOMAD_WORKER_REPOSITORY_REFERENCE')else p['argv']
   if p.get('secret_profile'):
    def request_secret_cancel(signum,frame):
     if not(op/'cancel.json').exists():atomic(op/'cancel.json',{'binding':p['binding'],'requested_at':time.time()})
    signal.signal(signal.SIGTERM,request_secret_cancel)
    secret=SecretOperation(p,command);child=secret.start()
    s.update(command_process=process(child.pid));atomic(op/'state.json',s)
    native(session,s,'secret_rendering',stream='stdout',text='Preparing approved secret profile '+p['secret_profile']+' for '+p['secret_client']+'; client not started.\n')
    values=secret.ready(lambda:(op/'cancel.json').exists(),s['_monotonic']+p['timeout'])
    for value in values:redactor.add(value,*value.splitlines())
    values=None
    with lock(op/'state.lock'):
     if (op/'cancel.json').exists():raise RuntimeError('SECRET_START_CANCELLED')
     secret.activate()
    child.stdout,child.stderr=secret.handles[1],secret.handles[2]
   else:child=subprocess.Popen(command,cwd=p['cwd'],stdin=subprocess.DEVNULL,stdout=subprocess.PIPE,stderr=subprocess.PIPE,start_new_session=True)
   s.update(phase='running',command_process=process(child.pid));atomic(op/'state.json',s)
   native(session,s,'running',stream='stdout',text='Operation '+s['label']+' started; waiting for named step output.\n');selector=selectors.DefaultSelector();selector.register(child.stdout,selectors.EVENT_READ,'stdout');selector.register(child.stderr,selectors.EVENT_READ,'stderr')
   start=s['_monotonic'];last=start;last_flush=start;last_output=start;term_at=None;exited_at=None
   def output(stream,raw):
    nonlocal seq,native_used,last_output
    observed_at=time.monotonic()
    # Redact a complete line BEFORE chunking, so split secret tokens cannot leak.
    text=redactor.text(raw.decode('utf-8','replace'))
    text,progress_phase,forward=progress_line(s,text,observed_at)
    if forward:last_output=observed_at
    for part in ([text[i:i+4096] for i in range(0,len(text),4096)] or ['']):
     seq+=1;event={'sequence':seq,'stream':stream,'time':time.time(),'text':part};data=encoded(event)+b'\n'
     if s['log_bytes']+len(data)<=p['policy']['max_log_bytes']:
      os.write(logfd,data);s['log_bytes']+=len(data)
     else:s['output_truncated']=True
     remaining=part
     while forward and remaining and native_used<p['policy']['native_output_bytes']:
      limit=min(2048,p['policy']['native_output_bytes']-native_used)
      fragment=remaining.encode()[:limit].decode('utf-8','ignore')
      if not fragment:break
      native_used+=len(fragment.encode());remaining=remaining[len(fragment):]
      s['native_output_bytes']=native_used
      native(session,s,progress_phase or stream,stream=stream,text=fragment)
     if forward and remaining and not s['native_output_truncated']:
      s['native_output_truncated']=True
      native(session,s,'output_truncated',stream='stderr',text='[NATIVE_OUTPUT_LIMIT] Further output is not forwarded. Use operation-logs for retained redacted output; output_truncated reports the guest log limit.\n')
   while selector.get_map() or child.poll() is None:
    now=time.monotonic()
    if child.poll() is not None:
     if secret is not None:secret.release_writers()
     if exited_at is None:exited_at=now
     elif selector.get_map() and now-exited_at>2:raise RuntimeError('DESCENDANTS_OR_OUTPUT_STILL_OPEN')
    if reason is None and ((op/'cancel.json').exists() or now-start>=p['timeout']):
     reason='cancelled' if (op/'cancel.json').exists() else 'timed_out'
     if child.poll() is None:os.killpg(child.pid,signal.SIGTERM);term_at=now
    if term_at is not None and now-term_at>=3 and child.poll() is None:os.killpg(child.pid,signal.SIGKILL);term_at=None
    events=selector.select(timeout=.2)
    if secret is not None and child.poll()is not None:events=[(key,selectors.EVENT_READ)for key in list(selector.get_map().values())]
    for key,_ in events:
     stream=key.data
     try:raw=os.read(key.fileobj.fileno(),16384)
     except BlockingIOError:continue
     if not raw:
      selector.unregister(key.fileobj);key.fileobj.close()
      if pending[stream]:output(stream,pending[stream]);pending[stream]=b''
      continue
     pending[stream]+=raw.replace(b'\r\n',b'\n').replace(b'\r',b'\n')
     while b'\n' in pending[stream]:
      line,pending[stream]=pending[stream].split(b'\n',1)
      if discard[stream] or len(line)>65536:discard[stream]=False;s['output_truncated']=True;output(stream,b'[REDACTED_OVERSIZE_LINE]\n')
      else:output(stream,line+b'\n')
     if len(pending[stream])>65536:pending[stream]=b'';discard[stream]=True;s['output_truncated']=True
    if now-last_flush>=p['policy']['heartbeat_seconds']:
     os.fsync(logfd);atomic(op/'state.json',s);last_flush=now
     if now-last_output>=max(30,p['policy']['heartbeat_seconds']) and now-last>=max(30,p['policy']['heartbeat_seconds']):
      native(session,s,'running',stream='stdout',text=quiet_text(s,now,last_output));last=now
   code=child.wait()
   if secret is not None:secret.stop()
   os.fsync(logfd);os.close(logfd);logfd=None
   raw=(op/'output.jsonl').read_bytes();s.update(phase=reason or ('completed' if code==0 else 'failed'),exit_code=code,finished_at=time.time(),log_sha256=sha(raw),log_bytes=len(raw),effects_may_be_partial=bool(reason or code))
   atomic(op/'state.json',s);native(session,s,('exit_'+str(code).replace('-','minus_')) if not reason else reason,exit_code=code,stream='stdout',text='Operation '+s['label']+' finished: phase='+s['phase']+', exit='+str(code)+', elapsed='+format(max(0,time.monotonic()-start),'.1f')+'s.\n')
  except BaseException as e:
   # Never leave a child silently running after runner failure; scope is our child group.
   if child is not None and child.poll() is None:
    try:os.killpg(child.pid,signal.SIGTERM);child.wait(timeout=3)
    except subprocess.TimeoutExpired:os.killpg(child.pid,signal.SIGKILL);child.wait()
    except ProcessLookupError:pass
   if secret is not None:
    try:secret.stop()
    except Exception:pass
   phase='cancelled'if secret is not None and(op/'cancel.json').exists()else'timed_out'if secret is not None and time.monotonic()-s['_monotonic']>=p['timeout']else'failed'if secret is not None and not secret.activated else'unknown'
   s.update(phase=phase,error=str(e)if re.fullmatch('[A-Z_]{1,100}',str(e))else type(e).__name__,finished_at=time.time())
   if phase!='unknown':
    raw=(op/'output.jsonl').read_bytes();s.update(log_sha256=sha(raw),log_bytes=len(raw),exit_code=child.returncode if child is not None else None,effects_may_be_partial=bool(secret and secret.activated))
   atomic(op/'state.json',s)
   try:native(session,s,phase,stream='stderr',text='Operation '+s['label']+': '+phase+'; '+s['error']+'. No automatic replay.\n')
   except Exception:pass
  finally:
   if secret is not None:
    try:secret.close()
    except Exception:
     s.update(phase='unknown',error='SECRET_CLEANUP_UNCONFIRMED');atomic(op/'state.json',s)
   if logfd is not None:os.fsync(logfd);os.close(logfd)

def main(p):
 if p['action']=='probe':
  guarded(p)
  schema=1
  task_main=pathlib.Path(p.get('task_main','/opt/nomad-workers/task-main.py'))
  if task_main.is_file() and 'OPERATION_LOG_SCHEMA = 2' in task_main.read_text():schema=2
  return {**identity(),'native_log_schema':schema}
 if p['action']=='secret-profiles':
  guarded(p)
  if identity()!=p['machine_identity']:raise RuntimeError('MACHINE_BOOT_CHANGED_OPERATION_UNKNOWN')
  return secret_listing(p)
 session,root,op=bound(p)
 if p['action']=='start':
  with lock(root/'creation.lock'):
   if op.exists():
    private(op);return status(p,op) # Existing intent is NEVER another launch.
   if sum(v.is_dir() for v in root.iterdir())>=p['policy']['max_operations_per_session']:raise RuntimeError('OPERATION_COUNT_QUOTA')
   private(op)
   s={'version':1,'operation_id':p['operation_id'],'binding':p['binding'],'command_sha256':p['command_sha256'],'label':p['label'],'phase':'launch_intent','timeout_seconds':p['timeout'],'created_at':time.time()}
   s.update({k:p[k]for k in ('secret_profile','secret_client','secret_catalogue_sha256')if k in p});atomic(op/'state.json',s)
   src=p.pop('runner_source');script=op/'runner.py'
   fd=os.open(script,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o500)
   with os.fdopen(fd,'w') as f:f.write(src);f.flush();os.fsync(f.fileno())
   # Finite workload process, with durable ownership/status, not nohup/service.
   child=subprocess.Popen([sys.executable,str(script),'supervise'],stdin=subprocess.PIPE,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,start_new_session=True,close_fds=True)
   child.stdin.write(encoded(p));child.stdin.close()
  end=time.monotonic()+8
  while time.monotonic()<end:
   s=status(p,op)
   if s['phase']!='launch_intent':return s
   time.sleep(.05)
  return s
 if not op.exists():raise RuntimeError('OPERATION_NOT_FOUND_NO_REPLAY')
 private(op)
 with lock(op/'state.lock'):
  s=status(p,op)
  if p['action']=='status':return s
  if p['action']=='cancel':
   if s['phase'] in ('completed','failed','cancelled','timed_out'):return s
   pending_launch=s['phase']=='launching'and s.get('command_process')is None
   if not alive(s.get('supervisor')) or (not pending_launch and not alive(s.get('command_process'))):raise RuntimeError('PROCESS_IDENTITY_UNCONFIRMED_NO_SIGNAL')
   if not (op/'cancel.json').exists():atomic(op/'cancel.json',{'binding':p['binding'],'requested_at':time.time()})
   return {**s,'cancel_requested':True}
  if p['action']=='logs':
   cursor=p.get('cursor',0)
   if type(cursor)!=int or cursor<0:raise RuntimeError('INVALID_CURSOR')
   path=op/'output.jsonl'
   if not path.exists():return {'data':'','cursor':cursor,'state':s,'eof':True}
   fd=os.open(path,os.O_RDONLY|os.O_NOFOLLOW)
   with os.fdopen(fd,'rb') as f:
    size=os.fstat(f.fileno()).st_size
    if cursor>size:raise RuntimeError('LOG_CURSOR_PAST_END')
    f.seek(cursor);data=b''
    while len(data)<65536:
     line=f.readline(70000)
     if not line:break
     if not line.endswith(b'\n'):break
     data+=line
    return {'data':base64.b64encode(data).decode(),'cursor':cursor+len(data),'state':s,'eof':cursor+len(data)==size}
 raise RuntimeError('INVALID_OPERATION_ACTION')

if __name__=='__main__':
 try:
  payload=json.load(sys.stdin)
  if sys.argv[-1:] == ['supervise']:supervise(payload)
  else:print(json.dumps({'ok':True,'result':main(payload)}))
 except Exception as e:print(json.dumps({'ok':False,'code':str(e) if re.fullmatch('[A-Z_]{1,100}',str(e)) else type(e).__name__}));sys.exit(1)
