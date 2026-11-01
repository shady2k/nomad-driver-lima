"""One-shot copied-guest preparation before any task token/workload. No model call."""
import hashlib,json,os,pathlib,re,shutil,stat,subprocess,sys,tempfile

def canonical(v):return json.dumps(v,sort_keys=True,separators=(',',':')).encode()
def atomic(path,value):
 fd,tmp=tempfile.mkstemp(dir=path.parent,prefix='.proof-')
 try:
  with os.fdopen(fd,'wb')as f:os.fchmod(f.fileno(),0o600);f.write(canonical(value));f.flush();os.fsync(f.fileno())
  os.replace(tmp,path);d=os.open(path.parent,os.O_RDONLY)
  try:os.fsync(d)
  finally:os.close(d)
 finally:
  if os.path.exists(tmp):os.unlink(tmp)
def prepare(payload,root=pathlib.Path('/'),run=subprocess.run):
 # root relocation is an internal test seam, never a payload field or CLI flag.
 if set(payload)!={'action','session_id','source_session','copy_sha256','manifest_sha256','source_key'}or payload['action']not in('clean','probe'):raise RuntimeError('PRIVATE_COPY_PREPARE_SCHEMA')
 for field in ('session_id','source_session'):
  if not re.fullmatch('[a-f0-9]{8}(?:-[a-f0-9]{4}){3}-[a-f0-9]{12}',payload[field]):raise RuntimeError('PRIVATE_COPY_SESSION_REQUIRED')
 if payload['session_id']==payload['source_session']:raise RuntimeError('NEW_PRIVATE_COPY_SESSION_REQUIRED')
 for field in('copy_sha256','manifest_sha256','source_key'):
  if not re.fullmatch('[a-f0-9]{64}',payload[field]):raise RuntimeError('PRIVATE_COPY_PIN_REQUIRED')
 def path(guest):
  p=root/guest.lstrip('/')
  for parent in [p.parent,*p.parent.parents]:
   if parent==root.parent:break
   if parent.is_symlink():raise RuntimeError('PRIVATE_COPY_CLEANUP_PARENT_SYMLINK')
   if parent==root:break
  return p
 def remove(guest):
  p=path(guest)
  if p.is_symlink()or p.is_file():p.unlink()
  elif p.exists():shutil.rmtree(p)
 folder=path('/var/lib/nomad-worker-recovery');marker=folder/(payload['session_id']+'.json')
 if folder.is_symlink():raise RuntimeError('PRIVATE_COPY_PROOF_DIRECTORY_UNSAFE')
 expected={k:v for k,v in payload.items()if k!='action'}
 if marker.exists()or marker.is_symlink():
  st=marker.lstat()
  if not stat.S_ISREG(st.st_mode)or stat.S_IMODE(st.st_mode)!=0o600 or st.st_uid!=os.getuid():raise RuntimeError('PRIVATE_COPY_PREPARATION_PROOF_UNSAFE')
  proof=json.loads(marker.read_text())
  if proof.get('binding')!=expected or proof.get('known_runtime_credentials_removed')is not True or proof.get('identity_reset_pending')is not True or proof.get('reserved_runtime_root_removed')is not True:raise RuntimeError('PRIVATE_COPY_PREPARATION_PROOF_CHANGED')
  return proof
 if payload['action']=='probe':raise RuntimeError('PRIVATE_COPY_PREPARATION_UNCONFIRMED_NO_REPLAY')
 for name in ('prime-agent','bao','acpx'):
  p=run(['pgrep','-x',name],capture_output=True,text=True)
  if p.returncode!=1:raise RuntimeError('PRIVATE_COPY_UNEXPECTED_RUNTIME_PROCESS')
 mounts=path('/proc/mounts').read_text().splitlines()
 if not any(len(row.split())>=3 and row.split()[1:3]==['/run','tmpfs']for row in mounts):raise RuntimeError('PRIVATE_COPY_RUNTIME_TMPFS_REQUIRED')
 before={'machine_id':path('/etc/machine-id').read_text().strip(),'boot_id':path('/proc/sys/kernel/random/boot_id').read_text().strip()}
 if not before['machine_id']or not before['boot_id']:raise RuntimeError('PRIVATE_COPY_INITIAL_IDENTITY_REQUIRED')
 # Only old application runtime paths. Workspace/git/tracker/package state untouched.
 # This entire path is reserved runtime state, not a project workspace.
 reserved=path('/var/tmp/nomad-workers');workspace=path('/workspace/repo')
 # Resolve existing parents before deletion; never follow a runtime-root symlink.
 if reserved.is_symlink()or workspace.is_symlink():raise RuntimeError('PRIVATE_COPY_RUNTIME_WORKSPACE_SYMLINK')
 a=reserved.resolve();b=workspace.resolve()
 if a==b or a in b.parents or b in a.parents:raise RuntimeError('PRIVATE_COPY_RUNTIME_WORKSPACE_OVERLAP')
 remove('/var/tmp/nomad-workers')
 homes=[path('/root')];base=path('/home')
 if base.exists():
  if base.is_symlink():raise RuntimeError('PRIVATE_COPY_HOME_SYMLINK')
  homes.extend(p for p in base.iterdir()if p.is_dir()or p.is_symlink())
 for home in homes:
  for name in ('.prime','.acpx','.bao-token','.vault-token','.config/prime'):
   remove('/'+str((home/name).relative_to(root)))
 for p in path('/etc/ssh').glob('ssh_host_*_key*'):
  if p.is_symlink()or p.is_file():p.unlink()
 for args in (['cloud-init','clean','--logs','--machine-id','--seed'],['systemctl','stop','systemd-random-seed.service']):
  if run(args,capture_output=True,text=True).returncode:raise RuntimeError('PRIVATE_COPY_IDENTITY_RESET_FAILED')
 remove('/var/lib/systemd/random-seed')
 if path('/etc/machine-id').read_text().strip()!='uninitialized':raise RuntimeError('PRIVATE_COPY_MACHINE_ID_NOT_SEALED')
 folder.mkdir(mode=0o700,parents=True,exist_ok=True)
 if folder.is_symlink()or folder.stat().st_mode&0o077:raise RuntimeError('PRIVATE_COPY_PROOF_DIRECTORY_UNSAFE')
 proof={'binding':expected,**before,'known_runtime_credentials_removed':True,'reserved_runtime_root_removed':True,'workspace_preserved':True,'identity_reset_pending':True}
 atomic(marker,proof)
 os.sync();return proof
if __name__=='__main__':
 try:print(json.dumps(prepare(json.load(sys.stdin))))
 except Exception as e:
  message=str(e);code=message if re.fullmatch('[A-Z_]{1,100}',message)else'PRIVATE_COPY_PREPARATION_FAILED'
  print(json.dumps({'error':code}));sys.exit(1)
