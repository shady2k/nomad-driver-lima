"""Shared finite guest command binding; no daemon or environment snapshots."""
def repository_bound_command(reference,argv):
 import os,sys,json,pathlib,hashlib,stat,re
 if not isinstance(reference,dict)or set(reference)!={'profile_sha256','environment_sha256','executor_sha256'}or any(not isinstance(v,str)or not re.fullmatch('[a-f0-9]{64}',v)for v in reference.values()):raise RuntimeError('REPOSITORY_EXECUTION_REFERENCE')
 path=pathlib.Path('/run/nomad-worker-repository-profiles')/reference['profile_sha256']/'executor.py'
 current=pathlib.Path('/')
 for part in path.parts[1:]:
  current=current/part
  if current.is_symlink():raise RuntimeError('REPOSITORY_EXECUTOR_SYMLINK')
 fd=os.open(path,os.O_RDONLY|os.O_NOFOLLOW)
 with os.fdopen(fd,'rb')as stream:
  st=os.fstat(stream.fileno())
  if not stat.S_ISREG(st.st_mode)or st.st_uid!=0 or st.st_mode&0o222 or st.st_size>131072:raise RuntimeError('REPOSITORY_EXECUTOR_UNSAFE')
  raw=stream.read(131073)
 if hashlib.sha256(raw).hexdigest()!=reference['executor_sha256']:raise RuntimeError('REPOSITORY_EXECUTOR_DIGEST_MISMATCH')
 return [sys.executable,str(path),'--run',json.dumps(reference,separators=(',',':')),'--',*argv]

def repository_command(argv):
 import os,json
 value=os.environ.get('NOMAD_WORKER_REPOSITORY_REFERENCE')
 return repository_bound_command(json.loads(value),argv)if value else argv
