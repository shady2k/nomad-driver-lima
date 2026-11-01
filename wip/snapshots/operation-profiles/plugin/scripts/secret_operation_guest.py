"""Finite managed-operation secret runtime. No RPC listener or authentication API."""
import os,json,pathlib,stat,fcntl,contextlib,tempfile,hashlib,subprocess,time,sys,signal,selectors,shutil

def secret_alive(row):
 if not row:return False
 current=process(row['pid'])
 return current is not None and current['state']!='Z'and current['pid']==row['pid']and current['ticks']==row['ticks']

def secret_subreaper():
 import ctypes
 libc=ctypes.CDLL(None,use_errno=True)
 if libc.prctl(36,1,0,0,0)!=0:raise RuntimeError('SECRET_SUBREAPER_UNAVAILABLE')
 current=ctypes.c_int()
 if libc.prctl(37,ctypes.byref(current),0,0,0)!=0 or current.value!=1:raise RuntimeError('SECRET_SUBREAPER_UNCONFIRMED')

def secret_descendants(rows):
 # This finite runner starts no unrelated workloads. Subreaper adoption keeps
 # daemonized/reparented children in its process tree without a service/cgroup.
 found={row['pid']:row for row in rows if row is not None and secret_alive(row)};todo=list(found)
 while todo:
  pid=todo.pop();row=found[pid]
  if not secret_alive(row):continue
  try:threads=list((pathlib.Path('/proc')/str(pid)/'task').iterdir())
  except FileNotFoundError:continue
  if len(threads)>1024:raise RuntimeError('SECRET_PROCESS_TREE_BOUND')
  for thread in threads:
   try:children=(thread/'children').read_text().split()
   except FileNotFoundError:continue
   for value in children:
    child=int(value)
    if child in found:continue
    current=process(child)
    if current is not None:found[child]=current;todo.append(child)
    if len(found)>1024:raise RuntimeError('SECRET_PROCESS_TREE_BOUND')
 return [row for row in found.values()if secret_alive(row)]

def secret_owned_descendants():
 return [row for row in secret_descendants([process(os.getpid())])if row['pid']!=os.getpid()]

def secret_root(session):
 import uuid
 if str(uuid.UUID(session))!=session:raise RuntimeError('SECRET_SESSION_INVALID')
 if not any(line.split()[4]=='/dev/shm'and line.split(' - ')[1].split()[0]=='tmpfs'for line in pathlib.Path('/proc/self/mountinfo').read_text().splitlines()):raise RuntimeError('SECRET_TMPFS_REQUIRED')
 root=pathlib.Path('/dev/shm')/('agent-worker-'+str(os.getuid()))/session
 for p in (root.parent,root):
  st=p.lstat()
  if not stat.S_ISDIR(st.st_mode)or st.st_uid!=os.getuid()or stat.S_IMODE(st.st_mode)!=0o700:raise RuntimeError('SECRET_ROOT_UNSAFE')
 return root

def secret_read(path):
 fd=os.open(path,os.O_RDONLY|os.O_NOFOLLOW)
 with os.fdopen(fd)as f:
  st=os.fstat(f.fileno())
  if not stat.S_ISREG(st.st_mode)or st.st_uid!=os.getuid()or st.st_mode&0o077 or st.st_size>262144:raise RuntimeError('SECRET_FILE_UNSAFE')
  return json.load(f)

@contextlib.contextmanager
def secret_lock(root):
 fd=os.open(root/'secret-process.lock',os.O_CREAT|os.O_RDWR|os.O_NOFOLLOW,0o600)
 try:
  fcntl.flock(fd,fcntl.LOCK_EX);yield
 finally:os.close(fd)

def secret_atomic(path,value):
 fd,name=tempfile.mkstemp(prefix='.secret-',dir=path.parent)
 try:
  with os.fdopen(fd,'w')as f:os.fchmod(f.fileno(),0o600);json.dump(value,f);f.flush();os.fsync(f.fileno())
  os.replace(name,path)
  fd=os.open(path.parent,os.O_RDONLY)
  try:os.fsync(fd)
  finally:os.close(fd)
 finally:
  if os.path.exists(name):os.unlink(name)

def secret_delivery(p):
 root=secret_root(p['session_id'])
 if any((root/name).exists()for name in ('sealed','secret-process.sealed')):raise RuntimeError('SECRET_RUNTIME_FINALIZED')
 d=secret_read(root/'secret-profiles.json')
 b=d['binding'];actual=p['binding']
 if any(b.get(k)!=actual.get(k)for k in ('session_id','creator_agent_id','creator_session_id','creator_generation'))or any(b.get('attempt',{}).get(k)!=actual.get('attempt',{}).get(k)for k in ('allocation','task'))or d['machine_identity']!=p['machine_identity']:raise RuntimeError('SECRET_DELIVERY_BINDING_CHANGED')
 validate(d['catalogue'])
 if digest(d['catalogue'])!=d['catalogue_sha256']:raise RuntimeError('SECRET_DELIVERY_DIGEST_CHANGED')
 return root,d

def secret_listing(p):
 try:root,d=secret_delivery(p)
 except FileNotFoundError:return []
 return public_profiles(d['catalogue'],d['namespace'],d['task'])

# This shim runs under Agent with the injected environment, then execs the client.
# Client output does not share Agent diagnostics. Values go only through a private
# FIFO to the existing trusted managed runner's in-memory exact-value redactor.
SECRET_CLIENT_SHIM=r'''
import os,sys,json
root=sys.argv[1];names=json.loads(sys.argv[2]);argv=json.loads(sys.argv[3])
values=[os.environ[n]for n in names]
raw=json.dumps({'values':values,'client_pid':os.getpid(),'parent_pid':os.getppid()}).encode()
if len(raw)>65536:raise RuntimeError('SECRET_VALUES_TOO_LARGE')
fd=os.open(root+'/control',os.O_WRONLY);raw+=b'\n'
while raw:raw=raw[os.write(fd,raw):]
os.close(fd)
fd=os.open(root+'/ack',os.O_RDONLY)
if os.read(fd,5)!=b'ready':raise RuntimeError('SECRET_PARENT_NOT_READY')
os.close(fd)
for name in ('VAULT_TOKEN','BAO_TOKEN','NOMAD_TOKEN'):os.environ.pop(name,None)
for number,name in ((1,'stdout'),(2,'stderr')):
 fd=os.open(root+'/'+name,os.O_WRONLY);os.dup2(fd,number);os.close(fd)
os.execvpe(argv[0],argv,os.environ)
'''

class SecretOperation:
 def __init__(self,p,argv):
  self.p=p;self.argv=argv;self.root,self.delivery=secret_delivery(p);self.child=None;self.directory=None;self.handles=[];self.writers=[];self.activated=False;self.processes=[];self.registry_id=None
 def start(self):
  p=self.p;d=self.delivery
  profile=select(d['catalogue'],p['secret_profile'],p['secret_catalogue_sha256'],d['namespace'],d['task'],p['secret_client'],p['cwd'],p['argv'])
  work=pathlib.Path(profile['clients'][p['secret_client']]['work_root'])
  cwd=pathlib.Path(p['cwd'])
  current=work
  if current.is_symlink():raise RuntimeError('SECRET_CLIENT_WORKSPACE_SYMLINK')
  for part in cwd.relative_to(work).parts:
   current=current/part
   if current.is_symlink():raise RuntimeError('SECRET_CLIENT_WORKSPACE_SYMLINK')
  if not cwd.is_dir()or not cwd.resolve().is_relative_to(work.resolve()):raise RuntimeError('SECRET_CLIENT_WORKSPACE_DENIED')
  token=self.root/'vault_token';st=token.lstat()
  if not stat.S_ISREG(st.st_mode)or st.st_uid!=os.getuid()or stat.S_IMODE(st.st_mode)!=0o600:raise RuntimeError('SECRET_TASK_TOKEN_UNSAFE')
  with secret_lock(self.root):
   secret_delivery(p) # serialize finalization vs spawn/registration
   secret_subreaper()
   parent=self.root/'secret-operations'
   try:parent.mkdir(mode=0o700)
   except FileExistsError:pass
   st=parent.lstat()
   if not stat.S_ISDIR(st.st_mode)or st.st_uid!=os.getuid()or stat.S_IMODE(st.st_mode)!=0o700:raise RuntimeError('SECRET_OPERATION_ROOT_UNSAFE')
   self.directory=parent/p['operation_id'];self.directory.mkdir(mode=0o700) # no second launch
   import uuid
   self.registry_id=uuid.uuid4().hex;self.processes=[process(os.getpid())]
   for name in ('control','stdout','stderr','ack'):
    os.mkfifo(self.directory/name,0o600);fd=os.open(self.directory/name,os.O_RDONLY|os.O_NONBLOCK|os.O_NOFOLLOW);self.handles.append(os.fdopen(fd,'rb',buffering=0));self.writers.append(os.open(self.directory/name,os.O_WRONLY|os.O_NONBLOCK|os.O_NOFOLLOW))
   shim=self.directory/'client.py';shim.write_text(SECRET_CLIENT_SHIM);shim.chmod(0o500)
   command=[sys.executable,str(shim),str(self.directory),json.dumps([b['env']for b in profile['bindings']]),json.dumps(self.argv)]
   hcl=compile_supervisor(profile,d['address'],str(token),command,internal_exec=True)
   config=self.directory/'agent.hcl';config.write_text(hcl);config.chmod(0o600)
   # No inherited task/project environment. Client gets only operator-mapped
   # secrets and this minimal environment, plus any repository wrapper contract.
   env={'PATH':'/usr/local/bin:/usr/bin:/bin','HOME':str(self.directory),'XDG_CONFIG_HOME':str(self.directory/'config'),'XDG_CACHE_HOME':str(self.directory/'cache'),'DO_NOT_TRACK':'1'}
   self.child=subprocess.Popen([d['bao_bin'],'agent','-config='+str(config),'-log-level=error'],cwd=p['cwd'],stdin=subprocess.DEVNULL,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,env=env,start_new_session=True)
   row=process(self.child.pid)
   if row is None:raise RuntimeError('SECRET_SUPERVISOR_START_UNCONFIRMED')
   self.processes=[process(os.getpid()),row];self.record_owner()
  return self.child
 def ready(self,cancelled,deadline):
  raw=b'';end=min(deadline,time.monotonic()+30)
  while time.monotonic()<end:
   if cancelled():raise RuntimeError('SECRET_START_CANCELLED')
   try:chunk=os.read(self.handles[0].fileno(),65537-len(raw))
   except BlockingIOError:chunk=b''
   raw+=chunk
   if len(raw)>65536:raise RuntimeError('SECRET_VALUE_BOUND')
   if b'\n'in raw:
    frame=json.loads(raw);values=frame['values']
    if not isinstance(values,list)or any(not isinstance(v,str)or not v for v in values):raise RuntimeError('SECRET_RENDER_EMPTY')
    client=process(frame['client_pid'])
    if frame.get('parent_pid')!=self.child.pid or not client or client['uid']!=os.getuid():raise RuntimeError('SECRET_CLIENT_PARENT_CHANGED')
    self.processes.append(client)
    with secret_lock(self.root):
     secret_delivery(self.p);self.record_owner()
    return values
   if self.child.poll()is not None:raise RuntimeError('SECRET_RENDER_OR_CLIENT_START_FAILED')
   time.sleep(.02)
  raise RuntimeError('SECRET_RENDER_READINESS_TIMEOUT')
 def activate(self):
  # Mark uncertain BEFORE the write: failure must not claim no client effects.
  self.activated=True
  os.write(self.writers[3],b'ready')
 def owner_record(self):
  return {'version':2,'phase':'active','registry_id':self.registry_id,'operation_id':self.p['operation_id'],'binding':self.p['binding'],'machine_identity':self.p['machine_identity'],'runner':self.processes[0],'processes':self.processes}
 def record_owner(self):
  secret_atomic(self.directory/'owner.json',self.owner_record())
 def stop(self):
  # The client PID is registered before exec, so Agent exit/reparenting cannot
  # make that foreground process disappear from our ownership set.
  for sig in (signal.SIGTERM,signal.SIGKILL):
   self.processes=list({row['pid']:row for row in [*self.processes,*secret_owned_descendants()]}.values())
   rows={row['pid']:row for row in self.processes if row['pid']!=os.getpid()}
   for row in reversed(list(rows.values())):
    if secret_alive(row):
     try:os.kill(row['pid'],sig)
     except ProcessLookupError:pass
   end=time.monotonic()+3
   while any(secret_alive(row)for row in rows.values())and time.monotonic()<end:time.sleep(.03)
  if secret_owned_descendants()or any(secret_alive(row)for row in rows.values()):raise RuntimeError('SECRET_PROCESS_STOP_UNCONFIRMED')
  if self.child is not None:self.child.wait(timeout=1)
 def release_writers(self):
  for fd in self.writers:os.close(fd)
  self.writers=[]
 def close(self):
  # Caller must stop/wait the supervisor before this cleanup; no token revoke.
  if self.child is not None and self.child.poll()is None:raise RuntimeError('SECRET_SUPERVISOR_STILL_RUNNING')
  if any(secret_alive(row)for row in self.processes if row['pid']!=os.getpid())or secret_owned_descendants():raise RuntimeError('SECRET_PROCESS_CLEANUP_UNCONFIRMED')
  self.release_writers()
  for f in self.handles:
   if not f.closed:f.close()
  if self.directory is not None:
   with secret_lock(self.root):
    # No further subprocess creation is permitted after this terminal close.
    # Commit proof BEFORE deleting active registration; process crash between
    # these two writes remains recoverable without trusting vanished roots.
    if not self.registry_id or not self.processes:raise RuntimeError('SECRET_REGISTRATION_UNCONFIRMED')
    owner=self.owner_record();proof={k:owner[k]for k in ('registry_id','operation_id','binding','machine_identity','runner')}
    proof.update(version=2,cleanup_complete=True,scope='process-tree',completed_at=time.time(),stopped_processes=[row for row in self.processes if row['pid']!=os.getpid()])
    target=secret_completion_dir(self.root)/(self.p['operation_id']+'.json')
    if target.exists():
     if not secret_proof_matches(secret_read(target),owner):raise RuntimeError('SECRET_COMPLETION_BINDING_CHANGED')
    else:secret_atomic(target,proof)
    retired=self.directory.parent/('.closed-'+self.p['operation_id']+'-'+self.registry_id)
    if retired.exists():raise RuntimeError('SECRET_RETIRED_DIRECTORY_COLLISION')
    os.rename(self.directory,retired);self.directory=None
   shutil.rmtree(retired)

def secret_process_record(row):
 return isinstance(row,dict)and type(row.get('pid'))is int and row['pid']>1 and isinstance(row.get('ticks'),str)and row['ticks'].isdigit()and type(row.get('uid'))is int and row['uid']>=0

def secret_module_identity(pid,module):
 # task-main v5 identity has pid/ticks only and exact-dict alive(); the managed
 # runner's process() includes uid/state. Normalize explicitly, not by equality.
 return module.process(pid)if hasattr(module,'process')else module.identity(pid)

def secret_module_alive(row,module):
 if not secret_process_record(row):raise RuntimeError('SECRET_PROCESS_RECORD_UNCONFIRMED')
 current=secret_module_identity(row['pid'],module)
 return current is not None and current.get('state')!='Z'and current.get('pid')==row['pid']and current.get('ticks')==row['ticks']

def secret_module_descendants(row,module):
 if not secret_module_alive(row,module):return []
 return module.descendants([secret_module_identity(row['pid'],module)])

def secret_context_match(row,delivery):
 b=row.get('binding',{});expected=delivery['binding']
 return row.get('machine_identity')==delivery['machine_identity']and all(b.get(k)==expected.get(k)for k in ('session_id','creator_agent_id','creator_session_id','creator_generation'))and all(b.get('attempt',{}).get(k)==expected.get('attempt',{}).get(k)for k in ('allocation','task'))

def secret_registry_valid(row,operation,delivery):
 import re,uuid
 try:
  return row.get('version')==2 and row.get('phase')=='active'and row.get('operation_id')==operation and str(uuid.UUID(operation))==operation and re.fullmatch('[a-f0-9]{32}',row.get('registry_id',''))is not None and secret_process_record(row.get('runner'))and row['runner']['uid']==os.getuid()and secret_context_match(row,delivery)
 except (ValueError,TypeError):return False

def secret_proof_matches(proof,owner):
 return proof.get('version')==2 and proof.get('cleanup_complete')is True and proof.get('scope')=='process-tree' and isinstance(proof.get('completed_at'),(float,int))and proof['completed_at']>0 and all(proof.get(k)==owner.get(k)for k in ('registry_id','operation_id','binding','machine_identity','runner'))and isinstance(proof.get('stopped_processes'),list)

def secret_completion_dir(root):
 path=root/'secret-completed'
 try:path.mkdir(mode=0o700)
 except FileExistsError:pass
 st=path.lstat()
 if not stat.S_ISDIR(st.st_mode)or st.st_uid!=os.getuid()or stat.S_IMODE(st.st_mode)!=0o700:raise RuntimeError('SECRET_COMPLETION_ROOT_UNSAFE')
 return path

def secret_stop_all(root,module,timeout=12):
 """Request registered runners to close; never infer cleanup from missing PIDs.

Only the live subreaper can prove its child tree empty. If it disappears before
committing that proof, finalization is unknown even when all recorded roots died.
"""
 expected={};proofs=root/'secret-completed'
 with secret_lock(root):
  secret_atomic(root/'secret-process.sealed',{'sealed':True})
  active=root/'secret-operations'
  if not active.exists()and not proofs.exists():return True
  delivery=secret_read(root/'secret-profiles.json')
  if active.exists():
   st=active.lstat()
   if not stat.S_ISDIR(st.st_mode)or st.st_uid!=os.getuid()or stat.S_IMODE(st.st_mode)!=0o700:raise RuntimeError('SECRET_OPERATION_ROOT_UNSAFE')
   paths=list(active.iterdir())
   if len(paths)>1024:raise RuntimeError('SECRET_REGISTRY_BOUND')
   for path in paths:
    st=path.lstat()
    if not stat.S_ISDIR(st.st_mode)or st.st_uid!=os.getuid()or stat.S_IMODE(st.st_mode)!=0o700:raise RuntimeError('SECRET_OPERATION_UNSAFE')
    if path.name.startswith('.closed-'):
     suffix=path.name[len('.closed-'):];operation=suffix[:36];registry=suffix[37:]
     if len(suffix)!=69 or suffix[36]!='-':raise RuntimeError('SECRET_RETIRED_IDENTITY_CHANGED')
     try:proof=secret_read(proofs/(operation+'.json'))
     except FileNotFoundError:raise RuntimeError('SECRET_RETIRED_PROOF_UNCONFIRMED')
     owner={**proof,'phase':'active'}
     if proof.get('registry_id')!=registry or not secret_registry_valid(owner,operation,delivery)or not secret_proof_matches(proof,owner):raise RuntimeError('SECRET_RETIRED_PROOF_UNCONFIRMED')
     if operation in expected:raise RuntimeError('SECRET_DUPLICATE_REGISTRATION')
     expected[operation]=owner;continue
    try:owner=secret_read(path/'owner.json')
    except FileNotFoundError:raise RuntimeError('SECRET_PROCESS_START_OUTCOME_UNKNOWN')
    if not secret_registry_valid(owner,path.name,delivery):raise RuntimeError('SECRET_ACTIVE_REGISTRATION_UNCONFIRMED')
    expected[path.name]=owner
  if proofs.exists():
   secret_completion_dir(root)
   paths=list(proofs.iterdir())
   if len(paths)>1024:raise RuntimeError('SECRET_REGISTRY_BOUND')
   for path in paths:
    proof=secret_read(path);operation=proof.get('operation_id')
    if path.name!=str(operation)+'.json':raise RuntimeError('SECRET_COMPLETION_IDENTITY_CHANGED')
    owner={**proof,'phase':'active'}
    if not secret_registry_valid(owner,operation,delivery)or not secret_proof_matches(proof,owner):raise RuntimeError('SECRET_COMPLETION_UNCONFIRMED')
    if operation not in expected:expected[operation]=owner
 requested=set();completed_at={};end=time.monotonic()+timeout
 while expected:
  for operation,owner in list(expected.items()):
   def completion():
    with secret_lock(root):
     try:return secret_read(proofs/(operation+'.json'))
     except FileNotFoundError:return None
   proof=completion();runner=owner['runner']
   if proof is None:
    if not secret_module_alive(runner,module):
     # A normal close commits before exit. Re-read under the same lock to avoid
     # misclassifying a concurrent proof commit as a lost runner.
     proof=completion()
     if proof is None:raise RuntimeError('SECRET_RUNNER_LOST_CLEANUP_UNCONFIRMED')
    elif operation not in requested:
     try:os.kill(runner['pid'],signal.SIGTERM)
     except ProcessLookupError:pass
     requested.add(operation)
   if proof is not None:
    if not secret_proof_matches(proof,owner):raise RuntimeError('SECRET_COMPLETION_BINDING_CHANGED')
    if any(secret_module_alive(row,module)for row in proof['stopped_processes']):raise RuntimeError('SECRET_COMPLETION_PROCESS_STILL_ALIVE')
    if secret_module_alive(runner,module):
     if any(row['pid']!=runner['pid']for row in secret_module_descendants(runner,module)):raise RuntimeError('SECRET_COMPLETED_RUNNER_HAS_CHILDREN')
     first=completed_at.setdefault(operation,time.monotonic())
     if time.monotonic()-first>=2:
      # The durable proof and fresh empty child tree make terminating only this
      # lingering trusted runner safe; never use this fallback for active work.
      if secret_module_alive(runner,module):
       try:os.kill(runner['pid'],signal.SIGKILL)
       except ProcessLookupError:pass
    else:expected.pop(operation)
  if expected:
   if time.monotonic()>=end:raise RuntimeError('SECRET_CLEANUP_COMPLETION_TIMEOUT')
   time.sleep(.03)
 return True
