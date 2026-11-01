#!/usr/bin/env python3
"""Native Claude lifecycle observation only: no daemon, network or VM heartbeat.
Never reads transcript/tool_input/prompts; never treats this as authenticated life.
"""
import fcntl,hashlib,json,os,pathlib,sys,tempfile,time,uuid,stat

def state_root(home=None):
 # Claude removes CLAUDE_PLUGIN_DATA on marketplace removal. Ownership must outlive it.
 return pathlib.Path.home().joinpath('.local/state/nomad-workers/initiators') if home is None else pathlib.Path(home)/'.local/state/nomad-workers/initiators'

def archive_generation(root,d):
 folder=root/'generations';folder.mkdir(mode=0o700,exist_ok=True);st=folder.lstat()
 if not stat.S_ISDIR(st.st_mode) or st.st_uid!=os.getuid() or st.st_mode&0o077:raise ValueError('Unsafe generation archive')
 generation=str(uuid.UUID(d['generation']));agent=str(uuid.UUID(d['agent_instance_id']))
 evidence={k:d[k] for k in ('version','native_session_id','agent_instance_id','generation','started_at','evidence')}
 if 'start_source' in d:evidence['start_source']=d['start_source']
 p=folder/(hashlib.sha256(d['native_session_id'].encode()).hexdigest()+'-'+generation+'.json')
 try:
  fd=os.open(p,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
  with os.fdopen(fd,'w') as f:json.dump(evidence,f);f.flush();os.fsync(f.fileno())
 except FileExistsError:
  st=p.lstat()
  if not stat.S_ISREG(st.st_mode) or st.st_uid!=os.getuid() or st.st_mode&0o077 or json.loads(p.read_text())!=evidence:raise ValueError('Generation archive changed')


def record(root,event,now=None):
 event_name=event.get('hook_event_name');session=event.get('session_id')
 if event_name not in ('SessionStart','SessionEnd','PreToolUse'):raise ValueError('unsupported hook')
 if not isinstance(session,str) or not 1<=len(session)<=256 or any(c not in 'abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._:-' for c in session):raise ValueError('invalid native session id')
 root=pathlib.Path(root);root.mkdir(parents=True,exist_ok=True,mode=0o700)
 if root.is_symlink() or root.stat().st_uid!=os.getuid() or root.stat().st_mode&0o077:raise ValueError('private initiator root required')
 p=root/(hashlib.sha256(session.encode()).hexdigest()+'.json');now=time.time() if now is None else now
 with open(str(p)+'.lock','a') as lock:
  os.chmod(lock.name,0o600);fcntl.flock(lock,fcntl.LOCK_EX)
  old=None
  if p.exists() or p.is_symlink():
   st=p.lstat()
   if not stat.S_ISREG(st.st_mode) or st.st_uid!=os.getuid() or st.st_mode&0o077:raise ValueError('Unsafe initiator record')
   old=json.loads(p.read_text())
  if event_name=='SessionStart':
   if old and event.get('source')=='compact':d=old
   else:
    if old:archive_generation(root,old)
    d={'version':1,'native_session_id':session,'agent_instance_id':str(uuid.uuid4()),'generation':str(uuid.uuid4()),'started_at':now,'sequence':0,'evidence':'native_hook_observation_not_authenticated_liveness','start_source':event.get('source','unknown')}
    # Common native fields are optional. Do not infer a human name/model, read
    # transcripts, or change existing generation/identity on a later tool hook.
    for source_key,target_key in (('agent_type','native_agent_type'),('agent_id','native_agent_id')):
     value=event.get(source_key)
     if isinstance(value,str)and 1<=len(value)<=256 and not any(ord(c)<32 for c in value):d[target_key]=value
    archive_generation(root,d)
   d['ended']=False
  else:
   if old is None:return None,None
   d=old
  d.update(sequence=d['sequence']+1,observed_at=now,last_event=event_name)
  if event_name=='SessionEnd':d['ended']=True
  fd,tmp=tempfile.mkstemp(prefix='.initiator-',dir=root)
  try:
   with os.fdopen(fd,'w') as f:os.fchmod(f.fileno(),0o600);json.dump(d,f);f.flush();os.fsync(f.fileno())
   os.replace(tmp,p)
  finally:
   if os.path.exists(tmp):os.unlink(tmp)
 return str(p),d

def main():
 raw=sys.stdin.buffer.read(1048577)
 if len(raw)>1048576:return
 event=json.loads(raw);root=state_root()
 p,d=record(root,event)
 if d and event.get('hook_event_name')=='SessionStart':print(json.dumps({'hookSpecificOutput':{'hookEventName':'SessionStart','additionalContext':'Nomad originating-agent record: '+p+'. Use --creator-record with dynamic request; it is an observation, not authenticated liveness. Never renew VM leases from a background loop.'}}))
if __name__=='__main__':
 try:main()
 except Exception:print('Nomad initiator hook observation unavailable; VM lifecycle unchanged',file=sys.stderr)
