# Ephemeral fixed token delivery; secret arrives only on stdin, never argv/output.
import json,os,re,stat,sys,tempfile,hashlib,urllib.request,urllib.error,urllib.parse,time,signal,pathlib
def stop_owned_runtime(module,d):
 try:module.stop_runtime(d);return True
 except Exception:
  # Old accepted guest assets only sent TERM. Complete the same guarded stop
  # from the driver workload without editing/restarting the installed guest.
  try:
   with module.runtime_lock(pathlib.Path(d)):
    with open(d+'/runtime-owner.json')as state_file:state=json.load(state_file)
    rows=module.descendants(state['processes'])
    for row in rows:
     if module.alive(row):
      try:os.kill(row['pid'],signal.SIGKILL)
      except ProcessLookupError:pass
    deadline=time.monotonic()+5
    while (any(module.alive(row) for row in rows)or module.listening_socket_inodes(pathlib.Path(d)/'prime.sock'))and time.monotonic()<deadline:time.sleep(.05)
    if any(module.alive(row) for row in rows)or module.listening_socket_inodes(pathlib.Path(d)/'prime.sock'):raise RuntimeError('UNCONFIRMED')
    module.atomic(pathlib.Path(d)/'runtime-stopped.json',{'stopped':True,'processes':rows});return True
  except Exception:
   return False

p=json.load(sys.stdin)
assert re.fullmatch(r'[a-f0-9-]{36}',p['session'])
assert re.fullmatch(r'[a-f0-9]{32}',p['owner'])
assert any(x.split()[4]=='/dev/shm' and x.split(' - ')[1].split()[0]=='tmpfs' for x in open('/proc/self/mountinfo'))
os.umask(0o077)
root='/dev/shm/agent-worker-'+str(os.getuid())
def private(d):
 try:os.mkdir(d,0o700)
 except FileExistsError:pass
 s=os.lstat(d);assert stat.S_ISDIR(s.st_mode) and s.st_uid==os.getuid() and stat.S_IMODE(s.st_mode)==0o700
private(root);d=root+'/'+p['session'];private(d)
owner=d+'/owner'
try:
 fd=os.open(owner,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
 with os.fdopen(fd,'w') as f:f.write(p['owner'])
except FileExistsError:
 fd=os.open(owner,os.O_RDONLY|os.O_NOFOLLOW)
 with os.fdopen(fd) as f:assert f.read(65)==p['owner']
# Stop-only maintenance is a disjoint terminal branch. It does not open token
# files, construct an HTTP client, send revoke, clear state, or stop the VM.
if p['action']=='stop-runtime':
 import importlib.util
 spec=importlib.util.spec_from_file_location('owned_runtime','/opt/nomad-workers/task-main.py');module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module)
 if not stop_owned_runtime(module,d):
  print('{"ok":false,"code":"OWNED_RUNTIME_STOP_UNCONFIRMED"}');sys.exit(0)
 fd,tmp=tempfile.mkstemp(prefix='.stop-seal-',dir=d)
 with os.fdopen(fd,'w')as f:os.fchmod(f.fileno(),0o600);f.write('owned-runtime-stopped');f.flush();os.fsync(f.fileno())
 os.replace(tmp,d+'/sealed')
 print(json.dumps({'ok':True,'runtime_stopped':True,'revocation':'not_attempted','tokens_retained':True}));sys.exit(0)
target=d+'/vault_token'
history=d+'/tokens';private(history)
receipts=d+'/revoke-receipts';private(receipts)
def write(name,value):
 fd,tmp=tempfile.mkstemp(prefix='.pending-',dir=d)
 try:
  with os.fdopen(fd,'w') as f:os.fchmod(f.fileno(),0o600);f.write(value);f.flush();os.fsync(f.fileno())
  os.replace(tmp,name)
 finally:
  if os.path.exists(tmp):os.unlink(tmp)
def read_token(name):
 fd=os.open(name,os.O_RDONLY|os.O_NOFOLLOW)
 with os.fdopen(fd) as f:
  st=os.fstat(f.fileno());assert stat.S_ISREG(st.st_mode) and st.st_uid==os.getuid() and stat.S_IMODE(st.st_mode)==0o600
  token=f.read(16385);assert 0<len(token)<=16384 and all(32<ord(c)<127 for c in token)
  return token
if p['action']=='revoke':
 # No delivery can pass a finalization seal. Driver also holds its delivery lock.
 # A previous receipt is evidence of an earlier stop, not a liveness check.
 # Always recheck current owned identities/socket; a stale marker must not hide
 # a surviving or reparented Prime process.
 import importlib.util
 spec=importlib.util.spec_from_file_location('owned_runtime','/opt/nomad-workers/task-main.py');module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module)
 if not stop_owned_runtime(module,d):
  print('{"ok":false,"code":"OWNED_RUNTIME_STOP_UNCONFIRMED"}');sys.exit(0)
 stopped=json.load(open(d+'/runtime-stopped.json'));assert stopped.get('stopped') is True
 for ident in stopped.get('processes',[]):
  try:current=open('/proc/'+str(int(ident['pid']))+'/stat').read().split(') ',1)[1].split();assert current[19]!=ident['ticks'] or current[0]=='Z'
  except FileNotFoundError:pass
 write(d+'/sealed','finish')
 u=urllib.parse.urlsplit(p['address']);assert u.scheme=='https' and u.hostname and not u.username and not u.password and not u.query and not u.fragment and u.path in ('','/')
 class NoRedirect(urllib.request.HTTPRedirectHandler):
  def redirect_request(self,*a,**kw):raise RuntimeError('redirect refused')
 opener=urllib.request.build_opener(NoRedirect)
 count=0
 for name in sorted(os.listdir(history)):
  assert re.fullmatch('[a-f0-9]{64}',name)
  if os.path.exists(receipts+'/'+name):continue
  token=read_token(history+'/'+name)
  req=urllib.request.Request(p['address'].rstrip('/')+'/v1/auth/token/revoke-self',data=b'{}',headers={'X-Vault-Token':token,'Content-Type':'application/json'},method='POST')
  try:
   with opener.open(req,timeout=8) as response:
    assert 200<=response.status<300;response.read(1024)
  except urllib.error.HTTPError as error:
   # 403 may mean ACL denial OR an expired/revoked credential. It is never a
   # successful revoke receipt; response bodies/tokens must not be disclosed.
   code='TASK_TOKEN_REVOKE_FORBIDDEN_UNCONFIRMED' if error.code==403 else 'TASK_TOKEN_REVOKE_UNCERTAIN'
   print(json.dumps({'ok':False,'code':code}));sys.exit(0)
  except Exception:
   print('{"ok":false,"code":"TASK_TOKEN_REVOKE_UNCERTAIN"}');sys.exit(0)
  write(receipts+'/'+name,'acknowledged');count+=1
 write(d+'/revoked.json',json.dumps({'confirmed':True,'tokens':len(os.listdir(history))}))
 print(json.dumps({'ok':True,'revoked_tokens':len(os.listdir(history))}));sys.exit(0)
if p['action']=='clear':
 # Do not discard unknown outcomes. Legacy non-runtime unit delivery can clear
 # only when no finalization seal was set.
 if os.path.exists(d+'/sealed'):assert json.load(open(d+'/revoked.json'))['confirmed'] is True
 for name in os.listdir(history):os.unlink(history+'/'+name)
 try:os.unlink(target)
 except FileNotFoundError:pass
else:
 assert p['action']=='sync'
 assert not os.path.exists(d+'/sealed'),'task secrets sealed'
 token=p['token'];assert isinstance(token,str) and 0<len(token)<=16384 and all(32<ord(c)<127 for c in token)
 write(history+'/'+hashlib.sha256(token.encode()).hexdigest(),token)
 fd,tmp=tempfile.mkstemp(prefix='.token-',dir=d)
 try:
  with os.fdopen(fd,'w') as f:os.fchmod(f.fileno(),0o600);f.write(token);f.flush();os.fsync(f.fileno())
  os.replace(tmp,target)
 finally:
  if os.path.exists(tmp):os.unlink(tmp)
print('{"ok":true}')
