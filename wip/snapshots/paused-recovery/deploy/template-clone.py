#!/usr/bin/env python3
"""Finite stopped-candidate stage, using official limactl clone without --start.
Run as the existing Lima runtime user from a trusted operator binding only.
No stop/delete/start, credential access, arbitrary shell, or rebuild fallback.
"""
import fcntl,hashlib,json,os,pathlib,re,subprocess,sys
sys.path.insert(0,str(pathlib.Path(__file__).parents[1]/'plugin/scripts'))
import template_save as save

def disk_identity(directory):
 result={}
 for name in ('disk','basedisk','diffdisk'):
  p=directory/name
  if p.is_symlink():raise RuntimeError('SOURCE_DISK_SYMLINK_REJECTED')
  if p.exists():
   if not p.is_file():raise RuntimeError('SOURCE_DISK_NOT_REGULAR')
   h=hashlib.sha256()
   with p.open('rb') as f:
    for b in iter(lambda:f.read(1048576),b''):h.update(b)
   result[name]=h.hexdigest()
 if not result:raise RuntimeError('SOURCE_DISK_UNAVAILABLE')
 return result

def clone(packet,receipts,home,limactl,run=subprocess.run):
 if packet['plan'].get('method')!='official_stopped_disk_clone_then_existing_seal_export_pipeline' or save.sha(packet['plan'])!=packet['plan_sha256']:raise RuntimeError('EXACT_DISK_CLONE_PLAN_REQUIRED')
 finish=save.verify_stage('source_finish',receipts['source_finish'],packet)
 source=finish.get('source_instance');candidate='agent-base-build-save-'+packet['plan']['operation_id'].replace('-','')
 if not isinstance(source,str) or not re.fullmatch('[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}',source) or source==candidate:raise RuntimeError('EXACT_SOURCE_INSTANCE_REQUIRED')
 home=pathlib.Path(home)
 if home.is_symlink() or home.stat().st_uid!=os.getuid() or home.stat().st_mode&0o077:raise RuntimeError('PRIVATE_RUNTIME_HOME_REQUIRED')
 src=home/source;dst=home/candidate
 if src.is_symlink() or not src.is_dir() or dst.exists() or dst.is_symlink():raise RuntimeError('CANDIDATE_EXISTS_OR_SOURCE_UNSAFE_NO_REPLAY')
 env={'HOME':os.path.expanduser('~'),'PATH':'/usr/local/bin:/usr/bin:/bin','LIMA_HOME':str(home)}
 def command(args):
  r=run([limactl,*args],env=env,text=True,capture_output=True,timeout=1800)
  if r.returncode:raise RuntimeError('LIMA_CLONE_STAGE_FAILED_OUTCOME_UNKNOWN')
  return r.stdout
 def stopped(name):
  rows=[json.loads(line) for line in command(['list','--json',name]).splitlines() if line.strip()]
  if len(rows)!=1 or rows[0].get('name')!=name or rows[0].get('status')!='Stopped' or pathlib.Path(rows[0].get('dir',''))!=home/name:raise RuntimeError('EXACT_STOPPED_INSTANCE_REQUIRED')
 stopped(source);before=disk_identity(src)
 if finish.get('source_disk_identity')!=before:raise RuntimeError('SOURCE_DISK_CHANGED_AFTER_FINISH')
 # Caller controller holds its operation lock; this additional lock serializes
 # operator clone attempts. Unknown outcomes must be reconciled, never replayed.
 fd=os.open(home/'.template-candidate.lock',os.O_CREAT|os.O_RDWR|os.O_NOFOLLOW,0o600)
 with os.fdopen(fd,'a') as lock:
  fcntl.flock(lock,fcntl.LOCK_EX)
  if dst.exists():raise RuntimeError('CANDIDATE_EXISTS_NO_REPLAY')
  stopped(source);command(['clone','--tty=false','--start=false','--set','.mounts=[] | .provision=[] | .probes=[] | .ssh.loadDotSSHPubKeys=false | .ssh.forwardAgent=false | .containerd.system=false | .containerd.user=false',source,candidate]);stopped(candidate);stopped(source)
  if disk_identity(src)!=before:raise RuntimeError('SOURCE_DISK_CHANGED_DATA_RETAINED')
 return {'operation_id':packet['plan']['operation_id'],'plan_sha256':packet['plan_sha256'],'source_instance':source,'candidate_instance':candidate,'source_disk_identity':before,'source_stopped_verified':True,'source_identity_bound':True,'candidate_distinct':True,'source_disk_unchanged':True,'clone_stopped':True,'no_rebuild':True}

if __name__=='__main__':
 try:
  payload=json.load(sys.stdin)
  print(json.dumps(clone(payload['packet'],payload['receipts'],sys.argv[1],sys.argv[2])))
 except Exception as e:
  # No subprocess stderr/argv/config values are copied to diagnostics.
  print(json.dumps({'error':str(e) if re.fullmatch('[A-Z_]{1,100}',str(e)) else type(e).__name__}),file=sys.stderr);sys.exit(1)
