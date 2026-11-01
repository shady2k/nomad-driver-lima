"""Report-only originating-session lease. Never a background heartbeat/reaper.
A CLI observation is not authenticated proof that the original agent is alive.
"""
import contextlib,fcntl,json,os,pathlib,tempfile,time,uuid
@contextlib.contextmanager
def locked(root):
 root=pathlib.Path(root)
 with open(root/'lease.lock','a') as f:os.chmod(f.name,0o600);fcntl.flock(f,fcntl.LOCK_EX);yield root/'lease.json'
def write(path,doc):
 fd,tmp=tempfile.mkstemp(prefix='.lease-',dir=path.parent)
 try:
  with os.fdopen(fd,'w') as f:os.fchmod(f.fileno(),0o600);json.dump(doc,f);f.flush();os.fsync(f.fileno())
  os.replace(tmp,path)
 finally:
  if os.path.exists(tmp):os.unlink(tmp)
def create(root,owner,session,attempt,now=None):
 now=time.time() if now is None else now
 with locked(root) as p:
  if p.exists():
   d=json.loads(p.read_text())
   if (d['owner'],d['session'],d['attempt'])!=(owner,session,attempt):raise RuntimeError('LEASE_OWNER_ATTEMPT_MISMATCH')
   return d
  d={'owner':owner,'session':session,'attempt':attempt,'generation':str(uuid.uuid4()),'sequence':0,'observed_at':now,'expires_at':now+300,'last_activity_at':now,'evidence':'plugin_invocation_not_authenticated_agent_liveness','auto_close_enabled':False,'auto_delete_enabled':False};write(p,d);return d
def observe(root,owner,session,attempt,generation,sequence,event_id,now=None):
 now=time.time() if now is None else now
 with locked(root) as p:
  d=json.loads(p.read_text())
  if (d['owner'],d['session'],d['attempt'],d['generation'])!=(owner,session,attempt,generation):raise RuntimeError('LEASE_OWNER_GENERATION_MISMATCH')
  # Replayed event/sequence cannot extend expiry; old generations never reclaim.
  if d.get('event_id')==event_id:return d
  if sequence!=d['sequence']+1:raise RuntimeError('LEASE_SEQUENCE_MISMATCH')
  if now<d['observed_at']:raise RuntimeError('LEASE_CLOCK_UNCERTAIN')
  d.update(sequence=sequence,event_id=event_id,observed_at=now,expires_at=now+300,last_activity_at=now);write(p,d);return d

def report(root,active=False,unknown=False,now=None):
 now=time.time() if now is None else now
 with locked(root) as p:d=json.loads(p.read_text())
 expiry=max(0,now-d['expires_at']);idle=max(0,now-d['last_activity_at'])
 return {'mode':'report-only','owner_state':'owner_unconfirmed','lease_expired':expiry>0,'orphan_suspect':expiry>0,'orphan_grace_elapsed':expiry>=1800,'idle_state':'unknown' if unknown else ('active' if active else ('idle_grace_elapsed' if idle>=2400 else ('idle_warning' if idle>=1800 else 'recent'))),'active_work':active,'unknown_outcome':unknown,'generation':d['generation'],'sequence':d['sequence'],'auto_close_enabled':False,'auto_delete_enabled':False,'reason':'lease observation is not authenticated agent liveness; no automatic cleanup'}
